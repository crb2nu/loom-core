package fleetgate

import (
	"strings"
	"testing"
)

func TestBenchmarkDeadlineIsInfrastructure(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		cause     string
		observed  int
		expected  int
		completed []float64
		want      bool
	}{
		{name: "partial slow deadline", status: 124, cause: "deadline", observed: 1, expected: 4, completed: []float64{24, 25}, want: true},
		{name: "partial fast deadline", status: 124, cause: "deadline", observed: 1, expected: 4, completed: []float64{5}},
		{name: "zero scenario deadline", status: 124, cause: "deadline", observed: 0, expected: 4, completed: []float64{24}},
		{name: "complete deadline", status: 124, cause: "deadline", observed: 4, expected: 4, completed: []float64{24}},
		{name: "sample timeout", status: 124, cause: "sample_timeout", observed: 1, expected: 4, completed: []float64{24}},
		{name: "non deadline failure", status: 1, cause: "sample_failure", observed: 1, expected: 4, completed: []float64{24}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := benchmarkDeadlineIsInfrastructure(tt.status, tt.cause, tt.observed, tt.expected, tt.completed, 30)
			if got != tt.want {
				t.Fatalf("benchmarkDeadlineIsInfrastructure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompareBenchmarksEmitsInfraSentinelForPartialSlowDeadline(t *testing.T) {
	manifest := testManifest()
	manifest.Benchmarks = []string{"BenchmarkRequired", "BenchmarkMissing"}
	metrics := repeatedMetric(100, minimumBenchmarkSamples)
	allocations := repeatedMetric(1, minimumBenchmarkSamples)
	benchmark := benchmarkOutput("BenchmarkRequired", metrics, allocations, allocations)
	baseline := benchmark + "LOOM_BENCHMARK_SAMPLE wall_seconds=24\n"
	candidate := benchmark + strings.Join([]string{
		"LOOM_BENCHMARK_SAMPLE wall_seconds=25",
		"LOOM_BENCHMARK_STOP status=124 cause=deadline sample_timeout_seconds=30",
		"",
	}, "\n")

	report, err := CompareBenchmarks(manifest, strings.NewReader(baseline), strings.NewReader(candidate))
	if err == nil || !strings.Contains(err.Error(), "CI-INFRA-FAILURE") {
		t.Fatalf("CompareBenchmarks() error = %v, want CI-INFRA-FAILURE", err)
	}
	if !report.InfrastructureFailure || report.CompletedSampleCount != 2 || report.MaxSampleWallSeconds != 25 {
		t.Fatalf("unexpected contention evidence: %+v", report)
	}
}
