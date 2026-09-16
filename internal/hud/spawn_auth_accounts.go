package hud

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/spawn"
)

// claudeAccount is one pooled Claude subscription: the cluster-agent-auth key
// holding its `claude setup-token` and, when known, the weekday and hour (UTC)
// at which its weekly usage window resets. Configured through
// SPAWN_CLAUDE_OAUTH_TOKEN_KEYS as `key[=weekday[@HH]]`, for example
// "claude-oauth-token=tue,claude-oauth-token-2=fri@17". A key without an
// annotation still pools but is paced on a rolling seven-day window.
type claudeAccount struct {
	Key          string
	ResetWeekday time.Weekday
	ResetHour    int
	HasReset     bool
}

// claudeWeeklyWindow is the vendor's weekly usage window.
const claudeWeeklyWindow = 7 * 24 * time.Hour

// claudeWindowPaceFloor is the least elapsed time used when projecting pace,
// so an account that reset minutes ago is not divided by ~zero.
const claudeWindowPaceFloor = time.Hour

// claudePaceTieRatio: two accounts whose projected window spend differs by
// less than this fraction are "level" and fall back to round-robin.
const claudePaceTieRatio = 0.02

var claudeWeekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseClaudeAccounts parses a SPAWN_CLAUDE_OAUTH_TOKEN_KEYS value. Keys are
// deduplicated in order; a key that is not in secret-key syntax is dropped;
// a malformed reset annotation drops only the annotation. Never empty: an
// unset or fully malformed value yields the single default key.
func parseClaudeAccounts(raw string) []claudeAccount {
	seen := map[string]bool{}
	var out []claudeAccount
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, annotation, _ := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if key == "" || seen[key] || !secretKeyPattern.MatchString(key) {
			continue
		}
		seen[key] = true
		acct := claudeAccount{Key: key}
		if day, hour, ok := parseClaudeReset(annotation); ok {
			acct.ResetWeekday, acct.ResetHour, acct.HasReset = day, hour, true
		}
		out = append(out, acct)
	}
	if len(out) == 0 {
		return []claudeAccount{{Key: defaultClaudeOAuthTokenKey}}
	}
	return out
}

// parseClaudeReset parses `weekday[@HH]` (UTC). Weekday names are the
// three-letter English abbreviations, case-insensitive; HH is 0-23.
func parseClaudeReset(annotation string) (time.Weekday, int, bool) {
	annotation = strings.ToLower(strings.TrimSpace(annotation))
	if annotation == "" {
		return 0, 0, false
	}
	dayName, hourText, hasHour := strings.Cut(annotation, "@")
	day, ok := claudeWeekdayNames[strings.TrimSpace(dayName)]
	if !ok {
		return 0, 0, false
	}
	hour := 0
	if hasHour {
		h, err := strconv.Atoi(strings.TrimSpace(hourText))
		if err != nil || h < 0 || h > 23 {
			return 0, 0, false
		}
		hour = h
	}
	return day, hour, true
}

// claudeAccounts reads the configured pool.
func claudeAccounts() []claudeAccount {
	return parseClaudeAccounts(os.Getenv(ClaudeOAuthTokenKeysEnv))
}

// windowStart returns when the account's current weekly window began: the
// most recent reset instant at or before now for an annotated account, else
// a rolling seven days.
func (a claudeAccount) windowStart(now time.Time) time.Time {
	now = now.UTC()
	if !a.HasReset {
		return now.Add(-claudeWeeklyWindow)
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), a.ResetHour, 0, 0, 0, time.UTC)
	daysBack := (int(now.Weekday()) - int(a.ResetWeekday) + 7) % 7
	start := day.AddDate(0, 0, -daysBack)
	if start.After(now) {
		start = start.AddDate(0, 0, -7)
	}
	return start
}

