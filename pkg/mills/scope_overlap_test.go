package mills

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func itemWithFiles(id string, files ...string) *store.BacklogItem {
	return &store.BacklogItem{
		ID:        id,
		Title:     id,
		State:     store.BacklogQueued,
		Priority:  store.P2,
		Slices:    []store.Slice{{Name: "s", Files: files}},
		Budget:    store.Budget{MaxCostUSD: 1.0, MaxTurns: 30},
		CreatedBy: "test",
	}
}

func TestScopeFairnessPolicy_Defaults(t *testing.T) {
	var p ScopeFairnessPolicy
	if !p.IsEnabled() || p.Deferrals() != 50 || p.Age() != 6*time.Hour || p.Hold() != 2*time.Hour {
		t.Fatalf("unexpected defaults: enabled=%v count=%d age=%s hold=%s", p.IsEnabled(), p.Deferrals(), p.Age(), p.Hold())
	}
}

func TestReconciler_ScopeFairnessReservationConvoy(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	blocker := itemWithFiles("RUNNING", "pkg/mills/pipeline/blocker.go")
	blocker.State = store.BacklogRunning
	starved := itemWithFiles("STARVED", "pkg/mills/pipeline/blocker.go")
	newer := itemWithFiles("NEWER", "pkg/mills/pipeline/blocker.go")
	for _, item := range []*store.BacklogItem{blocker, starved, newer} {
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	policy := env.policy.Current()
	policy.Pipeline.ScopeFairness.DeferralThreshold = 2
	policy.Pipeline.ScopeFairness.AgeThresholdHours = 24
	for i := 0; i < 2; i++ {
		decision, _, _, err := env.rec.tryStart(ctx, starved, policy)
		if err != nil || decision != decisionDeferred {
			t.Fatalf("deferral %d: decision=%v err=%v", i+1, decision, err)
		}
	}
	state, err := env.store.Backlog.ScopeFairness(ctx, starved.ID)
	if err != nil || state.DeferralCount != 2 || state.ReservedAt == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	decision, _, reason, err := env.rec.tryStart(ctx, newer, policy)
	if err != nil || decision != decisionDeferred || !strings.Contains(reason, blocker.ID) {
		t.Fatalf("newer decision=%v reason=%q err=%v", decision, reason, err)
	}
	blocker.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, blocker); err != nil {
		t.Fatal(err)
	}
	fresh, _ := env.store.Backlog.Get(ctx, starved.ID)
	decision, _, _, err = env.rec.tryStart(ctx, fresh, policy)
	if err != nil || decision != decisionStarted {
		t.Fatalf("reserved writer did not start: decision=%v err=%v", decision, err)
	}
	if _, err := env.store.Backlog.ScopeFairness(ctx, starved.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fairness state survived start: %v", err)
	}
}

