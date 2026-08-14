package mills

import (
	"context"
	"sync"
	"testing"

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

func TestScopeEnvelope_Overlaps(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []string
		want    bool
		witness string
	}{
		{
			name: "same directory different basenames",
			a:    []string{"pkg/mills/pipeline/escalate.go"},
			b:    []string{"pkg/mills/pipeline/runner.go"},
			want: true, witness: "pkg/mills/pipeline",
		},
		{
			name: "descendant directory",
			a:    []string{"pkg/mills/store/store.go"},
			b:    []string{"pkg/mills/store/migrations/011_x.sql"},
			want: true, witness: "pkg/mills/store",
		},
		{
			name: "ancestor directory (reversed)",
			a:    []string{"pkg/mills/store/migrations/011_x.sql"},
			b:    []string{"pkg/mills/store/store.go"},
			want: true, witness: "pkg/mills/store",
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
			name: "glob static prefix vs literal in same tree",
			a:    []string{"cmd/loom/*.go"},
			b:    []string{"cmd/loom/proxy_tool_filter.go"},
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
			a := envelopeForItem(itemWithFiles("A", tc.a...))
			b := envelopeForItem(itemWithFiles("B", tc.b...))
			got, witness := a.overlaps(b)
			if got != tc.want {
				t.Fatalf("overlaps=%v want %v (witness=%q)", got, tc.want, witness)
			}
			if tc.want && witness != tc.witness {
				t.Errorf("witness=%q want %q", witness, tc.witness)
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
// two queued items declaring files in the same package must not run
// concurrently — the second defers within the SAME tick that starts the
// first (tryStart persists state=running before the loop advances), and
// dispatches once the blocker leaves running.
func TestReconciler_DefersOnScopeOverlap(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()

	first := itemWithFiles("MILLS-OVL-A", "pkg/mills/pipeline/escalate.go")
	first.Priority = store.P1
	second := itemWithFiles("MILLS-OVL-B", "pkg/mills/pipeline/runner.go")
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
	second := itemWithFiles("MILLS-OVL-RACE-B", "pkg/mills/pipeline/runner.go")
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
	queued := itemWithFiles("MILLS-OFF-B", "pkg/mills/pipeline/runner.go")
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
	queued := itemWithFiles("MILLS-XR-B", "pkg/mills/pipeline/runner.go")
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
