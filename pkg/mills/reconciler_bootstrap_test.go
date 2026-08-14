package mills

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeRepoEnsurer records EnsureRepo calls and serves a canned result so the
// reconciler pre-flight can be driven without a real bootstrap.Service.
type fakeRepoEnsurer struct {
	calls   []string // target projects seen
	reasons []string // reason arg (backlog item id) per call
	created bool
	err     error
	// seedPaths overrides the scaffold the mint reports. Nil means the real
	// default, so existing cases exercise the production list.
	seedPaths []string
}

func (f *fakeRepoEnsurer) SeedPaths() []string {
	if f.seedPaths != nil {
		return f.seedPaths
	}
	return []string{"README.md", ".gitlab-ci.yml", ".gitignore", "AGENTS.md"}
}

type fakeClassifiedRepoEnsureError struct {
	code      string
	retryable bool
}

func (e *fakeClassifiedRepoEnsureError) Error() string       { return "classified bootstrap failure" }
func (e *fakeClassifiedRepoEnsureError) FailureCode() string { return e.code }
func (e *fakeClassifiedRepoEnsureError) Retryable() bool     { return e.retryable }

func (f *fakeRepoEnsurer) EnsureRepo(_ context.Context, project, reason string) (bool, string, error) {
	f.calls = append(f.calls, project)
	f.reasons = append(f.reasons, reason)
	return f.created, "https://gl/" + project, f.err
}

// bootstrapEnabledPolicy returns an enabled policy with cross_repo execution on
// and the bootstrap two-key gate on, allow-listing the given groups.
func bootstrapEnabledPolicy(groups ...string) *Policy {
	pol := Default()
	on := true
	pol.Enabled = &on
	pol.CrossRepo.Enabled = true
	pol.CrossRepo.AllowBootstrapped = true
	pol.CrossRepo.BootstrapAllowedGroups = groups
	return pol
}

func countBootstrapEvents(t *testing.T, env *recTestEnv) int {
	t.Helper()
	n, err := env.store.Events.CountByKindSince(
		context.Background(), "reconciler.bootstrap", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count bootstrap events: %v", err)
	}
	return n
}

// TestReconciler_BootstrapPreflightCreatesMissingRepo: a cross-repo item whose
// target group is allow-listed triggers EnsureRepo, which mints the repo; the
// item then starts and a bootstrap event is recorded.
func TestReconciler_BootstrapPreflightCreatesMissingRepo(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{created: true}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dec, run, _, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionStarted || run == nil {
		t.Errorf("expected decisionStarted with a run, got dec=%v run=%v", dec, run)
	}
	if len(ens.calls) != 1 || ens.calls[0] != "services/familyforge" {
		t.Errorf("EnsureRepo calls = %v, want one for services/familyforge", ens.calls)
	}
	if len(ens.reasons) != 1 || ens.reasons[0] != "MILLS-BOOT" {
		t.Errorf("EnsureRepo reason = %v, want [MILLS-BOOT] (the item id)", ens.reasons)
	}
	// A mint now emits two bootstrap events: repo_created, and
	// seed_scope_declared for the scaffold stamped into the item's scope.
	if got := countBootstrapEvents(t, env); got != 2 {
		t.Errorf("bootstrap events = %d, want 2 (repo_created + seed_scope_declared)", got)
	}
	if env.starter.calls() != 1 {
		t.Errorf("starter should run after a successful pre-flight, got %d", env.starter.calls())
	}

	// The mint must DECLARE the scaffolding it seeded. The item's slices were
	// authored before the repo existed, so without this the first implementer
	// escalates at the scope gate for editing the placeholder .gitlab-ci.yml
	// that the seeded AGENTS.md tells it to replace (2026-07-27,
	// services/housemd).
	stored, err := env.store.Backlog.Get(ctx, "MILLS-BOOT")
	if err != nil {
		t.Fatalf("reload item: %v", err)
	}
	var scaffold *store.Slice
	for i := range stored.Slices {
		if stored.Slices[i].Name == seedScopeSliceName {
			scaffold = &stored.Slices[i]
		}
	}
	if scaffold == nil {
		t.Fatalf("item declares no %q slice; slices=%+v", seedScopeSliceName, stored.Slices)
	}
	for _, want := range ens.SeedPaths() {
		if !slices.Contains(scaffold.Files, want) {
			t.Errorf("scaffold slice missing %q; got %v", want, scaffold.Files)
		}
	}
}