func TestReconciler_ScopeFairnessExcludesIneligibleReservations(t *testing.T) {
	cases := []struct {
		name       string
		wantReason string
		configure  func(*testing.T, context.Context, *recTestEnv, *Policy, *store.BacklogItem)
	}{
		{
			name: "item human review", wantReason: "require_human_review",
			configure: func(_ *testing.T, _ context.Context, _ *recTestEnv, _ *Policy, item *store.BacklogItem) {
				item.Policy.RequireHumanReview = true
			},
		},
		{
			name: "repository human review", wantReason: "per_repo_require_human_review",
			configure: func(_ *testing.T, _ context.Context, _ *recTestEnv, policy *Policy, item *store.BacklogItem) {
				yes := true
				item.TargetProject = "services/flexdeck"
				policy.Pipeline.PerRepoOverrides = map[string]RepoExecutionOverride{
					"services/flexdeck": {RequireHumanReview: &yes},
				}
			},
		},
		{
			name: "unmet dependency", wantReason: "dependencies_unmet",
			configure: func(t *testing.T, ctx context.Context, env *recTestEnv, _ *Policy, item *store.BacklogItem) {
				dep := itemWithFiles("BLOCKED-DEP", "pkg/other/dep.go")
				if err := env.store.Backlog.Put(ctx, dep); err != nil {
					t.Fatal(err)
				}
				item.Dependencies = []string{dep.ID}
			},
		},
		{
			name: "repository budget exhausted", wantReason: "per_repo_budget_exhausted",
			configure: func(t *testing.T, ctx context.Context, env *recTestEnv, policy *Policy, item *store.BacklogItem) {
				env.rec.HomeProject = "services/loom-core"
				item.TargetProject = "services/flexdeck"
				policy.Pipeline.PerRepoOverrides = map[string]RepoExecutionOverride{
					"services/flexdeck": {MaxRunsPerDay: 1},
				}
				old := itemWithFiles("OLD-RUN", "pkg/other/old.go")
				old.State = store.BacklogMerged
				old.TargetProject = item.TargetProject
				if err := env.store.Backlog.Put(ctx, old); err != nil {
					t.Fatal(err)
				}
				if err := env.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: "PIPE-OLD", BacklogID: old.ID, Template: "test", State: store.PipelineDone, StartedAt: env.now.Add(-time.Hour)}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			policy := env.policy.Current()
			reserved := itemWithFiles("RESERVED", "docs/MILLS.md")
			tc.configure(t, ctx, env, policy, reserved)
			if err := env.store.Backlog.Put(ctx, reserved); err != nil {
				t.Fatal(err)
			}
			if _, tripped, err := env.store.Backlog.RecordScopeDeferral(ctx, reserved.ID, env.now, 1, time.Hour); err != nil || !tripped {
				t.Fatalf("reserve: tripped=%v err=%v", tripped, err)
			}
			contender := itemWithFiles("CONTENDER", "docs/MILLS.md")
			contender.TargetProject = reserved.TargetProject
			passCtx := withStarvedExclusionAudit(ctx)
			for i := 0; i < 2; i++ {
				blocker, _, err := env.rec.scopeReservationBlocker(passCtx, contender, policy, 2*time.Hour)
				if err != nil || blocker != "" {
					t.Fatalf("check %d: blocker=%q err=%v", i+1, blocker, err)
				}
			}
			events, err := env.store.Events.ListSince(ctx, time.Time{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			want := "starved_candidate_excluded:" + tc.wantReason
			matches := 0
			for _, event := range events {
				if event.Kind == "reconciler.skipped" && event.Payload["item"] == reserved.ID && event.Payload["reason"] == want {
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("matching skipped events=%d want 1 (reason %q)", matches, want)
			}
		})
	}
}

func TestScopeFairness_TimeBoundaryAndCapRestartAging(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now.UTC()
	item := itemWithFiles("AGED", "pkg/mills/store/a.go")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatal(err)
	}
	state, tripped, err := env.store.Backlog.RecordScopeDeferral(ctx, item.ID, now, 50, 6*time.Hour)
	if err != nil || tripped {
		t.Fatalf("first deferral state=%+v tripped=%v err=%v", state, tripped, err)
	}
	state, tripped, err = env.store.Backlog.RecordScopeDeferral(ctx, item.ID, now.Add(6*time.Hour), 50, 6*time.Hour)
	if err != nil || !tripped || state.ReservedAt == nil {
		t.Fatalf("age boundary state=%+v tripped=%v err=%v", state, tripped, err)
	}
	env.rec.Clock = func() time.Time { return now.Add(8 * time.Hour) }
	other := itemWithFiles("OTHER", "pkg/mills/store/b.go")
	if err := env.store.Backlog.Put(ctx, other); err != nil {
		t.Fatal(err)
	}
	blocker, _, err := env.rec.scopeReservationBlocker(ctx, other, env.policy.Current(), 2*time.Hour)
	if err != nil || blocker != "" {
		t.Fatalf("cap release blocker=%q err=%v", blocker, err)
	}
	if _, err := env.store.Backlog.ScopeFairness(ctx, item.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cap did not reset aging: %v", err)
	}
	state, _, err = env.store.Backlog.RecordScopeDeferral(ctx, item.ID, now.Add(8*time.Hour), 50, 6*time.Hour)
	if err != nil || !state.FirstDeferredAt.Equal(now.Add(8*time.Hour)) || state.DeferralCount != 1 {
		t.Fatalf("aging did not restart: %+v err=%v", state, err)
	}
}

func TestScopeEnvelope_Overlaps(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []string
		want    bool
		witness string
	}{
		{name: "cleaned literal", a: []string{" ./pkg/mills/../mills/policy.go "}, b: []string{"pkg/mills/policy.go"}, want: true, witness: "pkg/mills/policy.go"},
		{name: "policy and spin", a: []string{"pkg/mills/policy.go"}, b: []string{"pkg/mills/spin/spin.go"}, want: false, witness: ""},
		{name: "pipeline glob", a: []string{"pkg/mills/pipeline/*.go"}, b: []string{"pkg/mills/pipeline/dispatcher.go"}, want: true, witness: "pkg/mills/pipeline"},
		{name: "glob below literal directory", a: []string{"pkg/mills/pipeline/*.go"}, b: []string{"pkg/mills/policy.go"}, want: true, witness: "pkg/mills/pipeline"},
		{name: "glob ancestor", a: []string{"pkg/mills/**"}, b: []string{"pkg/mills/pipeline/*.go"}, want: true, witness: "pkg/mills"},
		{name: "segment boundary", a: []string{"pkg/mills/*.go"}, b: []string{"pkg/mills-extra/main.go"}, want: false, witness: ""},
		{name: "root glob", a: []string{"./*.go"}, b: []string{"pkg/mills/policy.go"}, want: false, witness: ""},
		{
			name: "same directory different basenames",
			a:    []string{"pkg/mills/pipeline/escalate.go"},
			b:    []string{"pkg/mills/pipeline/runner.go"},
			want: false,
		},
		{
			name: "descendant directory",
			a:    []string{"pkg/mills/store/store.go"},
			b:    []string{"pkg/mills/store/migrations/011_x.sql"},
			want: false,
		},
		{
			name: "ancestor directory (reversed)",
			a:    []string{"pkg/mills/store/migrations/011_x.sql"},
			b:    []string{"pkg/mills/store/store.go"},
			want: false,
		},
		{
			name: "disjoint packages",
			a:    []string{"pkg/mills/pipeline/escalate.go"},
			b:    []string{"internal/hud/spawn.go"},
			want: false,
		},
		{
			name: "sibling directories with shared parent do not overlap",
			a:    []string{"pkg/mills/council/brief.go"},
			b:    []string{"pkg/mills/pipeline/runner.go"},
			want: false,
		},
		{
			name: "repo-root same file",
			a:    []string{"Makefile"},
			b:    []string{"Makefile"},
			want: true, witness: "Makefile",
		},
		{
			name: "repo-root different files",
			a:    []string{"Makefile"},
			b:    []string{"Dockerfile"},
			want: false,
		},
		{
			name: "glob static prefix reserves literal file",
			a:    []string{"docs/*.md"},
			b:    []string{"docs/MILLS.md"},
			want: true, witness: "docs",
		},
		{
			name: "identical non-exempt globs overlap",
			a:    []string{"cmd/loom/*.go"},
			b:    []string{"cmd/loom/*.go"},
			want: true, witness: "cmd/loom",
		},
		{
			name: "rootless glob never matches",
			a:    []string{"*.go"},
			b:    []string{"pkg/mills/pipeline/runner.go"},
			want: false,
		},
		{
			name: "changelog.d globs never collide",
			a:    []string{"changelog.d/*.md"},
			b:    []string{"changelog.d/*.md"},
			want: false,
		},
		{
			name: "changelog.d literal fragments with distinct slugs do not overlap",
			a:    []string{"changelog.d/feat-a.added.md"},
			b:    []string{"changelog.d/fix-b.fixed.md"},
			want: false,
		},
		{
			name: "identical changelog.d literal fragment still collides",
			a:    []string{"changelog.d/same-slug.fixed.md"},
			b:    []string{"changelog.d/same-slug.fixed.md"},
			want: true, witness: "changelog.d/same-slug.fixed.md",
		},
		{
			name: "disjoint code scopes with shared changelog glob do not overlap",
			a:    []string{"pkg/mills/escalation_sweeper.go", "changelog.d/*.md"},
			b:    []string{".gitlab-ci.yml", "changelog.d/*.md"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, pair := range [][2][]string{{tc.a, tc.b}, {tc.b, tc.a}} {
				left, right := itemWithFiles("A", pair[0]...), itemWithFiles("B", pair[1]...)
				hit, witness := store.BacklogScopesOverlap(left, right, "")
				if hit != tc.want || witness != tc.witness {
					t.Fatalf("store overlap=(%v, %q), want (%v, %q)", hit, witness, tc.want, tc.witness)
				}
				hit, witness = envelopeForItem(left).overlaps(envelopeForItem(right))
				if hit != tc.want || witness != tc.witness {
					t.Fatalf("envelope overlap=(%v, %q), want (%v, %q)", hit, witness, tc.want, tc.witness)
				}
			}
		})
	}
}

