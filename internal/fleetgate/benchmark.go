package fleetgate

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var benchmarkNamePattern = regexp.MustCompile(`^(Benchmark\S+)-\d+$`)
var benchmarkEvidencePattern = regexp.MustCompile(`^LOOM_BENCHMARK_(SAMPLE|STOP) (.+)$`)

const (
	minimumBenchmarkSamples = 11
	timeSignificanceAlpha   = 0.05
)

// nowFunc exists so waiver-expiry tests can pin the clock.
var nowFunc = time.Now

// activeWaiver returns the manifest waiver covering name on day `at`, or nil.
// Until is inclusive: the waiver still applies on its expiry date and is
// inert the day after.
func activeWaiver(manifest Manifest, name string, at time.Time) *BenchmarkWaiver {
	for i := range manifest.Waivers {
		w := &manifest.Waivers[i]
		if w.Benchmark != name {
			continue
		}
		until, err := time.Parse("2006-01-02", w.Until)
		if err != nil {
			return nil // Validate rejects this; fail closed here regardless.
		}
		if at.Before(until.AddDate(0, 0, 1)) {
			return w
		}
		return nil
	}
	return nil
}

type benchmarkSample struct {
	Nanos       float64
	Bytes       float64
	Allocations float64
}

type BenchmarkResult struct {
	Name                  string  `json:"name"`
	BaselineNanos         float64 `json:"baseline_ns_per_op"`
	CandidateNanos        float64 `json:"candidate_ns_per_op"`
	TimeRegressionPercent float64 `json:"time_regression_percent"`
	BaselineBytes         float64 `json:"baseline_bytes_per_op"`
	CandidateBytes        float64 `json:"candidate_bytes_per_op"`
	BytesRegression       float64 `json:"bytes_regression_percent"`
	BaselineAllocations   float64 `json:"baseline_allocations_per_op"`
	CandidateAllocations  float64 `json:"candidate_allocations_per_op"`
	AllocationRegression  float64 `json:"allocation_regression_percent"`
	BaselineSampleCount   int     `json:"baseline_sample_count"`
	CandidateSampleCount  int     `json:"candidate_sample_count"`
	CandidateSlowerPairs  int     `json:"candidate_slower_pair_count"`
	ComparableTimePairs   int     `json:"comparable_time_pair_count"`
	TimeSignTestPValue    float64 `json:"time_sign_test_p_value"`
	TimeSignificant       bool    `json:"time_statistically_significant"`
	WaiverApplied         string  `json:"waiver_applied,omitempty"`
	Failure               string  `json:"failure,omitempty"`
	Passed                bool    `json:"passed"`
}

type BenchmarkReport struct {
	SchemaVersion         string              `json:"schema_version"`
	SuiteVersion          int                 `json:"suite_version"`
	ExpectedScenarioCount int                 `json:"expected_scenario_count"`
	ObservedScenarioCount int                 `json:"observed_scenario_count"`
	Thresholds            BenchmarkThresholds `json:"thresholds"`
	TimeSignificanceAlpha float64             `json:"time_significance_alpha"`
	Benchmarks            []BenchmarkResult   `json:"benchmarks"`
	UnexpectedBenchmarks  []string            `json:"unexpected_benchmarks,omitempty"`
	CompletedSampleCount  int                 `json:"completed_sample_count,omitempty"`
	MaxSampleWallSeconds  float64             `json:"max_sample_wall_seconds,omitempty"`
	DeadlineStatus        int                 `json:"deadline_status,omitempty"`
	InfrastructureFailure bool                `json:"infrastructure_failure,omitempty"`
	Passed                bool                `json:"passed"`
}

type benchmarkEvidence struct {
	completedSampleWallSeconds []float64
	stopStatus                 int
	stopCause                  string
	sampleTimeoutSeconds       float64
}

