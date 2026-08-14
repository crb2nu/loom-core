package mills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// provenancePolicyYAML is fixtureV1 plus per-stage model pins, so a stamp has
// something concrete to record.
const provenancePolicyYAML = `
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
pipeline:
  default_template: mills-default-pipeline
  retry:
    max_attempts: 3
    cooldown_seconds: 300
  stage_models:
    implement: gpt-5.6-terra
    plan_slice: gpt-5.6-sol
`

// usePolicyWithStageModels repoints the reconciler (and its budget, which
// reads the same manager) at a policy file carrying stage_models, and returns
// the exact bytes on disk so a test can assert the stamped checksum is a
// digest of those bytes and not of some other read.
func usePolicyWithStageModels(t *testing.T, env *recTestEnv) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	raw := []byte(provenancePolicyYAML)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := NewPolicyManager(context.Background(), path, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	env.rec.Policy = pm
	env.rec.Budget = NewBudget(pm, NewStoreBudgetReader(env.store))
	return raw
}

// stampCounterProbe snapshots RunProvenanceStampsTotal so a test can assert
// its own delta. The counter lives in the default registry and accumulates
// across the package's tests, so absolute values are not assertable.
type stampCounterProbe struct{ before map[string]float64 }

func newStampCounterProbe() *stampCounterProbe {
	p := &stampCounterProbe{before: map[string]float64{}}
	for _, lane := range []string{"pipeline", "workflow"} {
		for _, outcome := range []string{"stamped", "duplicate", "error"} {
			p.before[lane+"/"+outcome] = testutil.ToFloat64(
				RunProvenanceStampsTotal.WithLabelValues(lane, outcome))
		}
	}
	return p
}

func (p *stampCounterProbe) delta(lane, outcome string) float64 {
	now := testutil.ToFloat64(RunProvenanceStampsTotal.WithLabelValues(lane, outcome))
	return now - p.before[lane+"/"+outcome]
}

func provenanceEvent(t *testing.T, env *recTestEnv, subjectKind, runID string) *store.Event {
	t.Helper()
	ev, err := env.store.Events.FirstBySubjectKind(
		context.Background(), subjectKind, runID, RunProvenanceEventKind)
	if err != nil {
		t.Fatalf("read %s provenance stamp: %v", subjectKind, err)
	}
	return ev
}

func provenanceEventCount(t *testing.T, env *recTestEnv, subjectKind, runID string) int {
	t.Helper()
	rows, err := env.store.Events.ListBySubject(context.Background(), subjectKind, runID, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	n := 0
	for _, ev := range rows {
		if ev != nil && ev.Kind == RunProvenanceEventKind {
			n++
		}
	}
	return n
}

// payloadStringMap reads a JSON-round-tripped map[string]any field back as
// map[string]string. Events come off the store decoded from JSON, so nested
// maps arrive as map[string]any.
func payloadStringMap(t *testing.T, ev *store.Event, field string) map[string]string {
	t.Helper()
	raw, ok := ev.Payload[field]
	if !ok {
		t.Fatalf("payload has no %q field: %+v", field, ev.Payload)
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("payload %q = %T, want an object", field, raw)
	}
	out := make(map[string]string, len(nested))
	for k, v := range nested {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("payload %s[%s] = %T, want string", field, k, v)
		}
		out[k] = s
	}
	return out
}

