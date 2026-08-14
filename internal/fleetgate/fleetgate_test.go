package fleetgate

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func testManifest() Manifest {
	return Manifest{
		SchemaVersion: ManifestSchema,
		SuiteVersion:  1,
		Thresholds: BenchmarkThresholds{
			TimePercent: 10, BytesPercent: 15, AllocationsPercent: 15,
		},
		TestGroups: []TestGroup{{
			Name:      "required",
			Scenarios: []TestScenario{{Package: "example/pkg", Test: "TestRequired"}},
		}},
		Benchmarks: []string{"BenchmarkRequired"},
	}
}

func TestManifestRejectsUnknownSchema(t *testing.T) {
	manifest := testManifest()
	manifest.SchemaVersion = "loom.fleet-reliability.suite/v2"
	if err := manifest.Validate(); err == nil {
		t.Fatal("expected unknown manifest schema to fail validation")
	}
}

func TestManifestPlanDerivesRegexAndUniquePackages(t *testing.T) {
	manifest := testManifest()
	manifest.TestGroups[0].Scenarios = []TestScenario{
		{Package: "example/pkg", Test: "TestRequired"},
		{Package: "example/pkg", Test: "TestNameWith[Meta]"},
		{Package: "example/other", Test: "TestOther"},
	}

	plan, err := manifest.Plan("required")
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got, want := plan.Regex, `^(TestRequired|TestNameWith\[Meta\]|TestOther)$`; got != want {
		t.Fatalf("Plan().Regex = %q, want %q", got, want)
	}
	if got, want := strings.Join(plan.Packages, ","), "example/pkg,example/other"; got != want {
		t.Fatalf("Plan().Packages = %q, want %q", got, want)
	}
}

func benchmarkOutput(name string, nanos, bytes, allocations []float64) string {
	var output strings.Builder
	for i := range nanos {
		fmt.Fprintf(&output, "%s-8 100 %.6g ns/op %.6g B/op %.6g allocs/op\n",
			name, nanos[i], bytes[i], allocations[i])
	}
	return output.String()
}

func repeatedMetric(value float64, count int) []float64 {
	values := make([]float64, count)
	for i := range values {
		values[i] = value
	}
	return values
}

