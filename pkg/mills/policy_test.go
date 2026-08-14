package mills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureV1 = `
version: 1
budgets:
  council:
    max_usd_per_run: 15
    max_usd_per_day: 50
  pipeline:
    max_usd_per_run: 5
    max_usd_per_day: 75
    max_concurrent_runs: 4
    max_runs_per_day: 20
council:
  schedule_cron: "0 5 * * *"
  triggers:
    on_roadmap_change: true
    on_incident: true
    on_merge_drift_hours: 48
  ensemble:
    editor: { model: claude-opus-4-7, backend: claude-code }
    reviewers:
      - { name: security,    model: gpt-5-codex,       backend: codex }
      - { name: tech-debt,   model: qwen3.5-9b,        backend: flexinfer }
      - { name: user-impact, model: claude-sonnet-4-6, backend: claude-code }
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
pipeline:
  default_template: mills-default-pipeline
  per_label_overrides:
    - { label: docs,     auto_merge: true,  human_review: false }
    - { label: debt,     auto_merge: true,  human_review: false }
    - { label: security, auto_merge: false, human_review: true  }
  protected_paths:
    - "platform/gitops/**"
    - "cmd/loomd/**"
    - "**/*auth*.go"
  retry:
    max_attempts: 3
    cooldown_seconds: 300
human_handoff:
  on_escalation_create_handoff: true
  on_escalation_create_issue:   true
  notify_agent_id: "claude-code"
`