func TestScopeEnvelope_EmptyNeverBlocks(t *testing.T) {
	empty := envelopeForItem(&store.BacklogItem{ID: "CANARY"})
	full := envelopeForItem(itemWithFiles("A", "pkg/mills/pipeline/runner.go"))
	if !empty.empty() {
		t.Fatal("slice-less item should have an empty envelope")
	}
	if hit, _ := empty.overlaps(full); hit {
		t.Error("empty envelope must not overlap anything")
	}
	if hit, _ := full.overlaps(empty); hit {
		t.Error("nothing may overlap an empty envelope")
	}
}

func TestGlobStaticDir(t *testing.T) {
	cases := map[string]string{
		"cmd/*.go":           "cmd",
		"pkg/mills/**":       "pkg/mills",
		"a/b?.go":            "a",
		"*.go":               "",
		"?":                  "",
		"pkg/[ab]/thing.go":  "pkg",
		"pkg/mills/x/*.yaml": "pkg/mills/x",
	}
	for pat, want := range cases {
		if got := globStaticDir(pat); got != want {
			t.Errorf("globStaticDir(%q)=%q want %q", pat, got, want)
		}
	}
}

func TestSerializeOverlappingScopesEnabled_Defaults(t *testing.T) {
	var p PipelinePolicy
	if !p.SerializeOverlappingScopesEnabled() {
		t.Error("nil must default to enabled")
	}
	off := false
	p.SerializeOverlappingScopes = &off
	if p.SerializeOverlappingScopesEnabled() {
		t.Error("explicit false must disable")
	}
	on := true
	p.SerializeOverlappingScopes = &on
	if !p.SerializeOverlappingScopesEnabled() {
		t.Error("explicit true must enable")
	}
}