// claudeAccountUsage is one account's position in its current weekly window.
// PaceUSD projects what the window would cost at the current rate; the
// selector sends the next spawn to the account with the lowest projection,
// which evens spend out across accounts whose windows reset on different
// days instead of exhausting one while the other idles toward its reset.
type claudeAccountUsage struct {
	Key         string    `json:"key"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Elapsed     float64   `json:"elapsed_fraction"`
	UsedUSD     float64   `json:"used_usd"`
	PaceUSD     float64   `json:"pace_usd_per_window"`
	Spawns      int       `json:"spawns"`
}

// claudeAccountUsages sums each account's claude-code spawn spend (the vendor
// list-price equivalent from spawn telemetry, the same figure Mills bills as
// subscription cost) since its window started. Spawns that predate the
// window, ran another vendor, or carry no account are ignored.
func claudeAccountUsages(accounts []claudeAccount, spawns []*spawn.State, now time.Time) []claudeAccountUsage {
	out := make([]claudeAccountUsage, 0, len(accounts))
	for _, a := range accounts {
		start := a.windowStart(now)
		u := claudeAccountUsage{Key: a.Key, WindowStart: start, WindowEnd: start.Add(claudeWeeklyWindow)}
		for _, s := range spawns {
			if s == nil || s.AuthAccount != a.Key || s.Request.AgentType != "claude-code" || s.StartedAt.Before(start) {
				continue
			}
			u.Spawns++
			if s.Telemetry != nil {
				u.UsedUSD += s.Telemetry.TotalCostUSD
			}
		}
		elapsed := now.UTC().Sub(start)
		if elapsed < claudeWindowPaceFloor {
			elapsed = claudeWindowPaceFloor
		}
		u.Elapsed = elapsed.Seconds() / claudeWeeklyWindow.Seconds()
		u.PaceUSD = u.UsedUSD / u.Elapsed
		out = append(out, u)
	}
	return out
}

// pickClaudeAccount chooses the account furthest behind its weekly pace.
// Level accounts (all projections within claudePaceTieRatio of the lowest,
// including the all-idle case) fall back to round-robin so the pool still
// alternates when nothing distinguishes them.
func pickClaudeAccount(accounts []claudeAccount, spawns []*spawn.State, now time.Time) (string, []claudeAccountUsage) {
	usages := claudeAccountUsages(accounts, spawns, now)
	if len(usages) == 0 {
		return defaultClaudeOAuthTokenKey, nil
	}
	if len(usages) == 1 {
		return usages[0].Key, usages
	}
	best := 0
	for i := range usages {
		if usages[i].PaceUSD < usages[best].PaceUSD {
			best = i
		}
	}
	lowest := usages[best].PaceUSD
	level := true
	for _, u := range usages {
		if u.PaceUSD-lowest > claudePaceTieRatio*lowest || (lowest == 0 && u.PaceUSD > 0) {
			level = false
			break
		}
	}
	if level {
		n := claudeOAuthKeyCursor.Add(1) - 1
		return usages[n%uint64(len(usages))].Key, usages
	}
	return usages[best].Key, usages
}

// pickAuthAccount is nextAuthAccount informed by the HUD's own spawn ledger:
// with two or more pooled Claude accounts it sends the spawn to the one
// furthest behind its weekly pace (see claudeAccountUsage). Vendors without a
// pool, a single key, and an unavailable ledger keep the round-robin path.
func (o *SpawnOrchestrator) pickAuthAccount(agentType string) string {
	if agentType != "claude-code" {
		return ""
	}
	accounts := claudeAccounts()
	if o == nil || o.ctrl == nil {
		return nextAuthAccount(agentType)
	}
	now := time.Now()
	accounts = claudeHealthyAccounts(accounts, o.ctrl.List(), now, nil)
	if len(accounts) == 0 {
		return ""
	}
	key, usages := pickClaudeAccount(accounts, o.ctrl.List(), now)
	if o.logger != nil {
		attrs := []any{"account", key}
		for _, u := range usages {
			attrs = append(attrs, "pace["+u.Key+"]", strconv.FormatFloat(u.PaceUSD, 'f', 2, 64),
				"used["+u.Key+"]", strconv.FormatFloat(u.UsedUSD, 'f', 2, 64),
				"reset["+u.Key+"]", u.WindowEnd.Format(time.RFC3339))
		}
		o.logger.Info("claude account selected by weekly pace", attrs...)
	}
	return key
}
