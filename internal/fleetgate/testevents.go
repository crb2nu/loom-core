package fleetgate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

type ScenarioResult struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Runs    int    `json:"runs"`
	Passes  int    `json:"passes"`
	Fails   int    `json:"fails"`
	Skips   int    `json:"skips"`
}

type TestReport struct {
	SchemaVersion         string           `json:"schema_version"`
	SuiteVersion          int              `json:"suite_version"`
	Group                 string           `json:"group"`
	RaceEnabled           bool             `json:"race_enabled"`
	ExpectedScenarioCount int              `json:"expected_scenario_count"`
	ObservedScenarioCount int              `json:"observed_scenario_count"`
	Scenarios             []ScenarioResult `json:"scenarios"`
	UnexpectedTests       []string         `json:"unexpected_tests,omitempty"`
	UnexpectedSkips       []string         `json:"unexpected_skips,omitempty"`
	PackageFailures       []string         `json:"package_failures,omitempty"`
	Passed                bool             `json:"passed"`
}

func VerifyTestEvents(manifest Manifest, groupName string, reader io.Reader) (TestReport, error) {
	group, err := manifest.Group(groupName)
	if err != nil {
		return TestReport{}, err
	}

	report := TestReport{
		SchemaVersion:         ReportSchema,
		SuiteVersion:          manifest.SuiteVersion,
		Group:                 group.Name,
		RaceEnabled:           group.RaceEnabled,
		ExpectedScenarioCount: len(group.Scenarios),
	}

	results := make(map[string]*ScenarioResult, len(group.Scenarios))
	manifestedRoots := make(map[string]struct{}, len(group.Scenarios))
	for _, scenario := range group.Scenarios {
		key := scenario.Package + "\x00" + scenario.Test
		results[key] = &ScenarioResult{Package: scenario.Package, Test: scenario.Test}
		manifestedRoots[key] = struct{}{}
	}
	unexpectedTests := make(map[string]struct{})

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return TestReport{}, fmt.Errorf("decode go test event: %w", err)
		}

		if event.Test == "" {
			if event.Action == "fail" && event.Package != "" {
				report.PackageFailures = append(report.PackageFailures, event.Package)
			}
			continue
		}
		if event.Action == "skip" {
			report.UnexpectedSkips = append(report.UnexpectedSkips, event.Package+"/"+event.Test)
		}

		result := results[event.Package+"\x00"+event.Test]
		if result == nil {
			root, _, isSubtest := strings.Cut(event.Test, "/")
			if _, allowedSubtest := manifestedRoots[event.Package+"\x00"+root]; isSubtest && allowedSubtest {
				continue
			}
			if !isSubtest {
				unexpectedTests[event.Package+"/"+event.Test] = struct{}{}
			}
			continue
		}
		switch event.Action {
		case "run":
			result.Runs++
		case "pass":
			result.Passes++
		case "fail":
			result.Fails++
		case "skip":
			result.Skips++
		}
	}
	if err := scanner.Err(); err != nil {
		return TestReport{}, fmt.Errorf("read go test events: %w", err)
	}

	for _, result := range results {
		report.Scenarios = append(report.Scenarios, *result)
		if result.Runs == 1 && result.Passes == 1 && result.Fails == 0 && result.Skips == 0 {
			report.ObservedScenarioCount++
		}
	}
	for test := range unexpectedTests {
		report.UnexpectedTests = append(report.UnexpectedTests, test)
	}
	sort.Strings(report.UnexpectedTests)
	sort.Slice(report.Scenarios, func(i, j int) bool {
		if report.Scenarios[i].Package == report.Scenarios[j].Package {
			return report.Scenarios[i].Test < report.Scenarios[j].Test
		}
		return report.Scenarios[i].Package < report.Scenarios[j].Package
	})
	sort.Strings(report.UnexpectedSkips)
	sort.Strings(report.PackageFailures)

	report.Passed = report.ExpectedScenarioCount > 0 &&
		report.ObservedScenarioCount == report.ExpectedScenarioCount &&
		len(report.UnexpectedTests) == 0 &&
		len(report.UnexpectedSkips) == 0 &&
		len(report.PackageFailures) == 0
	if !report.Passed {
		return report, fmt.Errorf("test group %q failed: observed %d/%d required scenarios, unexpected=%d, skips=%d, package_failures=%d",
			group.Name, report.ObservedScenarioCount, report.ExpectedScenarioCount,
			len(report.UnexpectedTests), len(report.UnexpectedSkips), len(report.PackageFailures))
	}
	return report, nil
}