// TestReconciler_DefersOnScopeOverlap pins the dispatch guard end-to-end:
// two queued items declaring the same file must not run
// concurrently — the second defers within the SAME tick that starts the
// first (tryStart persists state=running before the loop advances), and
// dispatches once the blocker leaves running.
func TestReconciler_DefersOnScopeOverlap(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	first := itemWithFiles("MILLS-OVL-A", "pkg/mills/pipeline/escalate.go")
	first.Priority = store.P1
	second := itemWithFiles("MILLS-OVL-B", "pkg/mills/pipeline/escalate.go")
	if err := env.store.Backlog.Put(ctx, first); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := env.store.Backlog.Put(ctx, second); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 || res.Deferred != 1 {
		t.Fatalf("want 1 started + 1 deferred, got %+v", res)
	}
	gotA, _ := env.store.Backlog.Get(ctx, first.ID)
	if gotA.State != store.BacklogRunning {
		t.Errorf("A should be running, got %v", gotA.State)
	}
	gotB, _ := env.store.Backlog.Get(ctx, second.ID)
	if gotB.State != store.BacklogQueued {
		t.Errorf("B should stay queued, got %v", gotB.State)
	}

	// Blocker lands → next tick dispatches the deferred sibling.
	gotA.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, gotA); err != nil {
		t.Fatalf("merge A: %v", err)
	}
	res, err = env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res.Started != 1 {
		t.Fatalf("want B started after A merged, got %+v", res)
	}
	gotB, _ = env.store.Backlog.Get(ctx, second.ID)
	if gotB.State != store.BacklogRunning {
		t.Errorf("B should be running after blocker merged, got %v", gotB.State)
	}
}