func CompareBenchmarks(manifest Manifest, baselineReader, candidateReader io.Reader) (BenchmarkReport, error) {
	baseline, baselineEvidence, err := parseBenchmarks(baselineReader)
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("parse baseline: %w", err)
	}
	candidate, candidateEvidence, err := parseBenchmarks(candidateReader)
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("parse candidate: %w", err)
	}

	report := BenchmarkReport{
		SchemaVersion:         ReportSchema,
		SuiteVersion:          manifest.SuiteVersion,
		ExpectedScenarioCount: len(manifest.Benchmarks),
		Thresholds:            manifest.Thresholds,
		TimeSignificanceAlpha: timeSignificanceAlpha,
		Passed:                true,
	}
	expectedBenchmarks := make(map[string]struct{}, len(manifest.Benchmarks))
	for _, name := range manifest.Benchmarks {
		expectedBenchmarks[name] = struct{}{}
	}
	unexpectedBenchmarks := make(map[string]struct{})
	for name := range baseline {
		if _, expected := expectedBenchmarks[name]; !expected && strings.HasPrefix(name, "BenchmarkFleet") {
			unexpectedBenchmarks[name] = struct{}{}
		}
	}
	for name := range candidate {
		if _, expected := expectedBenchmarks[name]; !expected && strings.HasPrefix(name, "BenchmarkFleet") {
			unexpectedBenchmarks[name] = struct{}{}
		}
	}
	for name := range unexpectedBenchmarks {
		report.UnexpectedBenchmarks = append(report.UnexpectedBenchmarks, name)
	}
	sort.Strings(report.UnexpectedBenchmarks)
	if len(report.UnexpectedBenchmarks) > 0 {
		report.Passed = false
	}

	for _, name := range manifest.Benchmarks {
		baseSamples := baseline[name]
		candidateSamples := candidate[name]
		result := BenchmarkResult{
			Name:                 name,
			BaselineSampleCount:  len(baseSamples),
			CandidateSampleCount: len(candidateSamples),
			TimeSignTestPValue:   1,
		}
		if len(baseSamples) != len(candidateSamples) {
			result.Failure = fmt.Sprintf("sample count mismatch: baseline=%d candidate=%d", len(baseSamples), len(candidateSamples))
			report.Benchmarks = append(report.Benchmarks, result)
			report.Passed = false
			continue
		}
		if len(baseSamples) < minimumBenchmarkSamples {
			result.Failure = fmt.Sprintf("need at least %d paired samples, got %d", minimumBenchmarkSamples, len(baseSamples))
			report.Benchmarks = append(report.Benchmarks, result)
			report.Passed = false
			continue
		}

		base := medianSample(baseSamples)
		head := medianSample(candidateSamples)
		result.BaselineNanos = base.Nanos
		result.CandidateNanos = head.Nanos
		result.BaselineBytes = base.Bytes
		result.CandidateBytes = head.Bytes
		result.BaselineAllocations = base.Allocations
		result.CandidateAllocations = head.Allocations

		timeRatios := make([]float64, len(baseSamples))
		byteRatios := make([]float64, len(baseSamples))
		allocationRatios := make([]float64, len(baseSamples))
		for i := range baseSamples {
			timeRatios[i] = sampleRatio(baseSamples[i].Nanos, candidateSamples[i].Nanos)
			switch {
			case candidateSamples[i].Nanos > baseSamples[i].Nanos:
				result.CandidateSlowerPairs++
				result.ComparableTimePairs++
			case candidateSamples[i].Nanos < baseSamples[i].Nanos:
				result.ComparableTimePairs++
			}
			byteRatios[i] = sampleRatio(baseSamples[i].Bytes, candidateSamples[i].Bytes)
			allocationRatios[i] = sampleRatio(baseSamples[i].Allocations, candidateSamples[i].Allocations)
		}
		result.TimeRegressionPercent = regressionPercentFromRatio(median(timeRatios))
		result.TimeSignTestPValue = exactOneSidedSignTestPValue(result.CandidateSlowerPairs, result.ComparableTimePairs)
		result.TimeSignificant = result.TimeSignTestPValue <= timeSignificanceAlpha
		result.BytesRegression = regressionPercentFromRatio(median(byteRatios))
		result.AllocationRegression = regressionPercentFromRatio(median(allocationRatios))
		effectiveTimePercent := manifest.Thresholds.TimePercent
		if w := activeWaiver(manifest, result.Name, nowFunc()); w != nil {
			effectiveTimePercent = w.MaxTimePercent
			if result.TimeRegressionPercent > manifest.Thresholds.TimePercent {
				result.WaiverApplied = fmt.Sprintf("%s (cap %.0f%%, until %s)", w.Reason, w.MaxTimePercent, w.Until)
			}
		}
		timeRegression := result.TimeRegressionPercent > effectiveTimePercent && result.TimeSignificant
		result.Passed = !timeRegression &&
			result.BytesRegression <= manifest.Thresholds.BytesPercent &&
			result.AllocationRegression <= manifest.Thresholds.AllocationsPercent
		if !result.Passed {
			report.Passed = false
		}
		report.Benchmarks = append(report.Benchmarks, result)
		report.ObservedScenarioCount++
	}
	if report.ExpectedScenarioCount == 0 || report.ObservedScenarioCount != report.ExpectedScenarioCount {
		report.Passed = false
	}
	evidence := mergeBenchmarkEvidence(baselineEvidence, candidateEvidence)
	report.CompletedSampleCount = len(evidence.completedSampleWallSeconds)
	report.DeadlineStatus = evidence.stopStatus
	for _, seconds := range evidence.completedSampleWallSeconds {
		if seconds > report.MaxSampleWallSeconds {
			report.MaxSampleWallSeconds = seconds
		}
	}
	report.InfrastructureFailure = benchmarkDeadlineIsInfrastructure(
		evidence.stopStatus,
		evidence.stopCause,
		report.ObservedScenarioCount,
		report.ExpectedScenarioCount,
		evidence.completedSampleWallSeconds,
		evidence.sampleTimeoutSeconds,
	)
	if !report.Passed {
		if report.InfrastructureFailure {
			return report, fmt.Errorf("CI-INFRA-FAILURE: benchmark deadline under severe runner contention: observed %d/%d scenarios", report.ObservedScenarioCount, report.ExpectedScenarioCount)
		}
		return report, fmt.Errorf("benchmark gate failed: observed %d/%d scenarios", report.ObservedScenarioCount, report.ExpectedScenarioCount)
	}
	return report, nil
}