// TestReconciler_BootstrapSeedScopeIsIdempotent: a repo that already exists
// mints nothing, so nothing is stamped; and a second mint-and-stamp on an item
// that already carries the slice must not append a duplicate.
func TestReconciler_BootstrapSeedScopeIsIdempotent(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	env.rec.RepoEnsurer = &fakeRepoEnsurer{created: true}
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-IDEM", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 2; i++ {
		fresh, err := env.store.Backlog.Get(ctx, "MILLS-BOOT-IDEM")
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		env.rec.stampSeedScopeOnMint(ctx, fresh, "services/familyforge")
	}
	stored, err := env.store.Backlog.Get(ctx, "MILLS-BOOT-IDEM")
	if err != nil {
		t.Fatalf("reload item: %v", err)
	}
	n := 0
	for _, s := range stored.Slices {
		if s.Name == seedScopeSliceName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%q slices = %d, want exactly 1 (stamp must be idempotent)", seedScopeSliceName, n)
	}
}

// TestReconciler_BootstrapSeedScopeNotStampedWhenRepoExisted: EnsureRepo
// reporting created=false means the repo was already there, so its scope is
// whatever the plan authored — the mint must not widen it.
func TestReconciler_BootstrapSeedScopeNotStampedWhenRepoExisted(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	env.rec.RepoEnsurer = &fakeRepoEnsurer{created: false}
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-EXISTS", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, _, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services")); err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	stored, err := env.store.Backlog.Get(ctx, "MILLS-BOOT-EXISTS")
	if err != nil {
		t.Fatalf("reload item: %v", err)
	}
	for _, s := range stored.Slices {
		if s.Name == seedScopeSliceName {
			t.Fatal("an existing repo must not have its scope widened by the pre-flight")
		}
	}
}

// TestReconciler_BootstrapPreflightSkippedWhenGroupNotAllowed: cross_repo is on
// but the target's group is not allow-listed. The pre-flight is a no-op — no
// EnsureRepo call — and the item proceeds (a missing repo would then fall
// through to the clone-time escalation, unchanged from pre-bootstrap).
func TestReconciler_BootstrapPreflightSkippedWhenGroupNotAllowed(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{created: true}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-NG", "labs/experiment")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Allow-list only "services"; the item targets "labs".
	dec, run, _, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionStarted || run == nil {
		t.Errorf("expected decisionStarted (no pre-flight, falls through), got dec=%v run=%v", dec, run)
	}
	if len(ens.calls) != 0 {
		t.Errorf("EnsureRepo must not run for a non-allow-listed group, got %v", ens.calls)
	}
}

// TestReconciler_BootstrapPreflightDefersOnError: a transient EnsureRepo failure
// defers the item (retry next tick) rather than dispatching into a guaranteed
// clone failure; the starter never runs.
func TestReconciler_BootstrapPreflightDefersOnError(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{err: errors.New("gitlab 502")}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-ERR", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dec, run, reason, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionDeferred || run != nil {
		t.Errorf("expected decisionDeferred with no run, got dec=%v run=%v", dec, run)
	}
	if reason == "" {
		t.Errorf("expected a non-empty defer reason")
	}
	if len(ens.calls) != 1 {
		t.Errorf("EnsureRepo should have been attempted once, got %v", ens.calls)
	}
	if env.starter.calls() != 0 {
		t.Errorf("starter must not run when the pre-flight defers, got %d", env.starter.calls())
	}
}

// A persistent pre-flight failure must not occupy the fast cadence forever.
func TestReconciler_BootstrapPreflightRetryBudgetEscalates(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{err: errors.New("gitlab 502")}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-CAP", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	policy := bootstrapEnabledPolicy("services")
	policy.Pipeline.Retry.MaxAttempts = 3

	for attempt := 1; attempt <= policy.Pipeline.Retry.MaxAttempts; attempt++ {
		current, err := env.store.Backlog.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("attempt %d read backlog: %v", attempt, err)
		}
		decision, run, reason, err := env.rec.tryStart(ctx, current, policy)
		if err != nil {
			t.Fatalf("attempt %d tryStart: %v", attempt, err)
		}
		if run != nil || reason == "" {
			t.Fatalf("attempt %d: run=%v reason=%q, want no run and an actionable reason", attempt, run, reason)
		}
		if attempt < policy.Pipeline.Retry.MaxAttempts && decision != decisionDeferred {
			t.Fatalf("attempt %d decision=%s, want deferred", attempt, decision)
		}
		if attempt == policy.Pipeline.Retry.MaxAttempts && decision != decisionSkipped {
			t.Fatalf("attempt %d decision=%s, want terminal skip", attempt, decision)
		}
	}

	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("read escalated backlog: %v", err)
	}
	if got.State != store.BacklogEscalated {
		t.Fatalf("backlog state=%s, want escalated after retry budget", got.State)
	}
	if len(ens.calls) != policy.Pipeline.Retry.MaxAttempts {
		t.Fatalf("EnsureRepo calls=%d, want %d", len(ens.calls), policy.Pipeline.Retry.MaxAttempts)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("starter must not run after bootstrap failures, got %d calls", env.starter.calls())
	}
	failures, err := env.store.Events.CountBySubjectKind(
		ctx,
		bootstrapFailureSubjectKind,
		bootstrapFailureSubjectID(item.ID, item.TargetProject),
		bootstrapFailureEventKind,
	)
	if err != nil || failures != policy.Pipeline.Retry.MaxAttempts {
		t.Fatalf("durable failure attempts=%d err=%v, want %d", failures, err, policy.Pipeline.Retry.MaxAttempts)
	}
	terminal, err := env.store.Events.CountBySubjectKind(
		ctx, "backlog_item", item.ID, bootstrapEscalatedEventKind,
	)
	if err != nil || terminal != 1 {
		t.Fatalf("terminal events=%d err=%v, want 1", terminal, err)
	}
}

