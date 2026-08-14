package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// The token-sweep fixture, verbatim from the 2026-07-26 escalation the
// amendment was written for: the item declared only components/mills, the
// implementer necessarily restyled the shared components it renders through.
const (
	declaredMillsComponent = "internal/hud/frontend/src/lib/components/mills/MillsPipelineDrawer.svelte"
	sharedPanelShell       = "internal/hud/frontend/src/lib/components/shared/PanelShell.svelte"
	sharedEmptyState       = "internal/hud/frontend/src/lib/components/shared/EmptyState.svelte"
	// A protected path (pipeline.protected_paths ships "platform/gitops/**"),
	// so the amendment refuses it no matter how the ancestor rule reads.
	crossTreeGitops = "platform/gitops/k3s/mills/configmap-policy.yaml"
)

// scopeGateOnly registers ONLY the real scope gate, so post_implement_gate's
// verdict is scope's alone and gateVerdict.scopeOnlyFailure() holds. The other
// gates named by DefaultStages are unregistered and therefore skipped.
func scopeGateOnly(t *testing.T) *gates.Registry {
	t.Helper()
	r := gates.NewRegistry()
	r.Register(&gates.Scope{})
	return r
}

// seedSlices declares an item's slice scope and re-persists it, echoing the CAS
// revision so the runner's own amendment write sees a current row.
func seedSlices(t *testing.T, st *store.Store, item *store.BacklogItem, files ...string) {
	t.Helper()
	item.Slices = []store.Slice{{Name: "token-sweep", Files: files}}
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed slices: %v", err)
	}
}

func countCalls(calls []string, stage string) int {
	n := 0
	for _, c := range calls {
		if c == stage {
			n++
		}
	}
	return n
}

func eventKinds(t *testing.T, st *store.Store) []*store.Event {
	t.Helper()
	evs, err := st.Events.ListSince(context.Background(), time.Time{}, 500)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return evs
}

// The headline S1 contract: a scope-only failure whose violations are all
// admissible amends the item's declared scope and CONTINUES on the existing
// diff. No rewind, no second implement spawn — which is the whole point, since
// a respawned implementer correctly reaches for the same necessary files again
// (token-sweep re-edited the identical two files on every attempt).
func TestRunner_ScopeAmendmentProceedsWithoutRespawn(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	seedSlices(t, st, item, declaredMillsComponent)

	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:        0.10,
			FilesChanged:   []string{declaredMillsComponent, sharedPanelShell, sharedEmptyState},
			LinesAdded:     20,
			DiffPatch:      []byte("diff --git a/x b/x\n+x\n"),
			CommitMessages: []string{"feat: token sweep"},
		},
		"mr":    {MRIID: 77},
		"merge": {MergedSHA: "deadbeef"},
	}}
	r := New(st, scopeGateOnly(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 7, 26, 20, 23, 0, 0, time.UTC) }

	// The rollout measurement: decision="admitted" is what the 24h soak reads
	// against the KPI escalation rate. Measured as a delta because the counter
	// is process-global.
	before := testutil.ToFloat64(mills.ScopeAmendmentsTotal.WithLabelValues("admitted"))
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if after := testutil.ToFloat64(mills.ScopeAmendmentsTotal.WithLabelValues("admitted")); after != before+1 {
		t.Errorf("mills_scope_amendments_total{admitted} = %v, want %v", after, before+1)
	}

	if n := countCalls(disp.callsList(), "implement"); n != 1 {
		t.Errorf("implement dispatched %d times, want 1 (amend-and-proceed must not respawn)", n)
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("run state = %s, want done", got.State)
	}

	// The widened scope must be DURABLE: the next run / requeue of this item
	// inherits it, which is what stops the same reach re-escalating.
	reloaded, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if len(reloaded.Slices) != 1 {
		t.Fatalf("slices = %+v, want 1", reloaded.Slices)
	}
	for _, want := range []string{sharedPanelShell, sharedEmptyState} {
		found := false
		for _, f := range reloaded.Slices[0].Files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("amended slice files %v missing %q", reloaded.Slices[0].Files, want)
		}
	}

	// Both gate rows are true and both must be persisted: the gate failed on
	// the authored envelope and passed on the amended one.
	rows, err := st.Pipeline.ListGates(ctx, run.ID)
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}
	var sawFail, sawAmendedPass bool
	for _, g := range rows {
		if g.GateName != "scope" {
			continue
		}
		switch g.Outcome {
		case store.GateOutcomeFail:
			sawFail = true
		case store.GateOutcomePass:
			if strings.HasPrefix(strings.Join(g.Reasons, " "), "scope amended: ") {
				sawAmendedPass = true
			}
		}
	}
	if !sawFail {
		t.Error("expected the original scope fail row to survive for audit")
	}
	if !sawAmendedPass {
		t.Errorf("expected a 'scope amended: …' pass row, got %+v", rows)
	}

	var sawEvent bool
	for _, e := range eventKinds(t, st) {
		if e.Kind == "pipeline.gate.scope_amended" {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Error("expected a pipeline.gate.scope_amended event")
	}
}