func parseBenchmarks(reader io.Reader) (map[string][]benchmarkSample, benchmarkEvidence, error) {
	result := make(map[string][]benchmarkSample)
	evidence := benchmarkEvidence{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if match := benchmarkEvidencePattern.FindStringSubmatch(scanner.Text()); match != nil {
			parseBenchmarkEvidence(match[1], strings.Fields(match[2]), &evidence)
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		match := benchmarkNamePattern.FindStringSubmatch(fields[0])
		if match == nil {
			continue
		}
		sample := benchmarkSample{}
		var nanosSeen, bytesSeen, allocationsSeen bool
		for i := 1; i+1 < len(fields); i++ {
			value, parseErr := strconv.ParseFloat(fields[i], 64)
			if parseErr != nil {
				continue
			}
			switch fields[i+1] {
			case "ns/op":
				sample.Nanos = value
				nanosSeen = true
			case "B/op":
				sample.Bytes = value
				bytesSeen = true
			case "allocs/op":
				sample.Allocations = value
				allocationsSeen = true
			}
		}
		if !nanosSeen || sample.Nanos <= 0 {
			return nil, benchmarkEvidence{}, fmt.Errorf("benchmark %q has no positive ns/op sample", match[1])
		}
		if !bytesSeen || !allocationsSeen || sample.Bytes < 0 || sample.Allocations < 0 {
			return nil, benchmarkEvidence{}, fmt.Errorf("benchmark %q has incomplete allocation metrics", match[1])
		}
		result[match[1]] = append(result[match[1]], sample)
	}
	return result, evidence, scanner.Err()
}

func parseBenchmarkEvidence(kind string, fields []string, evidence *benchmarkEvidence) {
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			values[key] = value
		}
	}
	if kind == "SAMPLE" {
		if seconds, err := strconv.ParseFloat(values["wall_seconds"], 64); err == nil && seconds >= 0 {
			evidence.completedSampleWallSeconds = append(evidence.completedSampleWallSeconds, seconds)
		}
		return
	}
	evidence.stopStatus, _ = strconv.Atoi(values["status"])
	evidence.stopCause = values["cause"]
	evidence.sampleTimeoutSeconds, _ = strconv.ParseFloat(values["sample_timeout_seconds"], 64)
}

