package telemetry

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCrossRepoStampDeliveryMetricsBoundsOutcomesAndLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewCrossRepoStampDeliveryMetrics(reg)
	metrics.RecordCrossRepoStampDelivery(CrossRepoStampDeliverySuccess)
	metrics.RecordCrossRepoStampDelivery(CrossRepoStampDeliveryDenial)
	metrics.RecordCrossRepoStampDelivery(CrossRepoStampDeliveryFailure)
	metrics.RecordCrossRepoStampDelivery("project-specific-error")

	for outcome, want := range map[string]float64{
		CrossRepoStampDeliverySuccess: 1,
		CrossRepoStampDeliveryDenial:  1,
		CrossRepoStampDeliveryFailure: 2,
	} {
		if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(outcome)); got != want {
			t.Errorf("deliveries{%q} = %v, want %v", outcome, got, want)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "mills_crossrepo_stamp_deliveries_total" {
		t.Fatalf("metric families = %+v", families)
	}
	var outcomes []string
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "outcome" {
			t.Fatalf("labels = %+v, want only outcome", metric.Label)
		}
		outcomes = append(outcomes, metric.Label[0].GetValue())
	}
	sort.Strings(outcomes)
	if got, want := outcomes, []string{"denial", "failure", "success"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

func TestOverseerSoakMetricsCountsFourBoundedDecisionSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewOverseerSoakMetrics(reg)

	for _, decision := range []struct {
		wouldAct bool
		diverged bool
	}{
		{wouldAct: false, diverged: false},
		{wouldAct: false, diverged: true},
		{wouldAct: true, diverged: false},
		{wouldAct: true, diverged: true},
	} {
		metrics.RecordDryRunDecision(decision.wouldAct, decision.diverged)
	}

	for _, labels := range [][]string{{"false", "false"}, {"false", "true"}, {"true", "false"}, {"true", "true"}} {
		if got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(labels...)); got != 1 {
			t.Errorf("decisions%v = %v, want 1", labels, got)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "loom_mills_overseer_dry_run_decisions_total" || len(families[0].Metric) != 4 {
		t.Fatalf("metric families = %+v, want one family with four bounded series", families)
	}
}

func TestOverseerSoakMetricsReusesRegisteredCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := NewOverseerSoakMetrics(reg)
	second := NewOverseerSoakMetrics(reg)

	first.RecordDryRunDecision(true, false)
	second.RecordDryRunDecision(true, false)
	if got := testutil.ToFloat64(first.DecisionsTotal.WithLabelValues("true", "false")); got != 2 {
		t.Fatalf("shared decisions counter = %v, want 2", got)
	}
}

func TestTerminalStateMetricsBoundsAndCountsConflicts(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewTerminalStateMetrics(reg)

	metrics.RecordTerminalStateConflict("done")
	metrics.RecordTerminalStateConflict("escalated")
	metrics.RecordTerminalStateConflict("paused")

	if got := testutil.ToFloat64(metrics.ConflictsTotal.WithLabelValues("done")); got != 1 {
		t.Fatalf("done conflicts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.ConflictsTotal.WithLabelValues("escalated")); got != 1 {
		t.Fatalf("escalated conflicts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.ConflictsTotal.WithLabelValues(terminalStateLabelUnknown)); got != 1 {
		t.Fatalf("unknown conflicts = %v, want 1", got)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	if len(families) != 1 || len(families[0].Metric) != 3 {
		t.Fatalf("metric families = %+v, want one family with three bounded series", families)
	}
}

func TestClassificationMetricsBoundsAndCountsClasses(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewClassificationMetrics(reg)

	metrics.RecordClassification(ClassificationClassExternalDependencyIncident)
	metrics.RecordClassification(ClassificationClassRepositoryRegression)
	metrics.RecordClassification(ClassificationClassExternalDependencyIncident)
	metrics.RecordClassification("future_dynamic_class")

	for class, want := range map[string]float64{
		ClassificationClassExternalDependencyIncident: 2,
		ClassificationClassRepositoryRegression:       1,
		ClassificationClassUnknown:                    1,
	} {
		if got := testutil.ToFloat64(metrics.ClassificationsTotal.WithLabelValues(class)); got != want {
			t.Errorf("classifications{%q} = %v, want %v", class, got, want)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	if len(families) != 1 || len(families[0].Metric) != 3 {
		t.Fatalf("metric families = %+v, want one family with three bounded series", families)
	}
}

func TestGateMetricsCountsOnlyVerdictFlips(t *testing.T) {
	metrics := NewGateMetrics(prometheus.NewRegistry())
	record := GateEvaluation{
		GateID: "scope", InputDigest: "stable", Verdict: GateVerdictPass,
	}
	metrics.RecordGateEvaluation(record)
	metrics.RecordGateEvaluation(record)
	record.Verdict = GateVerdictFail
	metrics.RecordGateEvaluation(record)
	metrics.RecordGateEvaluation(record)

	if got := testutil.ToFloat64(metrics.VerdictFlipsTotal.WithLabelValues(
		"scope", "pass", "fail",
	)); got != 1 {
		t.Fatalf("pass-to-fail flips = %v, want 1", got)
	}
}

func TestGateMetricsUsesBoundedFallbackLabels(t *testing.T) {
	metrics := NewGateMetrics(nil)
	record := GateEvaluation{
		GateID: "dynamic-gate", InputDigest: "stable", Verdict: GateVerdict("surprise"),
	}
	metrics.RecordGateEvaluation(record)
	record.Verdict = GateVerdictPass
	metrics.RecordGateEvaluation(record)

	if got := testutil.ToFloat64(metrics.VerdictFlipsTotal.WithLabelValues(
		"other", "unknown", "pass",
	)); got != 1 {
		t.Fatalf("fallback-label flips = %v, want 1", got)
	}
}
