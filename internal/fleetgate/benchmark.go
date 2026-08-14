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
)

var benchmarkNamePattern = regexp.MustCompile(`^(Benchmark\S+)-\d+$`)

const (
	minimumBenchmarkSamples = 11
	timeSignificanceAlpha   = 0.05
)

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
	Passed                bool                `json:"passed"`
}

func CompareBenchmarks(manifest Manifest, baselineReader, candidateReader io.Reader) (BenchmarkReport, error) {
	baseline, err := parseBenchmarks(baselineReader)
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("parse baseline: %w", err)
	}
	candidate, err := parseBenchmarks(candidateReader)
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
		timeRegression := result.TimeRegressionPercent > manifest.Thresholds.TimePercent && result.TimeSignificant
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
	if !report.Passed {
		return report, fmt.Errorf("benchmark gate failed: observed %d/%d scenarios", report.ObservedScenarioCount, report.ExpectedScenarioCount)
	}
	return report, nil
}

func parseBenchmarks(reader io.Reader) (map[string][]benchmarkSample, error) {
	result := make(map[string][]benchmarkSample)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
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
			return nil, fmt.Errorf("benchmark %q has no positive ns/op sample", match[1])
		}
		if !bytesSeen || !allocationsSeen || sample.Bytes < 0 || sample.Allocations < 0 {
			return nil, fmt.Errorf("benchmark %q has incomplete allocation metrics", match[1])
		}
		result[match[1]] = append(result[match[1]], sample)
	}
	return result, scanner.Err()
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