// The gate must stay strict for a cross-tree reach: one rewind-retry (not the
// full maxAttempts budget), then escalate with the S2 hygiene — a [class=config]
// marker so the escalation is classified and auto-requeue eligible, plus the
// structured artifact. A rescue-MR failure must NOT mask any of it.
func TestRunner_ScopeEscalationHygiene(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	seedSlices(t, st, item, declaredMillsComponent)

	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:      0.10,
			FilesChanged: []string{declaredMillsComponent, crossTreeGitops},
			DiffPatch:    []byte("diff --git a/x b/x\n+x\n"),
		},
	}}
	r := New(st, scopeGateOnly(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 7, 26, 20, 23, 0, 0, time.UTC) }
	rescueCalls := 0
	r.RescueMR = func(context.Context, *store.PipelineRun, *store.BacklogItem, CreateMRRequest) (CreateMRResponse, error) {
		rescueCalls++
		return CreateMRResponse{}, errors.New("gitlab: 403 forbidden")
	}

	before := testutil.ToFloat64(mills.ScopeAmendmentsTotal.WithLabelValues("refused"))
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	// Both gate evaluations (original + the one retry) run the evaluator, so
	// the refusal is counted twice — the metric measures evaluations, not runs.
	if after := testutil.ToFloat64(mills.ScopeAmendmentsTotal.WithLabelValues("refused")); after != before+2 {
		t.Errorf("mills_scope_amendments_total{refused} = %v, want %v", after, before+2)
	}

	// Exactly one self-correction respawn — NOT the 3 the generic gate cap
	// would have spent on a verdict the amendment already proved will not move.
	if n := countCalls(disp.callsList(), "implement"); n != maxScopeRetryAttempts {
		t.Errorf("implement dispatched %d times, want %d", n, maxScopeRetryAttempts)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated", got.State)
	}
	if got.EscalationClass != string(ClassConfig) {
		t.Errorf("escalation_class = %q, want %q (the marker the pre-slice reason omitted)",
			got.EscalationClass, ClassConfig)
	}
	if rescueCalls != 1 {
		t.Errorf("rescue MR attempted %d times, want 1", rescueCalls)
	}

	// The escalation reason must carry the class marker and the file list.
	reason := escalationReasonFromEvents(t, st)
	if !strings.Contains(reason, "[class=config]") {
		t.Errorf("escalation reason %q missing [class=config]", reason)
	}
	if !strings.Contains(reason, crossTreeGitops) {
		t.Errorf("escalation reason %q missing the violating file", reason)
	}
	if strings.Contains(reason, "rescue draft MR") {
		t.Errorf("a failed rescue MR must not be advertised: %q", reason)
	}

	// Structured evidence, so the HUD/CLI can offer widen-and-requeue without
	// parsing the truncated prose.
	decision := scopeViolationsArtifactFrom(t, st, run.ID)
	if decision["admitted"] != false {
		t.Errorf("artifact admitted = %v, want false", decision["admitted"])
	}
	verdicts, _ := decision["verdicts"].([]any)
	if len(verdicts) != 1 {
		t.Fatalf("artifact verdicts = %v, want 1", decision["verdicts"])
	}
	v, _ := verdicts[0].(map[string]any)
	if v["file"] != crossTreeGitops {
		t.Errorf("artifact verdict file = %v, want %q", v["file"], crossTreeGitops)
	}
	if v["rule"] != gates.AmendRuleSensitivePath {
		t.Errorf("artifact verdict rule = %v, want %q", v["rule"], gates.AmendRuleSensitivePath)
	}
	if dirs, _ := decision["declared_dirs"].([]any); len(dirs) == 0 {
		t.Errorf("artifact must record the declared dirs it measured against: %v", decision)
	}
}

