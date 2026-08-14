package fleetgate

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	ManifestSchema = "loom.fleet-reliability.suite/v1"
	ReportSchema   = "loom.fleet-reliability.report/v1"
)

type Manifest struct {
	SchemaVersion string              `json:"schema_version"`
	SuiteVersion  int                 `json:"suite_version"`
	Thresholds    BenchmarkThresholds `json:"thresholds"`
	TestGroups    []TestGroup         `json:"test_groups"`
	Benchmarks    []string            `json:"benchmarks"`
}

type TestGroup struct {
	Name        string         `json:"name"`
	RaceEnabled bool           `json:"race_enabled"`
	Scenarios   []TestScenario `json:"scenarios"`
}

type TestScenario struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}

type BenchmarkThresholds struct {
	TimePercent        float64 `json:"time_percent"`
	BytesPercent       float64 `json:"bytes_percent"`
	AllocationsPercent float64 `json:"allocations_percent"`
}

// TestPlan is the executable test selection derived from one manifest group.
// Packages retain their import paths so the plan can be passed directly to
// go test from any directory in the module.
type TestPlan struct {
	Regex    string
	Packages []string
}

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchema {
		return fmt.Errorf("manifest schema_version = %q, want %q", m.SchemaVersion, ManifestSchema)
	}
	if m.SuiteVersion < 1 {
		return fmt.Errorf("manifest requires a positive suite_version")
	}
	if m.Thresholds.TimePercent <= 0 || m.Thresholds.BytesPercent <= 0 || m.Thresholds.AllocationsPercent <= 0 {
		return fmt.Errorf("manifest benchmark thresholds must be positive")
	}
	groups := make(map[string]struct{}, len(m.TestGroups))
	for _, group := range m.TestGroups {
		if group.Name == "" || len(group.Scenarios) == 0 {
			return fmt.Errorf("test group requires a name and at least one scenario")
		}
		if _, duplicate := groups[group.Name]; duplicate {
			return fmt.Errorf("duplicate test group %q", group.Name)
		}
		groups[group.Name] = struct{}{}
		scenarios := make(map[string]struct{}, len(group.Scenarios))
		for _, scenario := range group.Scenarios {
			key := scenario.Package + "\x00" + scenario.Test
			if scenario.Package == "" || scenario.Test == "" {
				return fmt.Errorf("group %q contains an incomplete scenario", group.Name)
			}
			if _, duplicate := scenarios[key]; duplicate {
				return fmt.Errorf("group %q contains duplicate scenario %s/%s", group.Name, scenario.Package, scenario.Test)
			}
			scenarios[key] = struct{}{}
		}
	}
	if len(m.Benchmarks) == 0 {
		return fmt.Errorf("manifest requires at least one benchmark")
	}
	benchmarks := make(map[string]struct{}, len(m.Benchmarks))
	for _, benchmark := range m.Benchmarks {
		if benchmark == "" {
			return fmt.Errorf("manifest contains an empty benchmark name")
		}
		if _, duplicate := benchmarks[benchmark]; duplicate {
			return fmt.Errorf("duplicate benchmark %q", benchmark)
		}
		benchmarks[benchmark] = struct{}{}
	}
	return nil
}

func (m Manifest) Group(name string) (TestGroup, error) {
	for _, group := range m.TestGroups {
		if group.Name == name {
			return group, nil
		}
	}
	return TestGroup{}, fmt.Errorf("test group %q not found", name)
}

// Plan returns an anchored test regex and stable, de-duplicated package list
// for a manifest group. Keeping this derivation beside manifest validation
// prevents the CI runner from silently selecting fewer scenarios than the
// verifier expects.
func (m Manifest) Plan(name string) (TestPlan, error) {
	group, err := m.Group(name)
	if err != nil {
		return TestPlan{}, err
	}

	tests := make([]string, 0, len(group.Scenarios))
	packages := make([]string, 0, len(group.Scenarios))
	seenPackages := make(map[string]struct{}, len(group.Scenarios))
	for _, scenario := range group.Scenarios {
		tests = append(tests, regexp.QuoteMeta(scenario.Test))
		if _, seen := seenPackages[scenario.Package]; seen {
			continue
		}
		seenPackages[scenario.Package] = struct{}{}
		packages = append(packages, scenario.Package)
	}

	return TestPlan{
		Regex:    "^(" + strings.Join(tests, "|") + ")$",
		Packages: packages,
	}, nil
}

// ScenarioCounts returns the authoritative evidence counts for every test
// group plus the benchmark group.
func (m Manifest) ScenarioCounts() map[string]int {
	counts := make(map[string]int, len(m.TestGroups)+1)
	for _, group := range m.TestGroups {
		counts[group.Name] = len(group.Scenarios)
	}
	counts["benchmark"] = len(m.Benchmarks)
	return counts
}