func TestReconciler_ConcurrentDistinctOverlappingStartsHaveOneWinner(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	first := itemWithFiles("MILLS-OVL-RACE-A", "pkg/mills/pipeline/escalate.go")
	second := itemWithFiles("MILLS-OVL-RACE-B", "pkg/mills/pipeline/escalate.go")
	for _, item := range []*store.BacklogItem{first, second} {
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	start := make(chan struct{})
	results := make(chan StartQueuedResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{first.ID, second.ID} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := env.rec.StartQueuedItem(ctx, id)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent start: %v", err)
		}
	}
	started, deferred := 0, 0
	for result := range results {
		switch result.Decision {
		case "started":
			started++
		case "deferred":
			deferred++
		default:
			t.Fatalf("unexpected decision: %+v", result)
		}
	}
	if started != 1 || deferred != 1 || env.starter.calls() != 1 {
		t.Fatalf("race outcomes started=%d deferred=%d starter_calls=%d",
			started, deferred, env.starter.calls())
	}
	var runs, transitions int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_transitions`).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if runs != 1 || transitions != 1 {
		t.Fatalf("race persisted runs=%d transitions=%d want 1/1", runs, transitions)
	}
}

func TestReconciler_DisjointScopesRunConcurrently(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	a := itemWithFiles("MILLS-DIS-A", "pkg/mills/pipeline/escalate.go")
	b := itemWithFiles("MILLS-DIS-B", "internal/hud/spawn.go")
	for _, it := range []*store.BacklogItem{a, b} {
		if err := env.store.Backlog.Put(ctx, it); err != nil {
			t.Fatalf("seed %s: %v", it.ID, err)
		}
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 2 {
		t.Fatalf("disjoint scopes must both start, got %+v", res)
	}
}

func TestReconciler_ScopeOverlapPolicyOptOut(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	running := itemWithFiles("MILLS-OFF-A", "pkg/mills/pipeline/escalate.go")
	running.State = store.BacklogRunning
	if err := env.store.Backlog.Put(ctx, running); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	queued := itemWithFiles("MILLS-OFF-B", "pkg/mills/pipeline/escalate.go")
	if err := env.store.Backlog.Put(ctx, queued); err != nil {
		t.Fatalf("seed queued: %v", err)
	}

	// writePolicyYAMLForTest pins fixtureV1, so exercise the opt-out through
	// tryStart's policy argument directly (tests are in-package). Copy the
	// manager's policy rather than mutating its live pointer.
	policy := *env.policy.Current()
	off := false
	policy.Pipeline.SerializeOverlappingScopes = &off
	decision, _, _, err := env.rec.tryStart(ctx, queued, &policy)
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if decision != decisionStarted {
		t.Fatalf("opt-out must start despite overlap, got %v", decision)
	}
}

func TestReconciler_ScopeOverlapIgnoresOtherRepos(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	running := itemWithFiles("MILLS-XR-A", "pkg/mills/pipeline/escalate.go")
	running.State = store.BacklogRunning
	running.TargetProject = "services/flexdeck"
	if err := env.store.Backlog.Put(ctx, running); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	queued := itemWithFiles("MILLS-XR-B", "pkg/mills/pipeline/escalate.go")
	if err := env.store.Backlog.Put(ctx, queued); err != nil {
		t.Fatalf("seed queued: %v", err)
	}
	env.rec.HomeProject = "services/loom-core"

	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 || res.Deferred != 0 {
		t.Fatalf("same paths in different repos must not serialize, got %+v", res)
	}
}

func TestReconciler_ScopeFairnessPriorityMatrix(t *testing.T) {
	for _, priority := range []store.Priority{store.P0, store.P1, store.P2, store.P3} {
		t.Run(string(priority), func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			reserved := itemWithFiles("reserved", "pkg/shared/a.go")
			candidate := itemWithFiles("candidate", "pkg/shared/a.go")
			candidate.Priority = priority
			for _, item := range []*store.BacklogItem{reserved, candidate} {
				if err := env.store.Backlog.Put(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := env.store.Backlog.RecordScopeDeferral(ctx, reserved.ID, env.now, 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			decision, _, reason, err := env.rec.tryStart(ctx, candidate, env.policy.Current())
			want := decisionDeferred
			if priority < store.P2 {
				want = decisionStarted
			}
			if err != nil || decision != want {
				t.Fatalf("decision=%v want=%v reason=%s err=%v", decision, want, reason, err)
			}
			events, err := env.store.Events.ListBySubject(ctx, "backlog_item", candidate.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			kind := "reconciler.deferred"
			if priority < store.P2 {
				kind = "reconciler.scope_reservation_override"
			}
			found := false
			for _, event := range events {
				if event.Kind == kind {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s on candidate", kind)
			}
		})
	}
}

func TestReconciler_ScopeFairnessBlockedReserverRelief(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	active := itemWithFiles("active", "pkg/a/active.go")
	active.State = store.BacklogRunning
	reserved := itemWithFiles("reserved", "pkg/a/active.go", "pkg/c/reserved.go")
	candidate := itemWithFiles("candidate", "pkg/c/reserved.go")
	for _, item := range []*store.BacklogItem{active, reserved, candidate} {
		if err := env.store.Backlog.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	before, _, err := env.store.Backlog.RecordScopeDeferral(ctx, reserved.ID, env.now, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	blocker, _, err := env.rec.scopeReservationBlocker(ctx, candidate, env.policy.Current(), 2*time.Hour)
	if err != nil || blocker != "" {
		t.Fatalf("blocker=%s err=%v", blocker, err)
	}
	after, err := env.store.Backlog.ScopeFairness(ctx, reserved.ID)
	if err != nil || after.DeferralCount != before.DeferralCount || !after.FirstDeferredAt.Equal(before.FirstDeferredAt) || !after.ReservedAt.Equal(*before.ReservedAt) {
		t.Fatalf("aging changed: before=%+v after=%+v err=%v", before, after, err)
	}
	active.State = store.BacklogMerged
	if err := env.store.Backlog.Put(ctx, active); err != nil {
		t.Fatal(err)
	}
	blocker, _, err = env.rec.scopeReservationBlocker(ctx, candidate, env.policy.Current(), 2*time.Hour)
	if err != nil || blocker != reserved.ID {
		t.Fatalf("reservation did not reactivate: blocker=%s err=%v", blocker, err)
	}
	active.State = store.BacklogRunning
	if err := env.store.Backlog.Put(ctx, active); err != nil {
		t.Fatal(err)
	}
	decision, _, reason, err := env.rec.tryStart(ctx, candidate, env.policy.Current())
	if err != nil || decision != decisionStarted {
		t.Fatalf("convoy: decision=%v reason=%s err=%v", decision, reason, err)
	}
}

func TestReconciler_ScopeReservationOldestWins(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count          int
		tieReservation bool
		tieCreation    bool
		highPriority   bool
	}{
		{name: "two reservations", count: 2},
		{name: "younger higher priority", count: 2, highPriority: true},
		{name: "three reservations", count: 3},
		{name: "creation breaks reservation tie", count: 3, tieReservation: true},
		{name: "ID breaks timestamp ties", count: 3, tieReservation: true, tieCreation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			items := make([]*store.BacklogItem, tc.count)
			for i := range items {
				// Reverse IDs and creation age so reservation order must win.
				id := []string{"C", "B", "A"}[i]
				if tc.tieCreation {
					id = []string{"A", "B", "C"}[i]
				}
				item := itemWithFiles(id, "pkg/shared/a.go")
				if tc.highPriority && i == 1 {
					item.Priority = store.P0
				}
				item.CreatedAt = env.now.Add(-time.Duration(i+1) * time.Hour)
				if tc.tieReservation {
					item.CreatedAt = env.now.Add(time.Duration(i-4) * time.Hour)
				}
				if tc.tieCreation {
					item.CreatedAt = env.now.Add(-4 * time.Hour)
				}
				if err := env.store.Backlog.Put(ctx, item); err != nil {
					t.Fatal(err)
				}
				reservedAt := env.now.Add(time.Duration(i-4) * time.Minute)
				if tc.tieReservation {
					reservedAt = env.now.Add(-4 * time.Minute)
				}
				if _, tripped, err := env.store.Backlog.RecordScopeDeferral(ctx, item.ID, reservedAt, 1, time.Hour); err != nil || !tripped {
					t.Fatalf("reserve %s: tripped=%v err=%v", item.ID, tripped, err)
				}
				items[i] = item
			}
			if tc.highPriority {
				items[0], items[1] = items[1], items[0]
			}
			for i, item := range items {
				want := items[0].ID
				if i == 0 {
					want = ""
				}
				blocker, _, err := env.rec.scopeReservationBlocker(ctx, item, env.policy.Current(), 2*time.Hour)
				if err != nil || blocker != want {
					t.Fatalf("%s: blocker=%q want=%q err=%v", item.ID, blocker, want, err)
				}
			}
			for _, item := range items[1:] {
				events, err := env.store.Events.ListSince(ctx, time.Time{}, 100)
				if err != nil {
					t.Fatal(err)
				}
				matches := 0
				for _, event := range events {
					if event.Kind == "reconciler.scope_reservation_yield" && event.Payload["item"] == item.ID {
						if event.Payload["item"] != item.ID || event.Payload["yields_to"] != items[0].ID || event.Payload["shared_scope"] != "pkg/shared/a.go" {
							t.Fatalf("unexpected yield: %+v", event.Payload)
						}
						matches++
					}
				}
				if matches != len(items) {
					t.Fatalf("yield events=%d want=%d", matches, len(items))
				}
			}
			// Exercise the transaction with the actual reconciler winner, including
			// the reservation read after its provisional RUNNING CAS.
			winner := items[0]
			_, err := env.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
				BacklogID: winner.ID, ExpectedRevision: winner.Revision,
				ExpectedClaimVersion:       winner.ClaimVersion,
				SerializeOverlappingScopes: true, EnforceScopeReservations: true,
				HomeProject: env.rec.HomeProject, Template: "mills-default-pipeline", Now: env.now,
			})
			if err != nil {
				t.Fatalf("reconciler winner rejected by store: %v", err)
			}

		})
	}
}

func TestReconciler_ScopeReservationSuppressionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files       []string
		target      string
		wantBlocker string
	}{
		{name: "suppressed envelope cannot block third party", files: []string{"pkg/a/a.go", "pkg/b/b.go"}},
		{name: "separate scopes stay eligible", files: []string{"pkg/b/b.go"}, wantBlocker: "younger"},
		{name: "separate targets stay eligible", files: []string{"pkg/a/a.go", "pkg/b/b.go"}, target: "services/other", wantBlocker: "younger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			env.rec.HomeProject = "services/loom-core"
			older := itemWithFiles("older", "pkg/a/a.go")
			younger := itemWithFiles("younger", tc.files...)
			younger.TargetProject = tc.target
			for i, item := range []*store.BacklogItem{older, younger} {
				if err := env.store.Backlog.Put(ctx, item); err != nil {
					t.Fatal(err)
				}
				if _, tripped, err := env.store.Backlog.RecordScopeDeferral(ctx, item.ID, env.now.Add(time.Duration(i-2)*time.Minute), 1, time.Hour); err != nil || !tripped {
					t.Fatalf("reserve: tripped=%v err=%v", tripped, err)
				}
			}
			candidate := itemWithFiles("candidate", "pkg/b/b.go")
			candidate.TargetProject = tc.target
			blocker, _, err := env.rec.scopeReservationBlocker(ctx, candidate, env.policy.Current(), 2*time.Hour)
			if err != nil || blocker != tc.wantBlocker {
				t.Fatalf("blocker=%q want=%q err=%v", blocker, tc.wantBlocker, err)
			}
			events, err := env.store.Events.ListSince(ctx, time.Time{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if tc.wantBlocker != "" && event.Kind == "reconciler.scope_reservation_yield" {
					t.Fatalf("independent reservation yielded: %+v", event.Payload)
				}
			}
		})
	}
}

func TestReconciler_SiblingFileScopesRunConcurrently(t *testing.T) {
	for _, other := range []string{"pkg/mills/other.go", "pkg/mills/spin/spin.go"} {
		t.Run(other, func(t *testing.T) {
			env := newRecEnv(t, nil)
			ctx := context.Background()
			for _, item := range []*store.BacklogItem{
				itemWithFiles("A", "pkg/mills/policy.go"),
				itemWithFiles("B", other),
			} {
				if err := env.store.Backlog.Put(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			result, err := env.rec.Tick(ctx)
			if err != nil || result.Started != 2 || result.Deferred != 0 {
				t.Fatalf("independent files: result=%+v err=%v", result, err)
			}
		})
	}
}

// These are the five queued examples named in the September 14 operator
// evidence, checked against the running files explicitly identified there.
// The complete running envelope and eight other queued envelopes were not
// supplied, so this is a reduced regression, not a 13-item snapshot replay.
func TestScopeEnvelope_ReportedIncidentExamples(t *testing.T) {
	running := itemWithFiles("bl-devbox-sandbox-quota-headroom-20260913",
		"pkg/mills/pipeline/dispatcher.go", "pkg/mills/pipeline/error_class.go")
	cases := []struct {
		id      string
		files   []string
		witness string
	}{
		{"bl-mills-spinning-room-cross-vendor-hop-20260914", []string{"cmd/loom-mills-operator/main.go", "pkg/mills/policy.go", "pkg/mills/spin/spin.go"}, ""},
		{"bl-hud-spawn-auth-state-and-billing-artifacts-20260914", []string{"internal/spawn/*.go", "pkg/mills/clients/spawn.go"}, ""},
		{"bl-mills-autonomy-breaker-hold-on-transient-capability-red-20260914", []string{"pkg/mills/pipeline/autonomy_gate.go"}, ""},
		{"bl-mills-ciwatch-reattach-running-pipeline-20260906", []string{"pkg/mills/pipeline/dispatcher.go", "pkg/mills/pipeline/error_class.go"}, "pkg/mills/pipeline/dispatcher.go"},
		{"bl-mills-lintparity-empty-output-tail-20260910", []string{"pkg/mills/pipeline/*.go"}, "pkg/mills/pipeline"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			queued := itemWithFiles(tc.id, tc.files...)
			for _, pair := range [][2]*store.BacklogItem{{running, queued}, {queued, running}} {
				hit, witness := store.BacklogScopesOverlap(pair[0], pair[1], "")
				if hit != (tc.witness != "") || witness != tc.witness {
					t.Fatalf("store overlap=(%v, %q), want witness %q", hit, witness, tc.witness)
				}
				hit, witness = envelopeForItem(pair[0]).overlaps(envelopeForItem(pair[1]))
				if hit != (tc.witness != "") || witness != tc.witness {
					t.Fatalf("envelope overlap=(%v, %q), want witness %q", hit, witness, tc.witness)
				}
			}
		})
	}
}

func TestScopeEnvelopeFiltersTestCommands(t *testing.T) {
	for _, command := range []string{"go test ./pkg/mills/ -run 'Scope|Envelope'", "cd internal/hud/frontend && pnpm test", "pnpm test", "npm test", "make test", "go\ttest", "go\ntest", "go\u00a0test", "a&&b", "a|b", "a;b", "a>b", ""} {
		t.Run(command, func(t *testing.T) {
			item := &store.BacklogItem{Slices: []store.Slice{{Tests: []string{command}}}}
			if got := envelopeForItem(item); !got.empty() {
				t.Fatalf("command contributed scope: %+v", got)
			}
			left := &store.BacklogItem{Slices: []store.Slice{{Files: []string{"pkg/alpha/a.go"}, Tests: []string{command}}}}
			right := &store.BacklogItem{Slices: []store.Slice{{Files: []string{"pkg/beta/b.go"}, Tests: []string{command}}}}
			a, b := envelopeForItem(left), envelopeForItem(right)
			if hit, witness := a.overlaps(b); hit || witness != "" {
				t.Fatalf("command overlap: %v %q", hit, witness)
			}
		})
	}
}

func TestScopeEnvelopePreservesTestDeclarations(t *testing.T) {
	item := &store.BacklogItem{Slices: []store.Slice{{Files: []string{"dir with spaces/file.go"}, Tests: []string{"pkg/alpha/a_test.go", "pkg/beta/*_test.go"}}}}
	got := envelopeForItem(item)
	for _, file := range []string{"dir with spaces/file.go", "pkg/alpha/a_test.go"} {
		if _, ok := got.files[file]; !ok {
			t.Errorf("missing literal %q", file)
		}
	}
	if _, ok := got.literalDirs["pkg/alpha"]; !ok {
		t.Error("missing test directory")
	}
	if _, ok := got.globDirs["pkg/beta"]; !ok {
		t.Error("missing test glob directory")
	}
}
