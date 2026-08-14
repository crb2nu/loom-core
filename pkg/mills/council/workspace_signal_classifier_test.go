package council

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestClassifyInfrastructureWorkspaceSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		signal     WorkspaceSignal
		wantMatch  bool
		dependency string
	}{
		{
			name: "gitlab API outage",
			signal: WorkspaceSignal{
				Service: "platform/gitlab-runner",
				Sample:  "gitlab.com request failed: status 503",
			},
			wantMatch:  true,
			dependency: "gitlab",
		},
		{
			name: "kubernetes API timeout",
			signal: WorkspaceSignal{
				Service: "kube-system/controller",
				Sample:  "kubernetes api server request failed: i/o timeout",
			},
			wantMatch:  true,
			dependency: "kubernetes",
		},
		{
			name: "flux upstream outage",
			signal: WorkspaceSignal{
				Service: "flux-system/source-controller",
				Sample:  "GitLab fetch failed: status 502",
			},
			wantMatch:  true,
			dependency: "git_provider",
		},
		{
			name: "observability backend outage",
			signal: WorkspaceSignal{
				Service: "observability/loki-gateway",
				Sample:  "loki query failed: connection refused",
			},
			wantMatch:  true,
			dependency: "observability",
		},
		{
			name: "signature without infrastructure namespace",
			signal: WorkspaceSignal{
				Service: "loom-core/unit-tests",
				Sample:  "kubernetes api server request failed: i/o timeout",
			},
		},
		{
			name: "infrastructure namespace without outage signature",
			signal: WorkspaceSignal{
				Service: "kube-system/controller",
				Sample:  "repository validation failed: malformed JSON",
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, matched := ClassifyInfrastructureWorkspaceSignal(tc.signal)
			if matched != tc.wantMatch {
				t.Fatalf("matched = %t, want %t: %+v", matched, tc.wantMatch, got)
			}
			if !tc.wantMatch {
				if got.IncidentClass != "" || got.ExternalDependency != "" {
					t.Fatalf("unmatched signal was stamped: %+v", got)
				}
				return
			}
			if got.IncidentClass != CIIncidentClass(store.IncidentClassExternalDependency) {
				t.Fatalf("class = %q, want %q", got.IncidentClass, store.IncidentClassExternalDependency)
			}
			if got.ExternalDependency != tc.dependency {
				t.Fatalf("dependency = %q, want %q", got.ExternalDependency, tc.dependency)
			}
		})
	}
}

func TestClassifyInfrastructureWorkspaceSignal_PreservesExistingClass(t *testing.T) {
	t.Parallel()

	input := WorkspaceSignal{
		Service:            "kube-system/controller",
		Sample:             "kubernetes api server: connection refused",
		IncidentClass:      CIIncidentRepositoryRegression,
		ExternalDependency: "existing",
	}
	got, matched := ClassifyInfrastructureWorkspaceSignal(input)
	if matched {
		t.Fatal("preclassified signal unexpectedly matched")
	}
	if got != input {
		t.Fatalf("preclassified signal changed: got %+v, want %+v", got, input)
	}
}

