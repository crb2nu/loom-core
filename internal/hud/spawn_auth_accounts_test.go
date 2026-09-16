package hud

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

func TestParseClaudeAccounts(t *testing.T) {
	got := parseClaudeAccounts(" claude-oauth-token=tue , claude-oauth-token-2=FRI@17 ,bad key=mon,claude-oauth-token-3=xyz,claude-oauth-token=sat")
	if len(got) != 3 {
		t.Fatalf("accounts = %+v, want 3", got)
	}
	if got[0].Key != "claude-oauth-token" || !got[0].HasReset || got[0].ResetWeekday != time.Tuesday || got[0].ResetHour != 0 {
		t.Errorf("first = %+v, want claude-oauth-token tue@0", got[0])
	}
	if got[1].Key != "claude-oauth-token-2" || !got[1].HasReset || got[1].ResetWeekday != time.Friday || got[1].ResetHour != 17 {
		t.Errorf("second = %+v, want claude-oauth-token-2 fri@17", got[1])
	}
	if got[2].Key != "claude-oauth-token-3" || got[2].HasReset {
		t.Errorf("third = %+v, want claude-oauth-token-3 with the malformed annotation dropped", got[2])
	}
	if def := parseClaudeAccounts("   "); len(def) != 1 || def[0].Key != defaultClaudeOAuthTokenKey || def[0].HasReset {
		t.Errorf("blank = %+v, want the single default key", def)
	}
	if _, _, ok := parseClaudeReset("fri@24"); ok {
		t.Error("hour 24 must be rejected")
	}
}

func TestClaudeAccountWindowStart(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) // Thursday
	tue := claudeAccount{Key: "a", ResetWeekday: time.Tuesday, HasReset: true}
	if got := tue.windowStart(now); !got.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("tue window start = %s, want 2026-09-15T00:00Z", got)
	}
	fri := claudeAccount{Key: "b", ResetWeekday: time.Friday, ResetHour: 17, HasReset: true}
	if got := fri.windowStart(now); !got.Equal(time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("fri@17 window start = %s, want 2026-09-11T17:00Z", got)
	}
	// Same weekday, before the reset hour: the window began last week.
	thu := claudeAccount{Key: "c", ResetWeekday: time.Thursday, ResetHour: 18, HasReset: true}
	if got := thu.windowStart(now); !got.Equal(time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("thu@18 window start = %s, want 2026-09-10T18:00Z", got)
	}
	rolling := claudeAccount{Key: "d"}
	if got := rolling.windowStart(now); !got.Equal(now.Add(-claudeWeeklyWindow)) {
		t.Errorf("unannotated window start = %s, want rolling 7d", got)
	}
}

func claudeSpawn(account string, startedAt time.Time, usd float64, agentType string) *spawn.State {
	return &spawn.State{
		AuthAccount: account,
		StartedAt:   startedAt,
		Request:     spawn.Request{AgentType: agentType},
		Telemetry:   &bridge.SpawnTelemetry{TotalCostUSD: usd},
	}
}

// TestPickClaudeAccount_PacesAgainstResetWindows pins the balancing contract:
// the next spawn goes to the account furthest behind its own weekly pace,
// usage before an account's reset does not count against it, other vendors
// and unattributed spawns are ignored, and level accounts round-robin.
func TestPickClaudeAccount_PacesAgainstResetWindows(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) // Thursday noon
	accounts := parseClaudeAccounts("claude-oauth-token=tue,claude-oauth-token-2=fri")
	orig, second := "claude-oauth-token", "claude-oauth-token-2"

	spawns := []*spawn.State{
		claudeSpawn(orig, time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC), 500, "claude-code"),  // Monday: before Tuesday's reset, ignored
		claudeSpawn(orig, time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), 30, "claude-code"),   // Wed: counts (window from Tue 00:00, 2.5d elapsed)
		claudeSpawn(second, time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC), 50, "claude-code"), // Sun: counts (window from Fri 00:00, 6.5d elapsed)
		claudeSpawn(second, time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), 10, "codex"),       // other vendor, ignored
		claudeSpawn("", time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), 999, "claude-code"),    // unattributed, ignored
	}
	key, usages := pickClaudeAccount(accounts, spawns, now)
	// orig: 30 / (2.5/7) = 84 projected; second: 60... no: 50 / (6.5/7) ≈ 53.8 projected → second is behind pace.
	if key != second {
		t.Fatalf("picked %q, want %q (usages %+v)", key, second, usages)
	}
	for _, u := range usages {
		switch u.Key {
		case orig:
			if u.UsedUSD != 30 || u.Spawns != 1 {
				t.Errorf("orig usage = %+v, want $30 over 1 spawn (Monday spawn predates the Tuesday reset)", u)
			}
		case second:
			if u.UsedUSD != 50 || u.Spawns != 1 {
				t.Errorf("second usage = %+v, want $50 over 1 spawn", u)
			}
		}
	}

	// Pile spend onto the second account: its projection overtakes and the
	// original is preferred again.
	spawns = append(spawns, claudeSpawn(second, time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC), 60, "claude-code"))
	if key, _ = pickClaudeAccount(accounts, spawns, now); key != orig {
		t.Fatalf("after extra spend picked %q, want %q", key, orig)
	}

	// Idle pool: nothing distinguishes the accounts, so they alternate.
	claudeOAuthKeyCursor.Store(0)
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		k, _ := pickClaudeAccount(accounts, nil, now)
		seen[k]++
	}
	if seen[orig] != 2 || seen[second] != 2 {
		t.Fatalf("idle pool did not round-robin: %v", seen)
	}

	// A single account is always itself.
	if k, _ := pickClaudeAccount(parseClaudeAccounts("claude-oauth-token=tue"), spawns, now); k != orig {
		t.Fatalf("single account = %q", k)
	}
}

func TestPickAuthAccount_FallsBackWithoutLedger(t *testing.T) {
	t.Setenv(ClaudeOAuthTokenKeysEnv, "claude-oauth-token=tue,claude-oauth-token-2=fri")
	var o *SpawnOrchestrator // no controller → round-robin path, never panics
	claudeOAuthKeyCursor.Store(0)
	if got := o.pickAuthAccount("claude-code"); got != "claude-oauth-token" {
		t.Fatalf("first pick without a ledger = %q, want claude-oauth-token", got)
	}
	if got := o.pickAuthAccount("claude-code"); got != "claude-oauth-token-2" {
		t.Fatalf("second pick without a ledger = %q, want claude-oauth-token-2", got)
	}
	if got := o.pickAuthAccount("codex"); got != "" {
		t.Fatalf("codex must have no account, got %q", got)
	}
}
