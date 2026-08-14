package mills

import (
	"strings"
	"testing"
	"time"
)

// Zero value = fully off, and every accessor resolves a safe default.
func TestOverseersZeroValueIsOff(t *testing.T) {
	var p Policy
	if p.GroomerEnabled() || p.SentinelEnabled() || p.ForemanEnabled() {
		t.Fatal("zero-value overseers enabled an agent")
	}
	// Master gate off keeps agents off even when individually enabled.
	p.Overseers.Groomer.Enabled = true
	if p.GroomerEnabled() {
		t.Fatal("groomer enabled without the master gate")
	}
	p.Overseers.Enabled = true
	if !p.GroomerEnabled() {
		t.Fatal("groomer not enabled with both gates")
	}
}

func TestOverseersDryRunDefaultsOn(t *testing.T) {
	var g GroomerPolicy
	if !DryRunOn(g.DryRun) {
		t.Fatal("nil dry_run must mean dry-run ON")
	}
	off := false
	g.DryRun = &off
	if DryRunOn(g.DryRun) {
		t.Fatal("explicit dry_run:false ignored")
	}
}

func TestOverseersAccessorDefaultsAndClamps(t *testing.T) {
	var g GroomerPolicy
	if got := g.Interval(); got != 60*time.Minute {
		t.Fatalf("groomer interval default = %v", got)
	}
	if got := g.TickCap(); got != 5 {
		t.Fatalf("tick cap default = %d", got)
	}
	if got := g.DayCap(); got != 20 {
		t.Fatalf("day cap default = %d", got)
	}
	if got := g.DedupThreshold(); got != 0.85 {
		t.Fatalf("dedup threshold default = %v", got)
	}
	if got := g.ZombieAge(); got != 14*24*time.Hour {
		t.Fatalf("zombie age default = %v", got)
	}

	g = GroomerPolicy{MaxActionsPerTick: 999, MaxActionsPerDay: 9999, IntervalMinutes: 999999}
	if got := g.TickCap(); got != 20 {
		t.Fatalf("tick cap clamp = %d, want 20", got)
	}
	if got := g.DayCap(); got != 100 {
		t.Fatalf("day cap clamp = %d, want 100", got)
	}
	if got := g.Interval(); got != 24*time.Hour {
		t.Fatalf("interval clamp = %v, want 24h", got)
	}

	var s SentinelPolicy
	if got := s.Interval(); got != 5*time.Minute {
		t.Fatalf("sentinel interval default = %v", got)
	}
	if got := s.TripThreshold(); got != 3 {
		t.Fatalf("trips default = %d", got)
	}
	if got := s.SuppressionTTL(); got != 30*time.Minute {
		t.Fatalf("suppression ttl default = %v", got)
	}

	var f ForemanPolicy
	if got := f.Interval(); got != 15*time.Minute {
		t.Fatalf("foreman interval default = %v", got)
	}
	if got := f.StuckRunAge(); got != 4*time.Hour {
		t.Fatalf("stuck run default = %v", got)
	}
	if got := f.BurnRatio(); got != 0.9 {
		t.Fatalf("burn ratio default = %v", got)
	}
}

func TestOverseersValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Policy)
		wantErr string
	}{
		{
			name:   "valid section",
			mutate: func(p *Policy) { p.Overseers = OverseersPolicy{Enabled: true, Groomer: GroomerPolicy{Enabled: true}} },
		},
		{
			name:    "dedup threshold inside gray band",
			mutate:  func(p *Policy) { p.Overseers.Groomer.DedupAutoThreshold = 0.5 },
			wantErr: "dedup_auto_threshold",
		},
		{
			name:    "dedup threshold above 1",
			mutate:  func(p *Policy) { p.Overseers.Groomer.DedupAutoThreshold = 1.5 },
			wantErr: "dedup_auto_threshold",
		},
		{
			name:    "negative cap",
			mutate:  func(p *Policy) { p.Overseers.Groomer.MaxActionsPerTick = -1 },
			wantErr: "max_actions_per_tick",
		},
		{
			name:    "burn ratio out of range",
			mutate:  func(p *Policy) { p.Overseers.Foreman.BudgetBurnRatio = 1.5 },
			wantErr: "budget_burn_ratio",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			tc.mutate(p)
			err := p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// An overseers YAML block round-trips through ParsePolicy.
func TestOverseersYAMLRoundTrip(t *testing.T) {
	yaml := `
version: 2
enabled: true
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
overseers:
  enabled: true
  groomer:
    enabled: true
    dry_run: false
    interval_minutes: 30
    max_actions_per_tick: 3
    dedup_auto_threshold: 0.9
    allow: { dedup_close: true, reprioritize: true }
`
	p, err := ParsePolicy([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.GroomerEnabled() {
		t.Fatal("groomer not enabled")
	}
	if DryRunOn(p.Overseers.Groomer.DryRun) {
		t.Fatal("dry_run:false not honored")
	}
	if p.Overseers.Groomer.Interval() != 30*time.Minute {
		t.Fatalf("interval = %v", p.Overseers.Groomer.Interval())
	}
	if !p.Overseers.Groomer.Allow.DedupClose || !p.Overseers.Groomer.Allow.Reprioritize {
		t.Fatal("allow flags not parsed")
	}
	if p.Overseers.Groomer.Allow.CloseObsolete {
		t.Fatal("close_obsolete defaulted on")
	}
}