func TestVerifyTestEventsRequiresRunAndPass(t *testing.T) {
	events := strings.Join([]string{
		`{"Action":"run","Package":"example/pkg","Test":"TestRequired"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestRequired"}`,
		`{"Action":"pass","Package":"example/pkg"}`,
	}, "\n")
	report, err := VerifyTestEvents(testManifest(), "required", strings.NewReader(events))
	if err != nil {
		t.Fatalf("VerifyTestEvents() error = %v", err)
	}
	if !report.Passed || report.ObservedScenarioCount != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestVerifyTestEventsRejectsSkipAndZeroMatch(t *testing.T) {
	events := `{"Action":"skip","Package":"example/pkg","Test":"TestOther"}`
	report, err := VerifyTestEvents(testManifest(), "required", strings.NewReader(events))
	if err == nil {
		t.Fatal("expected strict verification failure")
	}
	if report.Passed || report.ObservedScenarioCount != 0 || len(report.UnexpectedSkips) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestVerifyTestEventsRejectsUnexpectedTopLevelButAllowsManifestedSubtests(t *testing.T) {
	events := strings.Join([]string{
		`{"Action":"run","Package":"example/pkg","Test":"TestRequired"}`,
		`{"Action":"run","Package":"example/pkg","Test":"TestRequired/child"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestRequired/child"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestRequired"}`,
		`{"Action":"run","Package":"example/pkg","Test":"TestExtra"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestExtra"}`,
		`{"Action":"pass","Package":"example/pkg"}`,
	}, "\n")
	report, err := VerifyTestEvents(testManifest(), "required", strings.NewReader(events))
	if err == nil {
		t.Fatal("expected unexpected top-level test to fail verification")
	}
	if report.Passed || report.ObservedScenarioCount != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.UnexpectedTests) != 1 || report.UnexpectedTests[0] != "example/pkg/TestExtra" {
		t.Fatalf("unexpected top-level tests: %v", report.UnexpectedTests)
	}
}

func TestCompareBenchmarksThresholds(t *testing.T) {
	baseline := strings.NewReader(benchmarkOutput(
		"BenchmarkRequired",
		repeatedMetric(100, minimumBenchmarkSamples),
		repeatedMetric(10, minimumBenchmarkSamples),
		repeatedMetric(1, minimumBenchmarkSamples),
	))
	candidate := strings.NewReader(benchmarkOutput(
		"BenchmarkRequired",
		repeatedMetric(109, minimumBenchmarkSamples),
		repeatedMetric(11, minimumBenchmarkSamples),
		repeatedMetric(1, minimumBenchmarkSamples),
	))
	report, err := CompareBenchmarks(testManifest(), baseline, candidate)
	if err != nil {
		t.Fatalf("CompareBenchmarks() error = %v", err)
	}
	if !report.Passed || report.ObservedScenarioCount != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if got := report.Benchmarks[0].BaselineSampleCount; got != minimumBenchmarkSamples {
		t.Fatalf("baseline sample count = %d, want %d", got, minimumBenchmarkSamples)
	}
}

func TestCompareBenchmarksRejectsRegressionAndMissing(t *testing.T) {
	baselineText := benchmarkOutput(
		"BenchmarkRequired",
		repeatedMetric(100, minimumBenchmarkSamples),
		repeatedMetric(10, minimumBenchmarkSamples),
		repeatedMetric(1, minimumBenchmarkSamples),
	)
	baseline := strings.NewReader(baselineText)
	candidate := strings.NewReader(benchmarkOutput(
		"BenchmarkRequired",
		repeatedMetric(111, minimumBenchmarkSamples),
		repeatedMetric(12, minimumBenchmarkSamples),
		repeatedMetric(2, minimumBenchmarkSamples),
	))
	report, err := CompareBenchmarks(testManifest(), baseline, candidate)
	if err == nil || report.Passed {
		t.Fatalf("expected regression failure, report=%+v err=%v", report, err)
	}

	report, err = CompareBenchmarks(testManifest(), strings.NewReader(""), strings.NewReader(""))
	if err == nil || report.ObservedScenarioCount != 0 {
		t.Fatalf("expected missing benchmark failure, report=%+v err=%v", report, err)
	}
}

func TestCompareBenchmarksRequiresEqualMinimumSampleCounts(t *testing.T) {
	minimumSamples := repeatedMetric(100, minimumBenchmarkSamples)
	tooFewSamples := repeatedMetric(100, minimumBenchmarkSamples-1)
	metricMinimum := repeatedMetric(1, minimumBenchmarkSamples)
	metricTooFew := repeatedMetric(1, minimumBenchmarkSamples-1)

	report, err := CompareBenchmarks(
		testManifest(),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", minimumSamples, metricMinimum, metricMinimum)),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", tooFewSamples, metricTooFew, metricTooFew)),
	)
	if err == nil || report.Passed || report.Benchmarks[0].Failure == "" {
		t.Fatalf("expected unequal sample count failure, report=%+v err=%v", report, err)
	}
	if result := report.Benchmarks[0]; result.TimeSignTestPValue != 1 || result.TimeSignificant {
		t.Fatalf("unevaluated timing must report p=1 and non-significant: %+v", result)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatalf("marshal failure report: %v", marshalErr)
	}
	if !strings.Contains(string(encoded), `"time_sign_test_p_value":1`) {
		t.Fatalf("failure report JSON does not preserve unevaluated p=1: %s", encoded)
	}

	report, err = CompareBenchmarks(
		testManifest(),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", tooFewSamples, metricTooFew, metricTooFew)),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", tooFewSamples, metricTooFew, metricTooFew)),
	)
	if err == nil || report.Passed || report.Benchmarks[0].Failure == "" {
		t.Fatalf("expected minimum sample count failure, report=%+v err=%v", report, err)
	}
	if result := report.Benchmarks[0]; result.TimeSignTestPValue != 1 || result.TimeSignificant {
		t.Fatalf("short sample timing must report p=1 and non-significant: %+v", result)
	}
}

