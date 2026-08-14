package telemetry

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestGateResultEventJSONIsBounded(t *testing.T) {
	event := GateResultEvent{
		GateID:      "scope",
		Verdict:     GateVerdictSkip,
		ParseStatus: GateParseStatusParsed,
	}

	got, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"gate_id":"scope","verdict":"skip","parse_status":"parsed"}`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func TestGateFailureCollectorSnapshotIsBoundedAndDeterministic(t *testing.T) {
	collector := &GateFailureCollector{}
	events := []GateEvaluation{
		{GateID: "scope", RunID: "run-fail", Verdict: GateVerdictFail, FailureCategory: GateFailureCategoryFail, Reason: "secret raw failure"},
		{GateID: "missing", RunID: "run-unknown", Verdict: GateVerdictFail, FailureCategory: GateFailureCategoryUnknownGate, Reason: "dynamic missing gate text"},
		{GateID: "judge", RunID: "run-infra", Verdict: GateVerdictError, FailureCategory: GateFailureCategoryInfrastructureError, Reason: "dial tcp private.example"},
		{GateID: "gitlab", RunID: "run-external", Verdict: GateVerdictError, FailureCategory: GateFailureCategoryExternalDependency, Reason: "gitlab unavailable"},
		{GateID: "scope", RunID: "run-pass", Verdict: GateVerdictPass},
		{GateID: "scope", RunID: "run-skip", Verdict: GateVerdictSkip},
	}
	for i := len(events) - 1; i >= 0; i-- {
		collector.RecordGateEvaluation(events[i])
	}

	snapshot := collector.Snapshot()
	if len(snapshot.Failures) != 4 {
		t.Fatalf("failures = %+v, want four failure categories", snapshot.Failures)
	}
	want := []struct {
		gate, category, run string
	}{
		{"gitlab", "external_dependency", "run-external"},
		{"judge", "infrastructure-error", "run-infra"},
		{"missing", "unknown-gate", "run-unknown"},
		{"scope", "fail", "run-fail"},
	}
	for i, expected := range want {
		got := snapshot.Failures[i]
		if got.GateID != expected.gate || string(got.Category) != expected.category ||
			got.Count != 1 || len(got.RecentRunIDs) != 1 || got.RecentRunIDs[0] != expected.run {
			t.Errorf("failure[%d] = %+v, want gate/category/run %q/%q/%q", i, got, expected.gate, expected.category, expected.run)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"secret raw failure", "dynamic missing gate text", "private.example"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("KPI snapshot leaked raw reason %q: %s", raw, encoded)
		}
	}
}

func TestGateFailureCollectorConcurrentRetentionIsBounded(t *testing.T) {
	collector := &GateFailureCollector{}
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			collector.RecordGateEvaluation(GateEvaluation{
				GateID: "scope", RunID: "run", Verdict: GateVerdictFail,
				FailureCategory: GateFailureCategoryFail,
			})
		}()
	}
	workers.Wait()
	got := collector.Snapshot().Failures
	if len(got) != 1 || got[0].Count != 32 || len(got[0].RecentRunIDs) != gateFailureRecentRunLimit {
		t.Fatalf("snapshot = %+v, want count 32 and %d retained run IDs", got, gateFailureRecentRunLimit)
	}
}

func TestGateEvaluationJSONIncludesVerdictEvidence(t *testing.T) {
	event := GateEvaluation{GateID: "scope", Verdict: GateVerdictFail, Reason: "outside scope", RunID: "run-1", DurationMS: 12}
	got, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"gate_id":"scope","run_id":"run-1","input_digest":"","verdict":"fail","reason":"outside scope","duration_ms":12}`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}