func TestCompile_ClassifiesInfrastructureWorkspaceSignalsAtBriefCompileTime(t *testing.T) {
	t.Parallel()

	st := newCouncilTestStore(t)
	brief, err := Compile(context.Background(), BriefSources{
		Store: st,
		Signals: stubSource{sigs: []WorkspaceSignal{{
			Source:  "loki",
			Service: "kube-system/kube-controller-manager",
			Count:   9,
			Sample:  "kubernetes api server request failed: tls handshake timeout",
		}}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	for _, want := range []string{
		"class=`external_dependency_incident`",
		"external=`kubernetes`",
	} {
		if !strings.Contains(brief.Markdown, want) {
			t.Fatalf("brief missing persisted classification %q:\n%s", want, brief.Markdown)
		}
	}
}

func TestInfrastructureWorkspaceSignalRules_HaveStableUniqueIDs(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, rule := range InfrastructureWorkspaceSignalRules() {
		if rule.ID == "" || rule.NamespacePattern == "" || rule.LogPattern == "" || rule.ExternalDependency == "" {
			t.Fatalf("incomplete rule: %+v", rule)
		}
		if seen[rule.ID] {
			t.Fatalf("duplicate rule ID %q", rule.ID)
		}
		seen[rule.ID] = true
	}
}

func TestInfrastructureWorkspaceSignalRules_RunbookIsInSync(t *testing.T) {
	t.Parallel()

	const (
		runbookPath = "../../../docs/runbook-external-dependency-incidents.md"
		beginMarker = "<!-- BEGIN INFRASTRUCTURE WORKSPACE SIGNAL RULES -->"
		endMarker   = "<!-- END INFRASTRUCTURE WORKSPACE SIGNAL RULES -->"
	)
	raw, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read classifier runbook: %v", err)
	}
	runbook := string(raw)
	if strings.Count(runbook, beginMarker) != 1 || strings.Count(runbook, endMarker) != 1 {
		t.Fatalf("runbook must contain exactly one classifier rule table")
	}
	begin := strings.Index(runbook, beginMarker)
	end := strings.Index(runbook, endMarker)
	if begin < 0 || end < 0 || end <= begin {
		t.Fatalf("runbook must contain one ordered classifier rule table between %q and %q", beginMarker, endMarker)
	}

	documented := make(map[string]string)
	for _, line := range strings.Split(runbook[begin+len(beginMarker):end], "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 8 {
			t.Fatalf("classifier runbook row has %d cells, want 8: %s", len(cells), line)
		}
		id := strings.Trim(strings.TrimSpace(cells[1]), "`")
		dependency := strings.Trim(strings.TrimSpace(cells[3]), "`")
		if _, exists := documented[id]; exists {
			t.Fatalf("classifier runbook contains duplicate rule ID %q", id)
		}
		documented[id] = dependency
	}

	rules := InfrastructureWorkspaceSignalRules()
	if len(documented) != len(rules) {
		t.Fatalf("classifier runbook documents %d rules, classifier defines %d; documented=%v", len(documented), len(rules), documented)
	}
	for _, rule := range rules {
		dependency, ok := documented[rule.ID]
		if !ok {
			t.Errorf("classifier rule %q is missing from runbook table", rule.ID)
			continue
		}
		if dependency != rule.ExternalDependency {
			t.Errorf("classifier rule %q dependency = %q in runbook, want %q", rule.ID, dependency, rule.ExternalDependency)
		}
		sectionHeading := "### `" + rule.ID + "` source patterns"
		if strings.Count(runbook, sectionHeading) != 1 {
			t.Errorf("classifier rule %q must have exactly one source-pattern section", rule.ID)
			continue
		}
		sectionStart := strings.Index(runbook, sectionHeading) + len(sectionHeading)
		sectionEnd := strings.Index(runbook[sectionStart:], "\n### ")
		if sectionEnd < 0 {
			sectionEnd = len(runbook)
		} else {
			sectionEnd += sectionStart
		}
		section := runbook[sectionStart:sectionEnd]
		for field, value := range map[string]string{
			"namespace/service pattern": rule.NamespacePattern,
			"log pattern":               rule.LogPattern,
		} {
			if !strings.Contains(section, "\n"+value+"\n") {
				t.Errorf("classifier rule %q %s is missing or stale in runbook: %q", rule.ID, field, value)
			}
		}
		delete(documented, rule.ID)
	}
	for staleID := range documented {
		t.Errorf("runbook documents stale classifier rule %q", staleID)
	}

	const metadataHeadingPrefix = "### `"
	for _, line := range strings.Split(runbook, "\n") {
		if !strings.HasPrefix(line, metadataHeadingPrefix) || !strings.HasSuffix(line, "` source patterns") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(line, metadataHeadingPrefix), "` source patterns")
		found := false
		for _, rule := range rules {
			if rule.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("runbook contains stale source-pattern section for classifier rule %q", id)
		}
	}
}