// TestReconciler_RunProvenanceStampedOncePerRun: a dispatch that fails and is
// retried must not re-stamp. The stamp is the join key for per-version
// analytics, so a second row — or a row re-resolved against a policy that
// hot-reloaded between the crash and the retry — would misattribute the run.
func TestReconciler_RunProvenanceStampedOncePerRun(t *testing.T) {
	env := newRecEnv(t, nil)
	stampMetric := newStampCounterProbe()
	env.rec.ProvenanceStageModels = func(*store.BacklogItem) map[string]string {
		return map[string]string{"implement": "first-model"}
	}
	env.starter.fail = errors.New("starter unavailable")
	item := seedQueuedItemForRouting(t, env, "MILLS-PROV-RETRY", "pkg/mills/reconciler.go")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	// Configuration changes between the failed dispatch and the retry, exactly
	// as a policy hot-reload would.
	env.rec.ProvenanceStageModels = func(*store.BacklogItem) map[string]string {
		return map[string]string{"implement": "second-model"}
	}
	env.starter.fail = nil
	env.rec.Clock = func() time.Time { return env.now.Add(2 * time.Second) }
	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if got := stampMetric.delta("pipeline", "stamped"); got != 1 {
		t.Fatalf("stamped counter delta=%v, want 1", got)
	}
	if got := stampMetric.delta("pipeline", "duplicate"); got != 1 {
		t.Fatalf("duplicate counter delta=%v, want 1 (the retry re-reached an already-stamped run)", got)
	}

	runs, err := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("pipeline runs=%d err=%v, want 1", len(runs), err)
	}
	if got := provenanceEventCount(t, env, "pipeline_run", runs[0].ID); got != 1 {
		t.Fatalf("provenance stamps=%d, want exactly 1", got)
	}
	models := payloadStringMap(t, provenanceEvent(t, env, "pipeline_run", runs[0].ID), "stage_models")
	if models["implement"] != "first-model" {
		t.Fatalf("stage_models[implement]=%q, want the start-time value first-model", models["implement"])
	}
}

// TestReconciler_RunProvenanceCarriesPolicyChecksumAndStageModels: the stamped
// checksum must digest the exact policy bytes the active policy was parsed
// from, and the stage models must be the ones the fixture pins.
func TestReconciler_RunProvenanceCarriesPolicyChecksumAndStageModels(t *testing.T) {
	env := newRecEnv(t, nil)
	raw := usePolicyWithStageModels(t, env)
	item := seedQueuedItemForRouting(t, env, "MILLS-PROV-PAYLOAD", "pkg/mills/policy.go")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs, err := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("pipeline runs=%d err=%v, want 1", len(runs), err)
	}
	ev := provenanceEvent(t, env, "pipeline_run", runs[0].ID)
	if ev.Actor != RunProvenanceActor {
		t.Fatalf("actor=%q, want %q", ev.Actor, RunProvenanceActor)
	}
	if got := ev.Payload["run_id"]; got != runs[0].ID {
		t.Fatalf("run_id=%v, want %s", got, runs[0].ID)
	}
	if got := ev.Payload["backlog_id"]; got != item.ID {
		t.Fatalf("backlog_id=%v, want %s", got, item.ID)
	}
	if got := ev.Payload["lane"]; got != "pipeline" {
		t.Fatalf("lane=%v, want pipeline", got)
	}
	want := PolicyChecksum(raw)
	if got := ev.Payload["policy_checksum"]; got != want {
		t.Fatalf("policy_checksum=%v, want %s", got, want)
	}
	models := payloadStringMap(t, ev, "stage_models")
	if models["implement"] != "gpt-5.6-terra" || models["plan_slice"] != "gpt-5.6-sol" {
		t.Fatalf("stage_models=%v, want the fixture pins", models)
	}
	if _, pinned := models["pr_self_review"]; pinned {
		t.Fatalf("stage_models=%v, want unpinned stages omitted", models)
	}
}

// TestReconciler_RunProvenanceStampFailureDoesNotBlockDispatch: an events-table
// failure on the stamp must be swallowed. Provenance is an analytics join key;
// losing one must never cost a run.
func TestReconciler_RunProvenanceStampFailureDoesNotBlockDispatch(t *testing.T) {
	env := newRecEnv(t, nil)
	stampMetric := newStampCounterProbe()
	// Fail only the provenance insert, so the rest of the dispatch (claim,
	// transition events, dispatch ack) exercises its real path.
	_, err := env.store.DB().ExecContext(context.Background(), `
		CREATE TRIGGER reject_run_provenance BEFORE INSERT ON events
		WHEN NEW.kind = '`+RunProvenanceEventKind+`'
		BEGIN SELECT RAISE(ABORT, 'events unavailable'); END
	`)
	if err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	item := seedQueuedItemForRouting(t, env, "MILLS-PROV-APPEND-FAIL", "pkg/mills/store/store.go")

	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 || env.starter.calls() != 1 {
		t.Fatalf("started=%d starter calls=%d, want the dispatch to proceed", res.Started, env.starter.calls())
	}
	fresh, err := env.store.Backlog.Get(context.Background(), item.ID)
	if err != nil || fresh.State != store.BacklogRunning {
		t.Fatalf("item=%+v err=%v, want running", fresh, err)
	}
	runs, _ := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if len(runs) != 1 {
		t.Fatalf("pipeline runs=%d, want 1", len(runs))
	}
	if got := provenanceEventCount(t, env, "pipeline_run", runs[0].ID); got != 0 {
		t.Fatalf("provenance stamps=%d, want none (the append was rejected)", got)
	}
	// The stamp was attempted and counted as an error, not silently skipped.
	if got := stampMetric.delta("pipeline", "error"); got != 1 {
		t.Fatalf("error counter delta=%v, want 1", got)
	}
}