func mergeBenchmarkEvidence(left, right benchmarkEvidence) benchmarkEvidence {
	merged := left
	merged.completedSampleWallSeconds = append(merged.completedSampleWallSeconds, right.completedSampleWallSeconds...)
	if right.stopStatus != 0 {
		merged.stopStatus = right.stopStatus
		merged.stopCause = right.stopCause
		merged.sampleTimeoutSeconds = right.sampleTimeoutSeconds
	}
	return merged
}

// benchmarkDeadlineIsInfrastructure identifies only partial benchmark runs
// whose completed samples consumed at least 80% of the existing per-sample
// timeout. This derives the slowdown boundary from the CI budget instead of a
// second wall-clock constant; zero-progress, fast-partial, sample-timeout,
// complete, and ordinary benchmark failures remain code-verdict failures.
func benchmarkDeadlineIsInfrastructure(status int, cause string, observed, expected int, completed []float64, sampleTimeoutSeconds float64) bool {
	if status != 124 || cause != "deadline" || observed <= 0 || observed >= expected || sampleTimeoutSeconds <= 0 {
		return false
	}
	severeSlowdownSeconds := sampleTimeoutSeconds * 0.8
	if len(completed) == 0 {
		return false
	}
	sampleWallTimes := append([]float64(nil), completed...)
	return median(sampleWallTimes) >= severeSlowdownSeconds
}

func medianSample(samples []benchmarkSample) benchmarkSample {
	nanos := make([]float64, 0, len(samples))
	bytes := make([]float64, 0, len(samples))
	allocations := make([]float64, 0, len(samples))
	for _, sample := range samples {
		nanos = append(nanos, sample.Nanos)
		bytes = append(bytes, sample.Bytes)
		allocations = append(allocations, sample.Allocations)
	}
	return benchmarkSample{Nanos: median(nanos), Bytes: median(bytes), Allocations: median(allocations)}
}

func median(values []float64) float64 {
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func sampleRatio(baseline, candidate float64) float64 {
	if baseline == 0 {
		if candidate == 0 {
			return 1
		}
		return math.Inf(1)
	}
	return candidate / baseline
}

func regressionPercentFromRatio(ratio float64) float64 {
	if math.IsNaN(ratio) || math.IsInf(ratio, 1) {
		return math.MaxFloat64
	}
	return (ratio - 1) * 100
}

// exactOneSidedSignTestPValue returns P(X >= positive) for X distributed as
// Binomial(trials, 0.5). It tests whether the candidate is consistently slower
// without assuming normally distributed benchmark timings. Exact ties are
// excluded from trials by the caller.
func exactOneSidedSignTestPValue(positive, trials int) float64 {
	if trials <= 0 || positive <= 0 || positive > trials {
		return 1
	}

	term := math.Pow(0.5, float64(trials))
	pValue := 0.0
	for successes := 0; successes <= trials; successes++ {
		if successes >= positive {
			pValue += term
		}
		if successes < trials {
			term *= float64(trials-successes) / float64(successes+1)
		}
	}
	return math.Min(1, pValue)
}