// A successful rescue MR must be a Draft, must NOT be auto-merge-armed, and
// must be named in the escalation reason so the operator can find the diff.
func TestRunner_ScopeEscalationOpensDraftRescueMR(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	seedSlices(t, st, item, declaredMillsComponent)

	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {FilesChanged: []string{declaredMillsComponent, crossTreeGitops}},
	}}
	r := New(st, scopeGateOnly(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 7, 26, 20, 23, 0, 0, time.UTC) }
	var seen CreateMRRequest
	r.RescueMR = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, req CreateMRRequest) (CreateMRResponse, error) {
		seen = req
		return CreateMRResponse{MRIID: 1249}, nil
	}

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	if !strings.HasPrefix(seen.Title, "Draft: [scope-escalated] ") {
		t.Errorf("rescue MR title = %q, want the Draft prefix", seen.Title)
	}
	if seen.AutoMerge {
		t.Error("rescue MR must never be auto-merge-armed")
	}
	if seen.SourceBranch != BranchContractFor(run, item, Stage{}, "").SourceBranch {
		t.Errorf("rescue MR source branch = %q, want the run's contract branch", seen.SourceBranch)
	}
	if !strings.Contains(seen.Description, crossTreeGitops) {
		t.Errorf("rescue MR body must table the violations, got:\n%s", seen.Description)
	}
	if !strings.Contains(seen.Description, gates.AmendRuleSensitivePath) {
		t.Errorf("rescue MR body must name the refusal rule, got:\n%s", seen.Description)
	}
	if reason := escalationReasonFromEvents(t, st); !strings.Contains(reason, "rescue draft MR !1249") {
		t.Errorf("escalation reason %q must name the rescue MR", reason)
	}
}

// A NON-scope gate cap must not gain a blanket class MARKER — the original
// concern this test was written for is that forcing a class onto a judge-gate
// cap would misroute auto-requeue.
//
// That concern is now satisfied differently. The escalation no longer persists
// a NULL class (which bucketed as "unclassified" and, being invisible, could
// never be acted on); its evidence is classified instead. The safety property
// the test really cares about is preserved and is asserted directly below: the
// resulting class must NOT be auto-requeue eligible. Unrecognized gate-failure
// prose lands on Classify's conservative ClassCode default, and
// autoRequeueBaseClass admits only infra/transient/transient_quota — so the
// run is attributable without becoming retryable.
func TestRunner_NonScopeGateCapClassUnchanged(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	gr := gates.NewRegistry()
	gr.Register(&alwaysFailGate{name: "diff_size"})
	r := New(st, gr, &fakeDispatcher{}, nil)

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated", got.State)
	}
	// THE SAFETY PROPERTY: attributable, but never auto-requeue eligible.
	if autoRequeueEligibleClass(got.EscalationClass) {
		t.Errorf("non-scope gate cap escalation_class = %q, which IS auto-requeue eligible — "+
			"a judge-gate cap must never be retried around", got.EscalationClass)
	}
	if got.EscalationClass == "" {
		t.Error("non-scope gate cap must carry an attributable class; an empty class buckets as " +
			"\"unclassified\" and is invisible to both the KPI breakdown and the requeue sweep")
	}
	// No blanket marker is added to the reason format: the class comes from
	// classifying the evidence, not from stamping every gate-cap reason.
	if reason := escalationReasonFromEvents(t, st); strings.Contains(reason, "[class=") {
		t.Errorf("non-scope gate cap reason must not gain a blanket class marker: %q", reason)
	}
}

// A scope failure alongside ANOTHER failing gate is not a too-narrow-envelope
// story, so the amendment must not fire and the run keeps the full retry budget.
func TestRunner_ScopeAmendmentSkippedWhenAnotherGateFails(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	seedSlices(t, st, item, declaredMillsComponent)

	gr := gates.NewRegistry()
	gr.Register(&gates.Scope{})
	gr.Register(&alwaysFailGate{name: "secret_scan"})
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {FilesChanged: []string{declaredMillsComponent, sharedPanelShell}},
	}}
	r := New(st, gr, disp, nil)

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	// maxAttempts (3), not the scope-specific cap of 2.
	if n := countCalls(disp.callsList(), "implement"); n != 3 {
		t.Errorf("implement dispatched %d times, want 3 (generic gate budget)", n)
	}
	reloaded, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if len(reloaded.Slices[0].Files) != 1 {
		t.Errorf("scope must not be amended when another gate fails: %v", reloaded.Slices[0].Files)
	}
}

// escalationReasonFromEvents pulls the reason string off the durable
// pipeline.run.escalated event — the same payload the HUD renders.
func escalationReasonFromEvents(t *testing.T, st *store.Store) string {
	t.Helper()
	for _, e := range eventKinds(t, st) {
		if e.Kind != "pipeline.run.escalated" {
			continue
		}
		if reason, ok := e.Payload["reason"].(string); ok {
			return reason
		}
	}
	t.Fatal("no pipeline.run.escalated event with a reason")
	return ""
}

// scopeViolationsArtifactFrom reads the structured amendment decision off the
// gate stage's persisted stage_results row.
func scopeViolationsArtifactFrom(t *testing.T, st *store.Store, runID string) map[string]any {
	t.Helper()
	rows, err := st.Pipeline.ListStages(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	for _, sr := range rows {
		if sr.Stage != postImplementGateStage || sr.Artifacts == nil {
			continue
		}
		if d, ok := sr.Artifacts[scopeViolationsArtifact].(map[string]any); ok {
			return d
		}
	}
	t.Fatalf("no %s artifact on %s", scopeViolationsArtifact, postImplementGateStage)
	return nil
}