func TestParsePolicy_Valid(t *testing.T) {
	p, err := ParsePolicy([]byte(fixtureV1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Version != 1 {
		t.Errorf("version: %d", p.Version)
	}
	if !p.IsEnabled() {
		t.Errorf("expected default-enabled when 'enabled' is omitted")
	}
	if p.Budgets.Pipeline.MaxConcurrentRuns != 4 {
		t.Errorf("concurrent: %d", p.Budgets.Pipeline.MaxConcurrentRuns)
	}
	if len(p.Council.Ensemble.Reviewers) != 3 {
		t.Errorf("reviewers: %d", len(p.Council.Ensemble.Reviewers))
	}
	if p.Pipeline.Retry.CooldownDuration() != 5*time.Minute {
		t.Errorf("cooldown: %v", p.Pipeline.Retry.CooldownDuration())
	}
}

func TestPolicy_Validate_Errors(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Policy)
		want  string
	}{
		{
			name:  "version mismatch",
			patch: func(p *Policy) { p.Version = 99 },
			want:  "unsupported policy version",
		},
		{
			name:  "negative budget",
			patch: func(p *Policy) { p.Budgets.Council.MaxUSDPerRun = -1 },
			want:  "max_usd_per_run must be >= 0",
		},
		{
			name:  "per-run > per-day",
			patch: func(p *Policy) { p.Budgets.Pipeline.MaxUSDPerRun = 200 },
			want:  "exceeds max_usd_per_day",
		},
		{
			name:  "bad merge strategy",
			patch: func(p *Policy) { p.Council.ArtifactsMergeStrategy = "yolo" },
			want:  "must be 'fast-merge-loom-only' or 'always-mr'",
		},
		{
			name:  "unlabeled override",
			patch: func(p *Policy) { p.Pipeline.PerLabelOverrides[0].Label = "" },
			want:  "label is empty",
		},
		{
			name:  "reviewer missing backend",
			patch: func(p *Policy) { p.Council.Ensemble.Reviewers[0].Backend = "" },
			want:  "requires both model and backend",
		},
		{
			name:  "negative merged-work lookback",
			patch: func(p *Policy) { p.Council.Dedup.MergedWork.LookbackHours = -1 },
			want:  "lookback_hours must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(fixtureV1))
			if err != nil {
				t.Fatalf("setup parse: %v", err)
			}
			tc.patch(p)
			err = p.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPolicy_LabelOverrideFor(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))

	if ov, ok := p.LabelOverrideFor([]string{"docs", "frontend"}); !ok || !ov.AutoMerge {
		t.Errorf("docs auto_merge: ov=%+v ok=%v", ov, ok)
	}
	if ov, ok := p.LabelOverrideFor([]string{"security"}); !ok || ov.AutoMerge || !ov.HumanReview {
		t.Errorf("security override: %+v ok=%v", ov, ok)
	}
	if _, ok := p.LabelOverrideFor([]string{"random-label"}); ok {
		t.Errorf("expected no match for random-label")
	}
	// Mixed-case label still matches.
	if _, ok := p.LabelOverrideFor([]string{"  Debt  "}); !ok {
		t.Errorf("normalised lookup failed")
	}
}

func TestPolicy_ProtectedPathsHit(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	hits := p.ProtectedPathsHit([]string{
		"platform/gitops/k3s/foo.yaml",
		"internal/hud/spawn.go",
		"cmd/loomd/main.go",
		"pkg/auth/middleware_auth.go",
	})
	want := map[string]bool{
		"platform/gitops/k3s/foo.yaml": true,
		"cmd/loomd/main.go":            true,
		"pkg/auth/middleware_auth.go":  true, // matches **/*auth*.go
	}
	if len(hits) != len(want) {
		t.Errorf("hits: got %v want %v", hits, want)
	}
	for _, h := range hits {
		if !want[h] {
			t.Errorf("unexpected hit %q", h)
		}
	}
}

func TestPolicy_KillSwitch(t *testing.T) {
	p, err := ParsePolicy([]byte(`version: 1
enabled: false
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry: { max_attempts: 1, cooldown_seconds: 0 }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.IsEnabled() {
		t.Errorf("explicit enabled:false should disable")
	}
}

// TestPolicy_V1ForwardCompat proves a v1-shaped YAML (no version field,
// no v2 sections) still parses on a v2 binary, and that v2-only helpers
// report safe defaults so the operator doesn't accidentally turn on a
// feature the operator never opted into.
func TestPolicy_V1ForwardCompat(t *testing.T) {
	v1Body := `
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
`
	p, err := ParsePolicy([]byte(v1Body))
	if err != nil {
		t.Fatalf("v1 forward-compat parse: %v", err)
	}
	if !p.IsEnabled() {
		t.Errorf("default enabled when omitted")
	}
	if p.SquadsEnabled() {
		t.Errorf("squads must default off for v1 YAML")
	}
	if p.AuditEnabled() {
		t.Errorf("audit must default off when YAML omits the section")
	}
	if !p.AuditAdvisoryOnly() {
		t.Errorf("audit advisory_only must default true (fail-safe)")
	}
}

// TestPolicy_V2Roundtrip proves a v2-shaped YAML parses, validates, and
// the helpers return the YAML's values verbatim.
func TestPolicy_V2Roundtrip(t *testing.T) {
	v2Body := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
squads:
  enabled: true
  routing:
    min_confidence: 0.7
    fallback: _default
audit:
  enabled: true
  advisory_only: false
  survival_threshold: 0.85
  daily_budget_usd: 12.0
  pool_default:
    - { backend: flexinfer, model: llama-4-70b-instruct }
    - { backend: flexinfer, model: qwen-3-32b }
cross_repo:
  enabled: true
  per_repo_timeout_minutes: 60
  revert_strategy: all_or_revert
  demand_projects:
    - services/flexdeck
    - "services/loom-flightdeck"
debate:
  enabled:
    cron: false
    incident: true
    manual: true
  max_usd: 8.0
  max_rounds: 3
recursion:
  enabled: true
  max_depth: 2
  subrun_max_budget_share: 0.6
adaptive_policy:
  enabled: true
  auto_apply: false
  relax_path_denylist:
    - "platform/gitops/**"
  revert_window_hours: 24
`
	p, err := ParsePolicy([]byte(v2Body))
	if err != nil {
		t.Fatalf("v2 parse: %v", err)
	}
	if p.Version != 2 {
		t.Errorf("version: got %d want 2", p.Version)
	}
	if !p.SquadsEnabled() {
		t.Errorf("squads.enabled true must surface via helper")
	}
	if p.Squads.Routing.MinConfidence != 0.7 {
		t.Errorf("routing.min_confidence: got %v want 0.7", p.Squads.Routing.MinConfidence)
	}
	if !p.AuditEnabled() {
		t.Errorf("audit.enabled true must surface via helper")
	}
	if p.AuditAdvisoryOnly() {
		t.Errorf("audit.advisory_only false must surface via helper")
	}
	if p.Audit.SurvivalThreshold != 0.85 {
		t.Errorf("survival_threshold: got %v want 0.85", p.Audit.SurvivalThreshold)
	}
	if len(p.Audit.PoolDefault) != 2 {
		t.Errorf("pool_default: got %d members want 2", len(p.Audit.PoolDefault))
	}
	if !p.CrossRepo.Enabled || p.CrossRepo.PerRepoTimeoutMinutes != 60 {
		t.Errorf("cross_repo: %+v", p.CrossRepo)
	}
	if got := p.CrossRepoDemandProjects(); len(got) != 2 ||
		got[0] != "services/flexdeck" || got[1] != "services/loom-flightdeck" {
		t.Errorf("cross_repo demand_projects (enabled): got %v, want [services/flexdeck services/loom-flightdeck]", got)
	}
	if !p.Debate.Enabled.Incident || p.Debate.Enabled.Cron {
		t.Errorf("debate triggers: %+v", p.Debate.Enabled)
	}
	// AllowedFor mirrors store.CouncilTrigger strings to the
	// per-trigger flags (and returns false on unknown strings).
	if !p.Debate.Enabled.AllowedFor("incident") {
		t.Error("debate.AllowedFor(incident) must be true when triggers.incident is true")
	}
	if p.Debate.Enabled.AllowedFor("cron") {
		t.Error("debate.AllowedFor(cron) must be false when triggers.cron is false")
	}
	if p.Debate.Enabled.AllowedFor("roadmap") {
		t.Error("debate.AllowedFor(roadmap) must be false when triggers.roadmap_change is unset")
	}
	if !p.Debate.Enabled.AllowedFor("manual") {
		t.Error("debate.AllowedFor(manual) must be true when triggers.manual is true")
	}
	if p.Debate.Enabled.AllowedFor("not-a-real-trigger") {
		t.Error("debate.AllowedFor must return false on unknown trigger strings")
	}
	if !p.Recursion.Enabled || p.Recursion.MaxDepth != 2 {
		t.Errorf("recursion: %+v", p.Recursion)
	}
	if !p.AdaptivePolicy.Enabled || p.AdaptivePolicy.AutoApply {
		t.Errorf("adaptive_policy: %+v", p.AdaptivePolicy)
	}
	if len(p.AdaptivePolicy.RelaxPathDenylist) != 1 {
		t.Errorf("relax_path_denylist: %v", p.AdaptivePolicy.RelaxPathDenylist)
	}
}

// TestCrossRepoDemandProjects_TwoKeyGate pins the S6 two-key activation: the
// demand allowlist is inert unless cross_repo execution is ALSO enabled, and
// the accessor trims blanks + drops empty entries. This is the single guard
// that keeps a stray allowlist from sourcing foreign demand while cross-repo is
// off.
func TestCrossRepoDemandProjects_TwoKeyGate(t *testing.T) {
	// Disabled cross_repo: a populated allowlist yields nothing.
	off := &Policy{CrossRepo: CrossRepoPolicy{Enabled: false, DemandProjects: []string{"services/flexdeck"}}}
	if got := off.CrossRepoDemandProjects(); got != nil {
		t.Errorf("disabled cross_repo must yield nil demand projects, got %v", got)
	}
	// Enabled: trims surrounding blanks and drops empty entries.
	on := &Policy{CrossRepo: CrossRepoPolicy{Enabled: true, DemandProjects: []string{" services/flexdeck ", "  ", ""}}}
	if got := on.CrossRepoDemandProjects(); len(got) != 1 || got[0] != "services/flexdeck" {
		t.Errorf("enabled cross_repo: got %v, want [services/flexdeck]", got)
	}
	// nil policy fails closed.
	var nilp *Policy
	if got := nilp.CrossRepoDemandProjects(); got != nil {
		t.Errorf("nil policy must yield nil, got %v", got)
	}
}

// TestCrossRepoBootstrapEnabled_TwoKeyGate pins the bootstrap two-key
// activation: runtime-minted repos may source demand ONLY when cross_repo
// execution AND allow_bootstrapped are both on. Either key off fails closed,
// so a stray allow_bootstrapped can never mint or source while cross-repo is
// off, and vice versa.
func TestCrossRepoBootstrapEnabled_TwoKeyGate(t *testing.T) {
	cases := []struct {
		enabled, allow, want bool
	}{
		{false, false, false},
		{true, false, false},
		{false, true, false},
		{true, true, true},
	}
	for _, c := range cases {
		p := &Policy{CrossRepo: CrossRepoPolicy{Enabled: c.enabled, AllowBootstrapped: c.allow}}
		if got := p.CrossRepoBootstrapEnabled(); got != c.want {
			t.Errorf("enabled=%v allow=%v: got %v, want %v", c.enabled, c.allow, got, c.want)
		}
	}
	// nil policy fails closed.
	var nilp *Policy
	if nilp.CrossRepoBootstrapEnabled() {
		t.Error("nil policy must yield false")
	}
}

// TestCrossRepoBootstrapGroupAllowed pins the third safety on autonomous repo
// creation: the group allow-list. It is consulted ONLY when the two-key gate is
// on (a configured list is inert while allow_bootstrapped is off), matches are
// slash/space-trimmed and case-insensitive, and everything fails closed.
func TestCrossRepoBootstrapGroupAllowed(t *testing.T) {
	on := func(groups ...string) *Policy {
		return &Policy{CrossRepo: CrossRepoPolicy{
			Enabled: true, AllowBootstrapped: true, BootstrapAllowedGroups: groups,
		}}
	}
	cases := []struct {
		name  string
		p     *Policy
		group string
		want  bool
	}{
		{"allowed", on("services", "labs"), "services", true},
		{"allowed-labs", on("services", "labs"), "labs", true},
		{"not-listed", on("services"), "rogue", false},
		{"trimmed-input", on("services"), " services/ ", true},
		{"trimmed-config", on(" services/ "), "services", true},
		{"case-insensitive", on("Services"), "services", true},
		{"empty-group", on("services"), "", false},
		{"empty-list-fails-closed", on(), "services", false},
		// Two-key: a configured list is inert while the gate is off.
		{"gate-off-enabled-false", &Policy{CrossRepo: CrossRepoPolicy{
			Enabled: false, AllowBootstrapped: true, BootstrapAllowedGroups: []string{"services"},
		}}, "services", false},
		{"gate-off-allow-false", &Policy{CrossRepo: CrossRepoPolicy{
			Enabled: true, AllowBootstrapped: false, BootstrapAllowedGroups: []string{"services"},
		}}, "services", false},
		{"nil-policy", nil, "services", false},
	}
	for _, c := range cases {
		if got := c.p.CrossRepoBootstrapGroupAllowed(c.group); got != c.want {
			t.Errorf("%s: group=%q got %v, want %v", c.name, c.group, got, c.want)
		}
	}

	// AllowedGroups returns the trimmed list only when the gate is on; nil when
	// off or empty.
	if got := on("services", " labs/ ", "").CrossRepoBootstrapAllowedGroups(); len(got) != 2 || got[0] != "services" || got[1] != "labs" {
		t.Errorf("AllowedGroups = %v, want [services labs]", got)
	}
	if got := (&Policy{CrossRepo: CrossRepoPolicy{Enabled: false, AllowBootstrapped: true, BootstrapAllowedGroups: []string{"services"}}}).CrossRepoBootstrapAllowedGroups(); got != nil {
		t.Errorf("AllowedGroups with gate off = %v, want nil", got)
	}
}

// TestPolicy_EmptyYAMLMatchesDefault proves that parsing an essentially
// empty policy (only the required sections to clear validation) yields
// the same gating defaults as Default() — so v2 helpers fail closed
// when the operator forgets to fill the YAML.
func TestPolicy_EmptyYAMLMatchesDefault(t *testing.T) {
	body := `
budgets:
  council:  { max_usd_per_run: 0, max_usd_per_day: 0 }
  pipeline: { max_usd_per_run: 0, max_usd_per_day: 0 }
pipeline:
  retry: { max_attempts: 0, cooldown_seconds: 0 }
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("empty parse: %v", err)
	}
	// SquadsEnabled is the only helper Default() also reports false for
	// in spirit — Default() *does* turn squads on/off via the explicit
	// false. Both must agree.
	d := Default()
	if p.SquadsEnabled() != d.Squads.Enabled {
		t.Errorf("squads default mismatch: parsed=%v default=%v",
			p.SquadsEnabled(), d.Squads.Enabled)
	}
	if !p.AuditAdvisoryOnly() {
		t.Errorf("audit advisory_only must default true even on empty YAML")
	}
}

// TestPolicy_V2HelpersNilSafe codifies the fail-closed contract: every
// helper must return a safe value on a nil receiver so a misconfigured
// caller never accidentally enables a v2 feature.
func TestPolicy_V2HelpersNilSafe(t *testing.T) {
	var p *Policy
	if p.SquadsEnabled() {
		t.Errorf("nil policy must report squads disabled")
	}
	if p.AuditEnabled() {
		t.Errorf("nil policy must report audit disabled")
	}
	if !p.AuditAdvisoryOnly() {
		t.Errorf("nil policy must report audit advisory_only true (fail-safe)")
	}
	if !p.CouncilMergedWorkGroundingEnabled() {
		t.Errorf("nil policy must report merged-work grounding enabled (default ON)")
	}
	if got := p.CouncilMergedWorkLookback(); got != 14*24*time.Hour {
		t.Errorf("nil policy merged-work lookback = %v want 336h", got)
	}
	if !p.CouncilFactoryExhaustEnabled() {
		t.Errorf("nil policy must report factory-exhaust sourcing enabled (default ON)")
	}
	if got := p.CouncilFactoryExhaustLookback(); got != 14*24*time.Hour {
		t.Errorf("nil policy factory-exhaust lookback = %v want 336h", got)
	}
	if got := p.CouncilFactoryExhaustMaxItems(); got != 10 {
		t.Errorf("nil policy factory-exhaust max_items = %d want 10", got)
	}
}

// TestPolicy_CouncilFactoryExhaust covers the knob's three states, matching the
// merged-work contract: an omitted key (the common case — the default ships
// without a ConfigMap edit), an explicit opt-out, and explicit bounds.
func TestPolicy_CouncilFactoryExhaust(t *testing.T) {
	omitted, err := ParsePolicy([]byte(fixtureV1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !omitted.CouncilFactoryExhaustEnabled() {
		t.Errorf("omitted key must default to enabled")
	}
	if got := omitted.CouncilFactoryExhaustLookback(); got != 14*24*time.Hour {
		t.Errorf("omitted lookback = %v want 336h", got)
	}
	if got := omitted.CouncilFactoryExhaustMaxItems(); got != 10 {
		t.Errorf("omitted max_items = %d want 10", got)
	}

	off := false
	tuned := &Policy{Council: CouncilPolicy{Sources: CouncilSourcesPolicy{
		FactoryExhaust: CouncilFactoryExhaustPolicy{Enabled: &off, LookbackHours: 72, MaxItems: 3},
	}}}
	if tuned.CouncilFactoryExhaustEnabled() {
		t.Errorf("explicit enabled:false must disable the source")
	}
	if got := tuned.CouncilFactoryExhaustLookback(); got != 72*time.Hour {
		t.Errorf("lookback = %v want 72h", got)
	}
	if got := tuned.CouncilFactoryExhaustMaxItems(); got != 3 {
		t.Errorf("max_items = %d want 3", got)
	}
}

// The bounds are validated, not silently clamped: a negative value is a
// misconfiguration the operator should hear about at parse time.
func TestPolicy_CouncilFactoryExhaustValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy CouncilFactoryExhaustPolicy
		want   string
	}{
		{"negative lookback", CouncilFactoryExhaustPolicy{LookbackHours: -1}, "lookback_hours"},
		{"negative max items", CouncilFactoryExhaustPolicy{MaxItems: -1}, "max_items"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			p.Council.Sources.FactoryExhaust = tc.policy
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestPolicy_CouncilMergedWorkGrounding covers the flag's three states: an
// omitted key (the common case — the default ships without a ConfigMap edit),
// an explicit opt-out, and an explicit window override.
func TestPolicy_CouncilMergedWorkGrounding(t *testing.T) {
	omitted, err := ParsePolicy([]byte(fixtureV1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !omitted.CouncilMergedWorkGroundingEnabled() {
		t.Errorf("omitted key must default to enabled")
	}
	if got := omitted.CouncilMergedWorkLookback(); got != 14*24*time.Hour {
		t.Errorf("omitted lookback = %v want 336h", got)
	}

	off := false
	tuned := &Policy{Council: CouncilPolicy{Dedup: CouncilDedupPolicy{
		MergedWork: CouncilMergedWorkPolicy{Enabled: &off, LookbackHours: 48},
	}}}
	if tuned.CouncilMergedWorkGroundingEnabled() {
		t.Errorf("explicit enabled:false must disable grounding")
	}
	if got := tuned.CouncilMergedWorkLookback(); got != 48*time.Hour {
		t.Errorf("lookback = %v want 48h", got)
	}
}

func TestPolicyManager_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(fixtureV1), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var reloadErr atomic.Value
	mgr, err := NewPolicyManager(ctx, path, PolicyManagerOptions{
		OnError: func(e error) { reloadErr.Store(e) },
	})
	if err != nil {
		t.Fatalf("new mgr: %v", err)
	}
	defer mgr.Close()

	if mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != 4 {
		t.Errorf("initial cap: %d", mgr.Current().Budgets.Pipeline.MaxConcurrentRuns)
	}

	// fsnotify can be flaky on macOS — fall back to manual Reload after a
	// short fs-watch attempt. The semantics under test are atomic swap and
	// validation, not the OS notification path.
	updated := strings.Replace(fixtureV1, "max_concurrent_runs: 4", "max_concurrent_runs: 8", 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}

	// Subscribe so we can wait for a notification.
	notified := make(chan struct{}, 1)
	mgr.Subscribe(func(_, _ *Policy) {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	deadline := time.After(2 * time.Second)
	for mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != 8 {
		select {
		case <-notified:
		case <-deadline:
			// Notification didn't arrive — invoke Reload manually.
			if err := mgr.Reload(); err != nil {
				t.Fatalf("manual reload: %v", err)
			}
		}
	}

	if mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != 8 {
		t.Errorf("after reload: %d", mgr.Current().Budgets.Pipeline.MaxConcurrentRuns)
	}
	if v := reloadErr.Load(); v != nil {
		t.Errorf("unexpected reload error: %v", v)
	}
}

// TestPolicyManager_HotReload_K8sConfigMapSwap pins the regression behind
// the 2026-05-04 squads flip incident: K8s projected ConfigMap mounts use
// a chain of symlinks (`policy.yaml` → `..data/policy.yaml`, `..data` →
// timestamped subdir). When the data swaps, fsnotify emits Create/Rename
// events for `..data` (not `policy.yaml`), so a strict ev.Name == target
// match dropped every ConfigMap update and the operator only picked up
// new policies on a manual rolling restart. This test simulates the
// `..data` symlink swap and asserts the watcher reloads.
func TestPolicyManager_HotReload_K8sConfigMapSwap(t *testing.T) {
	dir := t.TempDir()

	// Stage v1 inside ..2026_05_04_initial / .
	dataInitial := filepath.Join(dir, "..2026_05_04_initial")
	if err := os.MkdirAll(dataInitial, 0o755); err != nil {
		t.Fatalf("mkdir initial: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataInitial, "policy.yaml"),
		[]byte(fixtureV1), 0o644); err != nil {
		t.Fatalf("seed initial: %v", err)
	}
	if err := os.Symlink(filepath.Base(dataInitial), filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	if err := os.Symlink("..data/policy.yaml", filepath.Join(dir, "policy.yaml")); err != nil {
		t.Fatalf("symlink policy.yaml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := NewPolicyManager(ctx, filepath.Join(dir, "policy.yaml"), PolicyManagerOptions{
		OnError: func(e error) { t.Logf("reload error: %v", e) },
	})
	if err != nil {
		t.Fatalf("new mgr: %v", err)
	}
	defer mgr.Close()

	if mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != 4 {
		t.Errorf("initial cap: %d", mgr.Current().Budgets.Pipeline.MaxConcurrentRuns)
	}

	notified := make(chan struct{}, 1)
	mgr.Subscribe(func(_, _ *Policy) {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	// Stage v2 in a new timestamped dir + atomic ..data swap. This is the
	// exact dance kubelet does on a ConfigMap update. The "..data" symlink
	// is renamed via Rename(2) so it appears as a single fsnotify event.
	dataNext := filepath.Join(dir, "..2026_05_04_updated")
	if err := os.MkdirAll(dataNext, 0o755); err != nil {
		t.Fatalf("mkdir next: %v", err)
	}
	updated := strings.Replace(fixtureV1, "max_concurrent_runs: 4", "max_concurrent_runs: 8", 1)
	if err := os.WriteFile(filepath.Join(dataNext, "policy.yaml"),
		[]byte(updated), 0o644); err != nil {
		t.Fatalf("seed next: %v", err)
	}
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(dataNext), tmpLink); err != nil {
		t.Fatalf("create tmp symlink: %v", err)
	}
	// kqueue discovers directory changes by rescanning. If the staging link
	// is created and renamed in the same timeslice, Darwin can collapse the
	// two operations and emit no child event at all. Keep the kubelet staging
	// link observable so this test exercises the portable staging-event path.
	if runtime.GOOS == "darwin" {
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("atomic swap ..data: %v", err)
	}

	// Wait for the watcher to fire. fsnotify can be slightly delayed on
	// macOS so allow a generous deadline; the watcher is what's under
	// test, not the OS notification path.
	deadline := time.After(3 * time.Second)
	for mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != 8 {
		select {
		case <-notified:
		case <-deadline:
			t.Fatalf("ConfigMap swap did not trigger reload: cap still %d (regression: fsnotify watch on `..data` symlink swap)",
				mgr.Current().Budgets.Pipeline.MaxConcurrentRuns)
		}
	}
}

func TestPolicyManager_BadReloadKeepsOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(fixtureV1), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mgr, err := NewPolicyManager(context.Background(), path, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("new mgr: %v", err)
	}
	defer mgr.Close()
	originalRuns := mgr.Current().Budgets.Pipeline.MaxConcurrentRuns

	if err := os.WriteFile(path, []byte("version: 99\nbudgets: {}\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if err := mgr.Reload(); err == nil {
		t.Errorf("expected validation error on bad policy")
	}
	if mgr.Current().Budgets.Pipeline.MaxConcurrentRuns != originalRuns {
		t.Errorf("bad reload clobbered policy: now %d", mgr.Current().Budgets.Pipeline.MaxConcurrentRuns)
	}
}

func TestPolicy_StageSubstrate_Defaults(t *testing.T) {
	// nil receiver and empty map both fall back to SubstrateDefault.
	var nilPol *Policy
	if got := nilPol.SubstrateForStage("implement"); got != SubstrateDefault {
		t.Errorf("nil policy: got %q want %q", got, SubstrateDefault)
	}

	p := &Policy{}
	for _, stage := range []string{"plan_slice", "research", "implement", "tests", "pr_self_review", "mr"} {
		if got := p.SubstrateForStage(stage); got != SubstrateDefault {
			t.Errorf("empty policy stage %s: got %q want %q", stage, got, SubstrateDefault)
		}
	}

	// Explicit empty value also falls back to default.
	p.Pipeline.StageSubstrate = map[string]string{"implement": ""}
	if got := p.SubstrateForStage("implement"); got != SubstrateDefault {
		t.Errorf("empty-string entry: got %q want %q", got, SubstrateDefault)
	}
}

func TestPolicy_StageSubstrate_LookupHits(t *testing.T) {
	p := &Policy{
		Pipeline: PipelinePolicy{
			StageSubstrate: map[string]string{
				"plan_slice":     "k8s",
				"implement":      "harvester-vm",
				"tests":          "harvester-vm",
				"pr_self_review": "k8s",
			},
		},
	}
	cases := []struct{ stage, want string }{
		{"plan_slice", "k8s"},
		{"implement", "harvester-vm"},
		{"tests", "harvester-vm"},
		{"pr_self_review", "k8s"},
		{"research", SubstrateDefault}, // unset → default
		{"mr", SubstrateDefault},       // not configurable → default
		{"unknown", SubstrateDefault},  // never configured → default
	}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			if got := p.SubstrateForStage(tc.stage); got != tc.want {
				t.Errorf("SubstrateForStage(%q): got %q want %q", tc.stage, got, tc.want)
			}
		})
	}
}

func TestPolicy_StageSubstrate_Validate(t *testing.T) {
	cases := []struct {
		name        string
		substrate   map[string]string
		wantErrFrag string // empty = expect success
	}{
		{
			name: "all valid keys + values",
			substrate: map[string]string{
				"plan_slice":     "k8s",
				"research":       "k8s",
				"implement":      "harvester-vm",
				"tests":          "harvester-vm",
				"pr_self_review": "k8s",
			},
		},
		{
			name:        "unknown stage key",
			substrate:   map[string]string{"mr": "k8s"},
			wantErrFrag: "is not a configurable stage",
		},
		{
			name:        "unknown substrate value",
			substrate:   map[string]string{"implement": "docker"},
			wantErrFrag: "is not a recognized substrate",
		},
		{
			name:        "empty substrate value rejected by validate",
			substrate:   map[string]string{"implement": ""},
			wantErrFrag: "is not a recognized substrate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(fixtureV1))
			if err != nil {
				t.Fatalf("setup parse: %v", err)
			}
			p.Pipeline.StageSubstrate = tc.substrate
			err = p.Validate()
			if tc.wantErrFrag == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("expected error containing %q, got %v", tc.wantErrFrag, err)
			}
		})
	}
}

func TestPolicy_StageSubstrate_Roundtrip(t *testing.T) {
	body := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_substrate:
    plan_slice:     k8s
    implement:      harvester-vm
    tests:          harvester-vm
    pr_self_review: k8s
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got, want := p.SubstrateForStage("implement"), "harvester-vm"; got != want {
		t.Errorf("SubstrateForStage(implement): got %q want %q", got, want)
	}
	if got, want := p.SubstrateForStage("research"), SubstrateDefault; got != want {
		t.Errorf("SubstrateForStage(research): got %q want %q", got, want)
	}
}

func TestPolicy_StageAgents_Defaults(t *testing.T) {
	// nil receiver, empty map, and empty-string entries all resolve to "" —
	// "no policy override" so the operator's env break-glass / AgentDefault
	// applies at wiring time.
	var nilPol *Policy
	if got := nilPol.AgentForStage("pr_self_review"); got != "" {
		t.Errorf("nil policy: got %q want empty", got)
	}

	p := &Policy{}
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review", "research", "tests", "mr"} {
		if got := p.AgentForStage(stage); got != "" {
			t.Errorf("empty policy stage %s: got %q want empty", stage, got)
		}
	}

	// Explicit empty value also resolves to "" (treated as unset).
	p.Pipeline.StageAgents = map[string]string{"pr_self_review": ""}
	if got := p.AgentForStage("pr_self_review"); got != "" {
		t.Errorf("empty-string entry: got %q want empty", got)
	}
}

func TestPolicy_StageAgents_LookupHits(t *testing.T) {
	p := &Policy{
		Pipeline: PipelinePolicy{
			StageAgents: map[string]string{
				"plan_slice":     "claude-code",
				"implement":      "claude-code",
				"pr_self_review": "gemini",
			},
		},
	}
	cases := []struct{ stage, want string }{
		{"plan_slice", "claude-code"},
		{"implement", "claude-code"},
		{"pr_self_review", "gemini"}, // the cheaper reviewer override
		{"research", ""},             // unset → no override
		{"mr", ""},                   // not configurable → no override
		{"unknown", ""},              // never configured → no override
	}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			if got := p.AgentForStage(tc.stage); got != tc.want {
				t.Errorf("AgentForStage(%q): got %q want %q", tc.stage, got, tc.want)
			}
		})
	}
}

func TestPolicy_StageAgents_Validate(t *testing.T) {
	cases := []struct {
		name        string
		agents      map[string]string
		wantErrFrag string // empty = expect success
	}{
		{
			name: "all valid keys + values",
			agents: map[string]string{
				"plan_slice":     "claude-code",
				"implement":      "codex",
				"pr_self_review": "gemini",
			},
		},
		{
			name:        "unknown stage key",
			agents:      map[string]string{"mr": "claude-code"},
			wantErrFrag: "is not an agent-configurable stage",
		},
		{
			// research has a devbox substrate but no agent selection, so it
			// is NOT an agent-configurable stage even though it IS a valid
			// stage_substrate key.
			name:        "substrate-only stage rejected for agents",
			agents:      map[string]string{"research": "gemini"},
			wantErrFrag: "is not an agent-configurable stage",
		},
		{
			name:        "unknown agent value",
			agents:      map[string]string{"pr_self_review": "gpt-5"},
			wantErrFrag: "is not a recognized agent",
		},
		{
			name:        "empty agent value rejected by validate",
			agents:      map[string]string{"pr_self_review": ""},
			wantErrFrag: "is not a recognized agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(fixtureV1))
			if err != nil {
				t.Fatalf("setup parse: %v", err)
			}
			p.Pipeline.StageAgents = tc.agents
			err = p.Validate()
			if tc.wantErrFrag == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("expected error containing %q, got %v", tc.wantErrFrag, err)
			}
		})
	}
}

func TestPolicy_StageAgents_Roundtrip(t *testing.T) {
	body := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_agents:
    pr_self_review: gemini
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got, want := p.AgentForStage("pr_self_review"), "gemini"; got != want {
		t.Errorf("AgentForStage(pr_self_review): got %q want %q", got, want)
	}
	if got := p.AgentForStage("implement"); got != "" {
		t.Errorf("AgentForStage(implement): got %q want empty (no override)", got)
	}
}

// TestPolicy_StageAgentKeys_SubsetOfSubstrate documents + guards the invariant
// that every agent-configurable stage is also a substrate-configurable stage
// (both are spawn-adjacent), while the reverse need not hold — research/tests
// have a substrate but no agent choice.
func TestPolicy_StageAgentKeys_SubsetOfSubstrate(t *testing.T) {
	for stage := range StageAgentKeysValid {
		if _, ok := StageSubstrateKeysValid[stage]; !ok {
			t.Errorf("stage %q is agent-configurable but not in StageSubstrateKeysValid", stage)
		}
	}
	// The agent set is strictly narrower: research + tests are substrate-only.
	for _, substrateOnly := range []string{"research", "tests"} {
		if _, ok := StageAgentKeysValid[substrateOnly]; ok {
			t.Errorf("stage %q must NOT be agent-configurable (no agent/harness choice)", substrateOnly)
		}
	}
}

func TestPolicy_StageModels_Defaults(t *testing.T) {
	// nil receiver, empty map, and empty-string entries all resolve to "" —
	// "no policy override" so the operator's env break-glass / vendor default
	// applies at spawn time.
	var nilPol *Policy
	if got := nilPol.ModelForStage("implement"); got != "" {
		t.Errorf("nil policy: got %q want empty", got)
	}

	p := &Policy{}
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review", "research", "tests", "mr"} {
		if got := p.ModelForStage(stage); got != "" {
			t.Errorf("empty policy stage %s: got %q want empty", stage, got)
		}
	}

	// Explicit empty value also resolves to "" (treated as unset).
	p.Pipeline.StageModels = map[string]string{"implement": ""}
	if got := p.ModelForStage("implement"); got != "" {
		t.Errorf("empty-string entry: got %q want empty", got)
	}
}

func TestPolicy_StageModels_LookupHits(t *testing.T) {
	p := &Policy{
		Pipeline: PipelinePolicy{
			StageModels: map[string]string{
				"implement":  "gpt-5.6-terra",
				"plan_slice": "gpt-5.6-sol",
			},
		},
	}
	cases := []struct{ stage, want string }{
		{"implement", "gpt-5.6-terra"},
		{"plan_slice", "gpt-5.6-sol"},
		{"pr_self_review", ""}, // unset → no override
		{"research", ""},       // not configurable → no override
		{"unknown", ""},        // never configured → no override
	}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			if got := p.ModelForStage(tc.stage); got != tc.want {
				t.Errorf("ModelForStage(%q): got %q want %q", tc.stage, got, tc.want)
			}
		})
	}
}

func TestPolicy_StageModels_Validate(t *testing.T) {
	cases := []struct {
		name        string
		models      map[string]string
		wantErrFrag string // empty = expect success
	}{
		{
			name: "all valid keys + vendor-native ids",
			models: map[string]string{
				"plan_slice":     "gpt-5.6-sol",
				"implement":      "gpt-5.6-terra",
				"pr_self_review": "openai/gpt-5.6-terra",
			},
		},
		{
			name:        "unknown stage key",
			models:      map[string]string{"mr": "gpt-5.6-terra"},
			wantErrFrag: "is not a model-configurable stage",
		},
		{
			// research has a devbox substrate but no spawn model, so it is NOT a
			// model-configurable stage even though it IS a stage_substrate key.
			name:        "substrate-only stage rejected for models",
			models:      map[string]string{"research": "gpt-5.6-terra"},
			wantErrFrag: "is not a model-configurable stage",
		},
		{
			name:        "empty model value rejected",
			models:      map[string]string{"implement": ""},
			wantErrFrag: "is not a valid model id",
		},
		{
			name:        "whitespace model value rejected",
			models:      map[string]string{"implement": "gpt 5.6 terra"},
			wantErrFrag: "is not a valid model id",
		},
		{
			name:        "shell-metachar model value rejected",
			models:      map[string]string{"implement": "gpt-5.6;rm -rf"},
			wantErrFrag: "is not a valid model id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(fixtureV1))
			if err != nil {
				t.Fatalf("setup parse: %v", err)
			}
			p.Pipeline.StageModels = tc.models
			err = p.Validate()
			if tc.wantErrFrag == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("expected error containing %q, got %v", tc.wantErrFrag, err)
			}
		})
	}
}

func TestPolicy_StageModels_Roundtrip(t *testing.T) {
	body := `
version: 2
budgets:
  council:  { max_usd_per_run: 15, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 75 }
pipeline:
  retry: { max_attempts: 3, cooldown_seconds: 300 }
  stage_models:
    implement:  gpt-5.6-terra
    plan_slice: gpt-5.6-sol
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got, want := p.ModelForStage("implement"), "gpt-5.6-terra"; got != want {
		t.Errorf("ModelForStage(implement): got %q want %q", got, want)
	}
	if got, want := p.ModelForStage("plan_slice"), "gpt-5.6-sol"; got != want {
		t.Errorf("ModelForStage(plan_slice): got %q want %q", got, want)
	}
	if got := p.ModelForStage("pr_self_review"); got != "" {
		t.Errorf("ModelForStage(pr_self_review): got %q want empty (no override)", got)
	}
}

// TestValidModelToken pins the vendor-native model-id shape guard directly.
func TestValidModelToken(t *testing.T) {
	valid := []string{"gpt-5.6-terra", "gpt-5.6-sol", "openai/gpt-5.6-terra", "claude-opus-4-8", "kimi-k3:0711", "gpt_5.6"}
	for _, s := range valid {
		if !validModelToken(s) {
			t.Errorf("validModelToken(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "   ", "gpt 5.6", "gpt;rm", "$(x)", "-leading-dash", strings.Repeat("a", 129)}
	for _, s := range invalid {
		if validModelToken(s) {
			t.Errorf("validModelToken(%q) = true, want false", s)
		}
	}
}

// TestTakeupPolicy_AccessorsAndFailClosed pins the take-up reconciler's policy
// surface: default-off, namespace fail-closed (trimmed), project fallback
// left to the caller, and the 5-minute default cadence.
func TestTakeupPolicy_AccessorsAndFailClosed(t *testing.T) {
	var nilPolicy *Policy
	if nilPolicy.TakeupEnabled() {
		t.Fatal("nil policy must report take-up disabled")
	}
	if nilPolicy.TakeupPollInterval() != 5*time.Minute {
		t.Fatalf("nil policy poll = %v, want 5m", nilPolicy.TakeupPollInterval())
	}
	if nilPolicy.TakeupTickTimeout() != 2*time.Minute {
		t.Fatalf("nil policy tick timeout = %v, want 2m", nilPolicy.TakeupTickTimeout())
	}

	p := &Policy{}
	if p.TakeupEnabled() {
		t.Fatal("zero-value policy must report take-up disabled (default-off)")
	}
	if p.TakeupTickTimeout() != 2*time.Minute {
		t.Fatalf("zero-value tick timeout = %v, want 2m default", p.TakeupTickTimeout())
	}

	p.Intake.Takeup = TakeupPolicy{
		Enabled:             true,
		Namespace:           "  mills/eligible  ",
		Project:             " services/loom-core ",
		PollIntervalSeconds: 60,
		TickTimeoutSeconds:  30,
	}
	if !p.TakeupEnabled() {
		t.Fatal("enabled flag not honored")
	}
	if got := p.TakeupNamespace(); got != "mills/eligible" {
		t.Fatalf("namespace = %q, want trimmed", got)
	}
	if got := p.TakeupProject(); got != "services/loom-core" {
		t.Fatalf("project = %q, want trimmed", got)
	}
	if got := p.TakeupPollInterval(); got != time.Minute {
		t.Fatalf("poll = %v, want 1m", got)
	}
	if got := p.TakeupTickTimeout(); got != 30*time.Second {
		t.Fatalf("tick timeout = %v, want 30s", got)
	}
}

func TestSpinningRoomPolicy_AccessorsAndFailClosed(t *testing.T) {
	var nilPolicy *Policy
	if nilPolicy.SpinningRoomEnabled() {
		t.Fatal("nil policy must report spinning room disabled")
	}
	if nilPolicy.SpinningRoomFrames() != nil {
		t.Fatal("nil policy must report no frames")
	}
	if nilPolicy.SpinningRoomDefaultPriority() != "P2" {
		t.Fatalf("nil policy default priority = %q, want P2", nilPolicy.SpinningRoomDefaultPriority())
	}

	// Disabled policy: frames present but Enabled=false => inert (fail-closed).
	disabled := &Policy{SpinningRoom: SpinningRoomPolicy{
		Frames: []CouncilAgent{{Name: "opus", Model: "claude-opus", Backend: "flexinfer"}},
	}}
	if disabled.SpinningRoomEnabled() {
		t.Fatal("Enabled=false must report disabled even with frames")
	}
	if disabled.SpinningRoomFrames() != nil {
		t.Fatal("disabled room must expose no frames")
	}
	if _, ok := disabled.SpinningRoomFrame("opus"); ok {
		t.Fatal("disabled room must not resolve a frame")
	}

	p := &Policy{SpinningRoom: SpinningRoomPolicy{
		Enabled:         true,
		DefaultPriority: "p1",
		Frames: []CouncilAgent{
			{Name: "opus", Model: "claude-opus", Backend: "flexinfer"},
			{Name: "", Model: "orphan", Backend: "flexinfer"}, // unnamed => dropped
			{Name: "gpt", Model: "gpt-5.4", Backend: "openai-responses"},
		},
	}}
	if !p.SpinningRoomEnabled() {
		t.Fatal("enabled flag not honored")
	}
	frames := p.SpinningRoomFrames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (unnamed dropped)", len(frames))
	}
	if got := p.SpinningRoomDefaultPriority(); got != "P1" {
		t.Fatalf("default priority = %q, want normalized P1", got)
	}
	// Case-insensitive, trimmed resolution.
	f, ok := p.SpinningRoomFrame("  GPT  ")
	if !ok || f.Model != "gpt-5.4" || f.Backend != "openai-responses" {
		t.Fatalf("frame lookup = %+v ok=%v, want gpt-5.4/openai-responses", f, ok)
	}
	if _, ok := p.SpinningRoomFrame("nope"); ok {
		t.Fatal("off-policy frame must not resolve")
	}
}

func TestSpinningRoomPolicy_Validate(t *testing.T) {
	cases := []struct {
		name    string
		room    SpinningRoomPolicy
		wantErr bool
	}{
		{"disabled ignores frames", SpinningRoomPolicy{Frames: []CouncilAgent{{Model: "m"}}}, false},
		{"enabled ok", SpinningRoomPolicy{Enabled: true, Frames: []CouncilAgent{{Name: "a", Model: "m"}}}, false},
		{"enabled missing name", SpinningRoomPolicy{Enabled: true, Frames: []CouncilAgent{{Model: "m"}}}, true},
		{"enabled missing model", SpinningRoomPolicy{Enabled: true, Frames: []CouncilAgent{{Name: "a"}}}, true},
		{"duplicate names", SpinningRoomPolicy{Enabled: true, Frames: []CouncilAgent{{Name: "a", Model: "m"}, {Name: "A", Model: "n"}}}, true},
		{"bad default priority", SpinningRoomPolicy{Enabled: true, DefaultPriority: "P9", Frames: []CouncilAgent{{Name: "a", Model: "m"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			p.SpinningRoom = tc.room
			err := p.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestSpinningRoomPolicy_ParsesFromYAML(t *testing.T) {
	body := `
version: 2
budgets:
  council:
    max_usd_per_run: 15
    max_usd_per_day: 50
  pipeline:
    max_usd_per_run: 5
    max_usd_per_day: 20
spinning_room:
  enabled: true
  default_priority: P1
  frames:
    - name: opus
      model: claude-opus
      backend: flexinfer
    - name: gpt
      model: gpt-5.4
      backend: openai-responses
`
	p, err := ParsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if !p.SpinningRoomEnabled() {
		t.Fatal("spinning_room.enabled true must surface via helper")
	}
	if len(p.SpinningRoomFrames()) != 2 {
		t.Fatalf("frames = %d, want 2", len(p.SpinningRoomFrames()))
	}
	if f, ok := p.SpinningRoomFrame("opus"); !ok || f.Model != "claude-opus" {
		t.Fatalf("opus frame = %+v ok=%v", f, ok)
	}
}

// The roadmap-intent guardrail must fail CLOSED: an omitted key, a nil policy,
// and Default() all resolve to true. Only an explicit `false` opts out.
func TestCouncilRequireRoadmapIntentsFailsClosed(t *testing.T) {
	base := `
version: 2
budgets:
  council:  { max_usd_per_run: 5, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 50 }
council:
  schedule_cron: "0 5 * * *"
`
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"omitted", base, true},
		{"explicit false", base + "  require_roadmap_intents: false\n", false},
		{"explicit true", base + "  require_roadmap_intents: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := p.CouncilRequireRoadmapIntents(); got != tc.want {
				t.Fatalf("CouncilRequireRoadmapIntents() = %v, want %v", got, tc.want)
			}
		})
	}

	if got := (*Policy)(nil).CouncilRequireRoadmapIntents(); !got {
		t.Error("nil policy must fail closed")
	}
	if got := Default().CouncilRequireRoadmapIntents(); !got {
		t.Error("Default() must fail closed")
	}

	// Rollout safety: ParsePolicy is lenient, so a NEW ConfigMap carrying the
	// key still parses on an OLD binary and vice versa. No ordered rollout.
	if _, err := ParsePolicy([]byte(base + "  some_future_key: 3\n")); err != nil {
		t.Fatalf("unknown key must not break parsing: %v", err)
	}
}