func TestCompareBenchmarksUsesMedianPairedRatios(t *testing.T) {
	// Independent medians would compare 100/1 and report a 9,900% regression.
	// Pairing each same-round sample yields ten ratios of 1 and one ratio of
	// 100, whose median is 1 and correctly discounts the single noisy round.
	baselineNanos := []float64{1, 1, 1, 1, 1, 1, 100, 100, 100, 100, 100}
	candidateNanos := []float64{1, 1, 1, 1, 1, 100, 100, 100, 100, 100, 100}
	metrics := repeatedMetric(1, minimumBenchmarkSamples)
	report, err := CompareBenchmarks(
		testManifest(),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
		strings.NewReader(benchmarkOutput("BenchmarkRequired", candidateNanos, metrics, metrics)),
	)
	if err != nil || !report.Passed {
		t.Fatalf("paired comparison should tolerate one noisy round, report=%+v err=%v", report, err)
	}
	if got := report.Benchmarks[0].TimeRegressionPercent; got != 0 {
		t.Fatalf("paired time regression = %v, want 0", got)
	}
}

func TestCompareBenchmarksRequiresEffectAndSignificanceForTime(t *testing.T) {
	baselineNanos := repeatedMetric(100, minimumBenchmarkSamples)
	metrics := repeatedMetric(1, minimumBenchmarkSamples)

	t.Run("eight of eleven slower is inconclusive", func(t *testing.T) {
		candidateNanos := append(repeatedMetric(120, 8), repeatedMetric(80, 3)...)
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", candidateNanos, metrics, metrics)),
		)
		if err != nil || !report.Passed {
			t.Fatalf("inconclusive timing should pass, report=%+v err=%v", report, err)
		}
		if report.TimeSignificanceAlpha != 0.05 {
			t.Fatalf("time significance alpha = %v, want 0.05", report.TimeSignificanceAlpha)
		}
		result := report.Benchmarks[0]
		if !floatClose(result.TimeRegressionPercent, 20) || result.CandidateSlowerPairs != 8 || result.ComparableTimePairs != 11 {
			t.Fatalf("unexpected timing evidence: %+v", result)
		}
		if result.TimeSignificant || !floatClose(result.TimeSignTestPValue, 0.11328125) {
			t.Fatalf("eight slower pairs should be inconclusive: %+v", result)
		}
	})

	t.Run("nine of eleven slower fails above threshold", func(t *testing.T) {
		candidateNanos := append(repeatedMetric(120, 9), repeatedMetric(80, 2)...)
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", candidateNanos, metrics, metrics)),
		)
		if err == nil || report.Passed {
			t.Fatalf("significant timing regression should fail, report=%+v err=%v", report, err)
		}
		result := report.Benchmarks[0]
		if !result.TimeSignificant || !floatClose(result.TimeSignTestPValue, 0.03271484375) {
			t.Fatalf("nine slower pairs should be significant: %+v", result)
		}
	})

	t.Run("significant direction below threshold passes", func(t *testing.T) {
		candidateNanos := append(repeatedMetric(105, 9), repeatedMetric(95, 2)...)
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", candidateNanos, metrics, metrics)),
		)
		if err != nil || !report.Passed {
			t.Fatalf("sub-threshold timing should pass, report=%+v err=%v", report, err)
		}
		result := report.Benchmarks[0]
		if !result.TimeSignificant || !floatClose(result.TimeRegressionPercent, 5) {
			t.Fatalf("expected significant direction with sub-threshold effect: %+v", result)
		}
	})

	t.Run("ties are excluded from the sign test", func(t *testing.T) {
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, metrics, metrics)),
		)
		if err != nil || !report.Passed {
			t.Fatalf("identical timings should pass, report=%+v err=%v", report, err)
		}
		result := report.Benchmarks[0]
		if result.CandidateSlowerPairs != 0 || result.ComparableTimePairs != 0 || result.TimeSignTestPValue != 1 || result.TimeSignificant {
			t.Fatalf("exact ties should not count as signs: %+v", result)
		}
	})
}

