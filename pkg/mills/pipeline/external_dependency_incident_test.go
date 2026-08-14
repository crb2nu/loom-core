package pipeline_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/contracts"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

func TestExternalDependencyIncidentClassificationContract(t *testing.T) {
	t.Parallel()

	if got, want := contracts.ExternalDependencyIncidentClassification, "external_dependency_incident"; got != want {
		t.Fatalf("ExternalDependencyIncidentClassification = %q, want %q", got, want)
	}

	if got := string(contracts.CouncilCIIncidentExternalDependency); got != contracts.ExternalDependencyIncidentClassification {
		t.Fatalf("Council contract external class = %q, want %q", got, contracts.ExternalDependencyIncidentClassification)
	}

	classification, ok := council.ClassifyExternalDependencyIncident(council.CIBranchEvidence{
		ChangedFiles: []string{"pkg/mills/pipeline/runner.go"},
	}, []council.CIFailureEvidence{{
		JobName:              "ci_watch",
		Stage:                "merge",
		ErrorLine:            "gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
		RecursAcrossBranches: true,
	}})
	if !ok {
		t.Fatal("ClassifyExternalDependencyIncident returned ok=false")
	}
	if got := string(classification.Class); got != contracts.ExternalDependencyIncidentClassification {
		t.Fatalf("council classifier class = %q, want %q", got, contracts.ExternalDependencyIncidentClassification)
	}

	rendered, ok := council.FormatExternalEscalation(council.ExternalEscalationRenderInput{
		Reason: "gitlab: status 401: unauthorized: authentication failed",
	})
	if !ok {
		t.Fatal("FormatExternalEscalation returned ok=false")
	}
	wantLine := fmt.Sprintf("**Incident class**: `%s`", contracts.ExternalDependencyIncidentClassification)
	if !strings.Contains(rendered.Markdown, wantLine) {
		t.Fatalf("rendered escalation missing %q:\n%s", wantLine, rendered.Markdown)
	}

	record := pipeline.ClassifyFailureRecord(errors.New("gitlab: status 401: unauthorized: authentication failed"))
	if record.Class != pipeline.FailureConfiguration || record.Retryable || !record.Terminal {
		t.Fatalf("pipeline failure record = %+v, want terminal configuration external incident", record)
	}
	if record.ExternalDependencyID == "" || record.ExternalDependency != "gitlab" {
		t.Fatalf("external dependency metadata = id %q dependency %q, want gitlab incident", record.ExternalDependencyID, record.ExternalDependency)
	}
}