// TestReconciler_RunProvenancePromptHashesEmptyWhenUnresolvable: with no
// prompt-hash resolver wired the stamp records an empty map rather than
// guessing. A wrong prompt digest silently corrupts every per-prompt-version
// rollup that joins on it.
func TestReconciler_RunProvenancePromptHashesEmptyWhenUnresolvable(t *testing.T) {
	env := newRecEnv(t, nil)
	item := seedQueuedItemForRouting(t, env, "MILLS-PROV-NOPROMPTS", "docs/MILLS.md")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs, _ := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if len(runs) != 1 {
		t.Fatalf("pipeline runs=%d, want 1", len(runs))
	}
	ev := provenanceEvent(t, env, "pipeline_run", runs[0].ID)
	if hashes := payloadStringMap(t, ev, "prompt_hashes"); len(hashes) != 0 {
		t.Fatalf("prompt_hashes=%v, want empty", hashes)
	}
}

// TestReconciler_RunProvenanceStampsResolvedPromptHashes: a wired resolver's
// digests reach the payload verbatim.
func TestReconciler_RunProvenanceStampsResolvedPromptHashes(t *testing.T) {
	env := newRecEnv(t, nil)
	digest := ProvenanceDigest([]byte("Implement backlog item %s (%q)."))
	env.rec.ProvenancePromptHashes = func() map[string]string {
		return map[string]string{"stage_prompt:implement": digest, "stage_prompt:blank": ""}
	}
	item := seedQueuedItemForRouting(t, env, "MILLS-PROV-PROMPTS", "cmd/loom-mills-operator/main.go")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs, _ := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if len(runs) != 1 {
		t.Fatalf("pipeline runs=%d, want 1", len(runs))
	}
	hashes := payloadStringMap(t, provenanceEvent(t, env, "pipeline_run", runs[0].ID), "prompt_hashes")
	if hashes["stage_prompt:implement"] != digest {
		t.Fatalf("prompt_hashes=%v, want the resolved digest", hashes)
	}
	if _, ok := hashes["stage_prompt:blank"]; ok {
		t.Fatalf("prompt_hashes=%v, want empty digests dropped", hashes)
	}
}

// TestReconciler_WorkflowStartStampsRunProvenance: the imperative lane stamps
// at the same boundary, keyed subject_kind=workflow_run. Without this the
// provenance join thins exactly as squad attribution did before !1260.
func TestReconciler_WorkflowStartStampsRunProvenance(t *testing.T) {
	env := newRecEnv(t, nil)
	raw := usePolicyWithStageModels(t, env)
	env.rec.WorkflowSelector = &fakeWorkflowSelector{sel: testWorkflowSelection()}
	item := seedQueuedWorkflowItem(t, env, "MILLS-PROV-WF")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	imperative, err := env.store.Workflow.ListRunningImperativeRuns(context.Background())
	if err != nil || len(imperative) != 1 {
		t.Fatalf("imperative runs=%v err=%v, want one", imperative, err)
	}
	ev := provenanceEvent(t, env, "workflow_run", imperative[0].ID)
	if got := ev.Payload["lane"]; got != "workflow" {
		t.Fatalf("lane=%v, want workflow", got)
	}
	if got := ev.Payload["backlog_id"]; got != item.ID {
		t.Fatalf("backlog_id=%v, want %s", got, item.ID)
	}
	if got := ev.Payload["policy_checksum"]; got != PolicyChecksum(raw) {
		t.Fatalf("policy_checksum=%v, want the loaded policy digest", got)
	}
}