// Auth/configuration failures must not waste retries that cannot repair them.
func TestReconciler_BootstrapPreflightTerminalFailureEscalatesImmediately(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{err: &fakeClassifiedRepoEnsureError{
		code: "project_create", retryable: false,
	}}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-TERMINAL", "services/hidden-project")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	decision, run, reason, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if decision != decisionSkipped || run != nil {
		t.Fatalf("decision=%s run=%v, want terminal skip", decision, run)
	}
	if !strings.Contains(reason, "code=project_create") || !strings.Contains(reason, "attempt=1/3") {
		t.Fatalf("reason=%q, want structured code and first attempt", reason)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil || got.State != store.BacklogEscalated {
		t.Fatalf("backlog=%+v err=%v, want escalated", got, err)
	}
	if len(ens.calls) != 1 || env.starter.calls() != 0 {
		t.Fatalf("EnsureRepo calls=%d starter calls=%d, want 1/0", len(ens.calls), env.starter.calls())
	}
}

func TestReconciler_BootstrapPreflightRetryBudgetIsPerTarget(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	env.rec.RepoEnsurer = &fakeRepoEnsurer{err: errors.New("gitlab 502")}
	ctx := context.Background()
	policy := bootstrapEnabledPolicy("services")
	policy.Pipeline.Retry.MaxAttempts = 3

	item := crossRepoItem("MILLS-BOOT-RETARGET", "services/old-target")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		current, err := env.store.Backlog.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("read old target: %v", err)
		}
		if decision, _, _, err := env.rec.tryStart(ctx, current, policy); err != nil || decision != decisionDeferred {
			t.Fatalf("old target attempt %d decision=%s err=%v, want deferred", attempt+1, decision, err)
		}
	}

	current, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("read for retarget: %v", err)
	}
	current.TargetProject = "services/new-target"
	if err := env.store.Backlog.Put(ctx, current); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	decision, _, reason, err := env.rec.tryStart(ctx, current, policy)
	if err != nil || decision != decisionDeferred {
		t.Fatalf("new target decision=%s err=%v, want fresh deferred budget", decision, err)
	}
	if !strings.Contains(reason, "attempt=1/3") {
		t.Fatalf("new target reason=%q, want attempt=1/3", reason)
	}
	got, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil || got.State != store.BacklogQueued {
		t.Fatalf("backlog=%+v err=%v, want queued after first new-target failure", got, err)
	}
}

// TestReconciler_BootstrapPreflightHomeItemNoop: an item targeting the home repo
// never triggers the pre-flight (the home repo always exists), even with
// bootstrap enabled.
func TestReconciler_BootstrapPreflightHomeItemNoop(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ens := &fakeRepoEnsurer{created: true}
	env.rec.RepoEnsurer = ens
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-HOME", "loom-core") // bare home name
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dec, run, _, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionStarted || run == nil {
		t.Errorf("expected decisionStarted for a home item, got dec=%v run=%v", dec, run)
	}
	if len(ens.calls) != 0 {
		t.Errorf("EnsureRepo must not run for a home-repo item, got %v", ens.calls)
	}
}

// TestReconciler_BootstrapPreflightNilEnsurerNoop: with no RepoEnsurer wired,
// bootstrap is inert — a cross-repo item runs normally (pre-bootstrap
// behavior).
func TestReconciler_BootstrapPreflightNilEnsurerNoop(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	// RepoEnsurer left nil.
	ctx := context.Background()

	item := crossRepoItem("MILLS-BOOT-NIL", "services/familyforge")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dec, run, _, err := env.rec.tryStart(ctx, item, bootstrapEnabledPolicy("services"))
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionStarted || run == nil {
		t.Errorf("expected decisionStarted with a nil ensurer, got dec=%v run=%v", dec, run)
	}
	if got := countBootstrapEvents(t, env); got != 0 {
		t.Errorf("no bootstrap events expected with a nil ensurer, got %d", got)
	}
}
