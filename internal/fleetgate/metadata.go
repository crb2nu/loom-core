package fleetgate

import (
	"fmt"
	"runtime"
	"sort"
)

type EvidenceMetadata struct {
	SchemaVersion  string         `json:"schema_version"`
	SuiteVersion   int            `json:"suite_version"`
	BuildSHA       string         `json:"build_sha"`
	Dirty          bool           `json:"dirty"`
	BaseSHA        string         `json:"base_sha"`
	ConfigDigest   string         `json:"config_digest"`
	SchemaDigest   string         `json:"schema_digest"`
	Command        string         `json:"command"`
	GoVersion      string         `json:"go_version"`
	GOOS           string         `json:"goos"`
	GOARCH         string         `json:"goarch"`
	ScenarioCounts map[string]int `json:"scenario_counts"`
	ScenarioTotal  int            `json:"scenario_total"`
}

func NewEvidenceMetadata(manifest Manifest, buildSHA, baseSHA string, dirty bool, configDigest, schemaDigest, command, goVersion string, counts map[string]int) (EvidenceMetadata, error) {
	if buildSHA == "" || baseSHA == "" || configDigest == "" || schemaDigest == "" || command == "" {
		return EvidenceMetadata{}, fmt.Errorf("evidence metadata requires build/base SHA, config/schema digests, and command")
	}
	metadata := EvidenceMetadata{
		SchemaVersion:  ReportSchema,
		SuiteVersion:   manifest.SuiteVersion,
		BuildSHA:       buildSHA,
		Dirty:          dirty,
		BaseSHA:        baseSHA,
		ConfigDigest:   configDigest,
		SchemaDigest:   schemaDigest,
		Command:        command,
		GoVersion:      goVersion,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		ScenarioCounts: make(map[string]int, len(counts)),
	}
	expectedCounts := manifest.ScenarioCounts()
	if len(counts) == 0 {
		counts = expectedCounts
	}
	if len(counts) != len(expectedCounts) {
		return EvidenceMetadata{}, fmt.Errorf("scenario counts cover %d groups, want %d", len(counts), len(expectedCounts))
	}
	keys := make([]string, 0, len(counts))
	for name := range counts {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		count := counts[name]
		expected, ok := expectedCounts[name]
		if name == "" || count <= 0 || !ok || count != expected {
			return EvidenceMetadata{}, fmt.Errorf("scenario count %q = %d, want %d", name, count, expected)
		}
		metadata.ScenarioCounts[name] = count
		metadata.ScenarioTotal += count
	}
	if metadata.ScenarioTotal == 0 {
		return EvidenceMetadata{}, fmt.Errorf("evidence metadata requires nonzero scenario counts")
	}
	return metadata, nil
}