func TestCompareBenchmarksResourceRegressionsRemainStrict(t *testing.T) {
	baselineNanos := repeatedMetric(100, minimumBenchmarkSamples)
	baselineMetrics := repeatedMetric(1, minimumBenchmarkSamples)

	t.Run("bytes", func(t *testing.T) {
		candidateBytes := repeatedMetric(2, minimumBenchmarkSamples)
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, baselineMetrics, baselineMetrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, candidateBytes, baselineMetrics)),
		)
		if err == nil || report.Passed || report.Benchmarks[0].BytesRegression != 100 {
			t.Fatalf("byte regression should fail unchanged, report=%+v err=%v", report, err)
		}
	})

	t.Run("allocations", func(t *testing.T) {
		candidateAllocations := repeatedMetric(2, minimumBenchmarkSamples)
		report, err := CompareBenchmarks(
			testManifest(),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, baselineMetrics, baselineMetrics)),
			strings.NewReader(benchmarkOutput("BenchmarkRequired", baselineNanos, baselineMetrics, candidateAllocations)),
		)
		if err == nil || report.Passed || report.Benchmarks[0].AllocationRegression != 100 {
			t.Fatalf("allocation regression should fail unchanged, report=%+v err=%v", report, err)
		}
	})
}

func TestExactOneSidedSignTestPValue(t *testing.T) {
	tests := []struct {
		name     string
		positive int
		trials   int
		want     float64
	}{
		{name: "no comparable pairs", positive: 0, trials: 0, want: 1},
		{name: "eight of eleven", positive: 8, trials: 11, want: 0.11328125},
		{name: "nine of eleven", positive: 9, trials: 11, want: 0.03271484375},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exactOneSidedSignTestPValue(test.positive, test.trials); !floatClose(got, test.want) {
				t.Fatalf("exactOneSidedSignTestPValue(%d, %d) = %v, want %v", test.positive, test.trials, got, test.want)
			}
		})
	}
}

func floatClose(got, want float64) bool {
	return math.Abs(got-want) <= 1e-12
}

func TestCompareBenchmarksRejectsUnexpectedFleetBenchmark(t *testing.T) {
	metrics := repeatedMetric(1, minimumBenchmarkSamples)
	required := benchmarkOutput("BenchmarkRequired", metrics, metrics, metrics)
	unexpected := benchmarkOutput("BenchmarkFleetUnexpected", metrics, metrics, metrics)
	report, err := CompareBenchmarks(
		testManifest(),
		strings.NewReader(required+unexpected),
		strings.NewReader(required+unexpected),
	)
	if err == nil || report.Passed {
		t.Fatalf("expected unexpected benchmark failure, report=%+v err=%v", report, err)
	}
	if len(report.UnexpectedBenchmarks) != 1 || report.UnexpectedBenchmarks[0] != "BenchmarkFleetUnexpected" {
		t.Fatalf("unexpected benchmark report = %+v", report.UnexpectedBenchmarks)
	}
}

func TestNewEvidenceMetadataRequiresPositiveScenarioCounts(t *testing.T) {
	metadata, err := NewEvidenceMetadata(testManifest(), "head", "base", false, "config", "schema", "make ci-reliability", "go test", map[string]int{"required": 1, "benchmark": 1})
	if err != nil {
		t.Fatalf("NewEvidenceMetadata() error = %v", err)
	}
	if metadata.ScenarioTotal != 2 || metadata.ScenarioCounts["required"] != 1 {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	if _, err := NewEvidenceMetadata(testManifest(), "head", "base", false, "config", "schema", "command", "go", map[string]int{"required": 0, "benchmark": 1}); err == nil {
		t.Fatal("expected zero scenario count to fail")
	}
}

func TestNewEvidenceMetadataDerivesScenarioCounts(t *testing.T) {
	metadata, err := NewEvidenceMetadata(testManifest(), "head", "base", false, "config", "schema", "make ci-reliability", "go test", nil)
	if err != nil {
		t.Fatalf("NewEvidenceMetadata() error = %v", err)
	}
	if metadata.ScenarioTotal != 2 || metadata.ScenarioCounts["required"] != 1 || metadata.ScenarioCounts["benchmark"] != 1 {
		t.Fatalf("unexpected derived metadata: %+v", metadata)
	}
}
