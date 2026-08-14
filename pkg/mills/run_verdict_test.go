package mills

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

var verdictNow = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func escalatedRun(class string) *store.PipelineRun {
	ended := verdictNow.Add(-time.Hour)
	return &store.PipelineRun{
		ID:              "run-1",
		State:           store.PipelineEscalated,
		EscalationClass: class,
		EndedAt:         &ended,
	}
}

func verdictEvent(kind string, at time.Time, payload map[string]any) *store.Event {
	return &store.Event{
		Actor: "reconciler", Kind: kind,
		SubjectKind: "pipeline_run", SubjectID: "run-1",
		OccurredAt: at, Payload: payload,
	}
}

func TestResolveRunVerdict_DerivedFromTerminalRow(t *testing.T) {
	v := ResolveRunVerdict(escalatedRun("code"), nil)
	if v.Superseded || v.Class != "code" || v.Source != "" {
		t.Fatalf("derived verdict wrong: %+v", v)
	}
	if v.OccurredAt.IsZero() {
		t.Fatal("derived verdict must carry EndedAt")
	}

	done := &store.PipelineRun{ID: "run-2", State: store.PipelineDone}
	if v := ResolveRunVerdict(done, nil); v.Class != "done" || v.Superseded {
		t.Fatalf("done run verdict wrong: %+v", v)
	}

	// Unclassified escalation fails closed to code.
	if v := ResolveRunVerdict(escalatedRun(""), nil); v.Class != "code" {
		t.Fatalf("unclassified escalation must derive code, got %+v", v)
	}
}

func TestResolveRunVerdict_ExplicitEventSupersedes(t *testing.T) {
	events := []*store.Event{
		verdictEvent(RunVerdictKindGhostSparkMerged, verdictNow, map[string]any{
			"class": RunVerdictClassMergedAfterEscalation, "prior_class": "code",
			"outcome": "adopted_green_mr",
		}),
		verdictEvent("judge.verdict", verdictNow.Add(-2*time.Hour), nil),
	}
	v := ResolveRunVerdict(escalatedRun("code"), events)
	if !v.Superseded || v.Class != RunVerdictClassMergedAfterEscalation {
		t.Fatalf("explicit event must supersede: %+v", v)
	}
	if v.Source != "ghost_spark_merged" || v.PriorClass != "code" || v.Outcome != "adopted_green_mr" {
		t.Fatalf("supersede metadata wrong: %+v", v)
	}
	if !v.OccurredAt.Equal(verdictNow) {
		t.Fatalf("verdict must carry the event time, got %s", v.OccurredAt)
	}
}

func TestResolveRunVerdict_LegacyClosureRecognized(t *testing.T) {
	events := []*store.Event{
		verdictEvent("reconciler.ghost_spark_closed", verdictNow, map[string]any{
			"outcome": "merged_branch",
		}),
	}
	v := ResolveRunVerdict(escalatedRun("infra"), events)
	if !v.Superseded || v.Class != RunVerdictClassMergedAfterEscalation {
		t.Fatalf("legacy closure must supersede retroactively: %+v", v)
	}
	if v.Source != "ghost_spark_closed" || v.PriorClass != "infra" || v.Outcome != "merged_branch" {
		t.Fatalf("legacy supersede metadata wrong: %+v", v)
	}

	// The legacy event only corrects runs still recorded escalated — a done
	// run's closure event is bookkeeping, not a correction.
	done := &store.PipelineRun{ID: "run-1", State: store.PipelineDone}
	if v := ResolveRunVerdict(done, events); v.Superseded {
		t.Fatalf("legacy closure must not supersede a non-escalated run: %+v", v)
	}
}

func TestResolveRunVerdict_NewestExplicitWins(t *testing.T) {
	// ListBySubject order: newest first. An explicit event outranks the
	// legacy closure regardless of order; among explicit events the newest
	// (first) wins.
	events := []*store.Event{
		verdictEvent(RunVerdictKindOperatorOverride, verdictNow, map[string]any{
			"class": "code", "outcome": "operator_reaffirmed",
		}),
		verdictEvent(RunVerdictKindGhostSparkMerged, verdictNow.Add(-time.Hour), map[string]any{
			"class": RunVerdictClassMergedAfterEscalation,
		}),
		verdictEvent("reconciler.ghost_spark_closed", verdictNow.Add(-2*time.Hour), nil),
	}
	v := ResolveRunVerdict(escalatedRun("code"), events)
	if v.Class != "code" || v.Source != "operator_override" || !v.Superseded {
		t.Fatalf("newest explicit event must win: %+v", v)
	}
}

type verdictKindListerStub struct {
	kinds  []string
	events []*store.Event
}

func (s *verdictKindListerStub) ListSinceByKinds(_ context.Context, kinds []string, _ time.Time, _ int) ([]*store.Event, error) {
	s.kinds = append([]string(nil), kinds...)
	return s.events, nil
}

func TestSupersededRunIDsSince_IncludesOperatorOverride(t *testing.T) {
	stub := &verdictKindListerStub{events: []*store.Event{
		verdictEvent(RunVerdictKindOperatorOverride, verdictNow, nil),
	}}
	got, err := SupersededRunIDsSince(context.Background(), stub, verdictNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["run-1"]; !ok {
		t.Fatalf("operator override run missing from superseded IDs: %v", got)
	}
	found := false
	for _, kind := range stub.kinds {
		found = found || kind == RunVerdictKindOperatorOverride
	}
	if !found {
		t.Fatalf("correction scan kinds omit operator override: %v", stub.kinds)
	}
}

func TestResolveRunVerdict_NilSafety(t *testing.T) {
	if v := ResolveRunVerdict(nil, nil); v.Class != "" || v.Superseded {
		t.Fatalf("nil run must yield zero verdict: %+v", v)
	}
	events := []*store.Event{nil, verdictEvent("unrelated.kind", verdictNow, nil)}
	if v := ResolveRunVerdict(escalatedRun("config"), events); v.Class != "config" || v.Superseded {
		t.Fatalf("nil/foreign events must be skipped: %+v", v)
	}
}
