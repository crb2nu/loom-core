package council_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestIncidentContextsFromClassifiedCIFailures_NormalizesExternalDependency(t *testing.T) {
	t.Parallel()

	retryable := false
	freeRetry := false
	terminal := true
	contexts := council.IncidentContextsFromClassifiedCIFailures([]*store.ClassifiedCIFailureSummary{{
		RunID:                "PIPE-17",
		BacklogID:            "MILLS-17",
		BacklogTitle:         "GitLab auth incident",
		Classifier:           pipeline.FailureClassifierName,
		EscalationClass:      "config",
		FailureClass:         string(pipeline.FailureConfiguration),
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
		Retryable:            &retryable,
		FreeRetry:            &freeRetry,
		Terminal:             &terminal,
	}})

	if len(contexts) != 1 {
		t.Fatalf("contexts = %d, want 1", len(contexts))
	}
	got := contexts[0]
	if got.Class != council.CIIncidentExternalDependency {
		t.Fatalf("Class = %q, want %q", got.Class, council.CIIncidentExternalDependency)
	}
	if got.Disposition != council.CIIncidentDispositionWaitDependency {
		t.Fatalf("Disposition = %q, want wait-for-dependency", got.Disposition)
	}
	if got.ExternalDependency != "gitlab" || got.ExternalDependencyID == "" {
		t.Fatalf("external dependency = %q id %q, want gitlab + id", got.ExternalDependency, got.ExternalDependencyID)
	}
	if got.Retryable == nil || *got.Retryable {
		t.Fatalf("Retryable = %v, want false", got.Retryable)
	}

	rendered := council.RenderIncidentPlanningContext(contexts)
	for _, want := range []string{
		"Incident classification metadata",
		"GitLab auth incident",
		"run=`PIPE-17`",
		"class=`external_dependency_incident`",
		"disposition=`wait_for_dependency_recovery`",
		"classifier=`mills-failure-classifier`",
		"failure_class=`configuration`",
		"retryable=`false`",
		"free_retry=`false`",
		"terminal=`true`",
		"external=`gitlab`",
		"do not propose outside-system remediation",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered context missing %q:\n%s", want, rendered)
		}
	}
}

func TestPipelineIncidentContextFromFailureClassification_MapsLiveRecord(t *testing.T) {
	t.Parallel()

	classification := pipeline.ClassifyFailureRecord(errors.New("gitlab: status 401: unauthorized: authentication failed"))
	got := pipeline.IncidentContextFromFailureClassification("pipeline_input", classification)

	if got.Source != "pipeline_input" {
		t.Fatalf("Source = %q, want pipeline_input", got.Source)
	}
	if got.Class != council.CIIncidentExternalDependency {
		t.Fatalf("Class = %q, want %q", got.Class, council.CIIncidentExternalDependency)
	}
	if got.Disposition != council.CIIncidentDispositionWaitDependency {
		t.Fatalf("Disposition = %q, want wait-for-dependency", got.Disposition)
	}
	if got.FailureClass != string(pipeline.FailureConfiguration) {
		t.Fatalf("FailureClass = %q, want configuration", got.FailureClass)
	}
	if got.Classifier != pipeline.FailureClassifierName {
		t.Fatalf("Classifier = %q, want %q", got.Classifier, pipeline.FailureClassifierName)
	}
	if got.Retryable == nil || *got.Retryable {
		t.Fatalf("Retryable = %v, want false", got.Retryable)
	}
	if got.Terminal == nil || !*got.Terminal {
		t.Fatalf("Terminal = %v, want true", got.Terminal)
	}
}

func TestCompile_AddsIncidentClassificationPlanningContext(t *testing.T) {
	t.Parallel()

	st := newIncidentContextStore(t)
	ctx := context.Background()
	retryable := false
	item := &store.BacklogItem{
		ID: "MILLS-CI-AUTH", Title: "GitLab auth outage follow-up",
		State: store.BacklogEscalated, Priority: store.P1, CreatedBy: "test",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	if err := st.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-CI-AUTH", BacklogID: item.ID, Template: "t",
		State: store.PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
		StartedAt:       time.Date(2026, 7, 12, 5, 0, 0, 0, time.UTC),
		EscalationClass: "config", FailureClass: "configuration",
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
		EscalationRetryable:  &retryable,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}

	brief, err := council.Compile(ctx, council.BriefSources{
		Store: st,
		Now: func() time.Time {
			return time.Date(2026, 7, 12, 6, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	classifiedIdx := strings.Index(brief.Markdown, "## Classified CI failures")
	planningIdx := strings.Index(brief.Markdown, "## Incident classification planning context")
	if classifiedIdx < 0 || planningIdx < 0 {
		t.Fatalf("brief missing classified/planning sections:\n%s", brief.Markdown)
	}
	if planningIdx <= classifiedIdx {
		t.Fatalf("planning context should follow classified failures: classified=%d planning=%d", classifiedIdx, planningIdx)
	}
	for _, want := range []string{
		"class=`external_dependency_incident`",
		"disposition=`wait_for_dependency_recovery`",
		"external=`gitlab`",
		"do not propose outside-system remediation",
	} {
		if !strings.Contains(brief.Markdown, want) {
			t.Fatalf("brief missing %q:\n%s", want, brief.Markdown)
		}
	}
}

func newIncidentContextStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "mills.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
