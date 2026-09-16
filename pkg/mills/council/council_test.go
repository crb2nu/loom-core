package council

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type reviewerOutcomeObservation struct {
	outcome        string
	classification string
}

type reviewerOutcomeRecorder struct {
	observations []reviewerOutcomeObservation
}

func (r *reviewerOutcomeRecorder) RecordCouncilReviewerOutcome(_ context.Context, outcome, classification string) {
	r.observations = append(r.observations, reviewerOutcomeObservation{outcome: outcome, classification: classification})
}

type reviewerStatusError int

func (e reviewerStatusError) Error() string   { return "provider request failed" }
func (e reviewerStatusError) StatusCode() int { return int(e) }

func TestEvaluateReviewerOutcomesClassifiesAndRecordsEachRoute(t *testing.T) {
	tests := []struct {
		name           string
		output         ReviewerOutput
		status         ReviewerOutcomeStatus
		classification ReviewerOutcomeClassification
	}{
		{name: "success", output: ReviewerOutput{Markdown: "useful"}, status: ReviewerOutcomeOK, classification: ReviewerClassificationNone},
		{name: "empty", output: ReviewerOutput{Markdown: " \n\t"}, status: ReviewerOutcomeEmpty, classification: ReviewerClassificationNone},
		{name: "generic error", output: ReviewerOutput{Err: errors.New("provider broke")}, status: ReviewerOutcomeError, classification: ReviewerClassificationFailure},
		{name: "timeout", output: ReviewerOutput{Err: context.DeadlineExceeded}, status: ReviewerOutcomeTimeout, classification: ReviewerClassificationFailure},
		{name: "auth error", output: ReviewerOutput{Err: reviewerStatusError(401)}, status: ReviewerOutcomeError, classification: ReviewerClassificationExternalDependencyIncident},
		{name: "quota error", output: ReviewerOutput{Err: errors.New("quota exceeded for model")}, status: ReviewerOutcomeError, classification: ReviewerClassificationExternalDependencyIncident},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &reviewerOutcomeRecorder{}
			restore := telemetry.SetCouncilReviewerOutcomeRecorderForTest(recorder)
			defer restore()
			tc.output.Lens.Name = "reviewer"
			outputs := []ReviewerOutput{{Lens: ReviewerLens{Name: "fallback"}, Markdown: "usable"}, tc.output}
			outcomes, err := EvaluateReviewerOutcomes(context.Background(), outputs)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			var got ReviewerOutcome
			for _, outcome := range outcomes {
				if outcome.Reviewer == "reviewer" {
					got = outcome
				}
			}
			if got.Status != tc.status || got.Classification != tc.classification {
				t.Fatalf("outcome = %+v, want status=%q classification=%q", got, tc.status, tc.classification)
			}
			if len(recorder.observations) != len(outputs) {
				t.Fatalf("telemetry observations = %d, want %d", len(recorder.observations), len(outputs))
			}
			last := recorder.observations[len(recorder.observations)-1]
			if last.outcome != string(tc.status) || last.classification != string(tc.classification) {
				t.Fatalf("telemetry = %+v, want outcome=%q classification=%q", last, tc.status, tc.classification)
			}
		})
	}
}

func TestEvaluateReviewerOutcomesMixedResultsAndStableTable(t *testing.T) {
	recorder := &reviewerOutcomeRecorder{}
	restore := telemetry.SetCouncilReviewerOutcomeRecorderForTest(recorder)
	defer restore()

	outcomes, err := EvaluateReviewerOutcomes(context.Background(), []ReviewerOutput{
		{Lens: ReviewerLens{Name: "zeta"}, Err: errors.New("rate limit reached")},
		{Lens: ReviewerLens{Name: "alpha"}, Markdown: "usable review"},
		{Lens: ReviewerLens{Name: "middle"}, Markdown: ""},
	})
	if err != nil {
		t.Fatalf("mixed outcomes failed: %v", err)
	}
	want := "| Reviewer | Status | Classification |\n" +
		"|---|---|---|\n" +
		"| alpha | ok | none |\n" +
		"| middle | empty | none |\n" +
		"| zeta | error | external_dependency_incident |\n"
	if got := RenderReviewerStatusTable(outcomes); got != want {
		t.Fatalf("status table:\n%s\nwant:\n%s", got, want)
	}
	if got := len(recorder.observations); got != 3 {
		t.Fatalf("telemetry observations = %d, want 3", got)
	}
}

func TestEvaluateReviewerOutcomesFailsClosedWithoutUsableResult(t *testing.T) {
	tests := []struct {
		name    string
		outputs []ReviewerOutput
	}{
		{name: "empty route"},
		{name: "all empty", outputs: []ReviewerOutput{
			{Lens: ReviewerLens{Name: "a"}}, {Lens: ReviewerLens{Name: "b"}, Markdown: "  "},
		}},
		{name: "all failed", outputs: []ReviewerOutput{
			{Lens: ReviewerLens{Name: "a"}, Err: errors.New("failed")},
			{Lens: ReviewerLens{Name: "b"}, Err: context.DeadlineExceeded},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &reviewerOutcomeRecorder{}
			restore := telemetry.SetCouncilReviewerOutcomeRecorderForTest(recorder)
			defer restore()
			outcomes, err := EvaluateReviewerOutcomes(context.Background(), tc.outputs)
			if err == nil || !strings.Contains(err.Error(), "no reviewer produced") {
				t.Fatalf("error = %v, want fail-closed error", err)
			}
			if len(outcomes) != len(tc.outputs) || len(recorder.observations) != len(tc.outputs) {
				t.Fatalf("outcomes/observations = %d/%d, want %d", len(outcomes), len(recorder.observations), len(tc.outputs))
			}
		})
	}
}

func TestRenderReviewerStatusTableDoesNotMutateInput(t *testing.T) {
	in := []ReviewerOutcome{{Reviewer: "z"}, {Reviewer: "a"}}
	want := append([]ReviewerOutcome(nil), in...)
	_ = RenderReviewerStatusTable(in)
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("input mutated: got %+v want %+v", in, want)
	}
}

func TestDispatcherMarksMixedRunDegradedAndSurfacesStableEditorStatus(t *testing.T) {
	brief := newFakeBrief()
	d := &Dispatcher{Reviewers: map[string]Reviewer{
		"zeta":  &FakeReviewer{ReturnErr: errors.New("quota exhausted")},
		"alpha": &FakeReviewer{Notes: "usable"},
	}}
	outputs, err := d.Dispatch(context.Background(), brief, []ReviewerLens{
		{Name: "zeta", Model: "z", Backend: "remote"},
		{Name: "alpha", Model: "a", Backend: "local"},
	}, DispatchOptions{MinQuorum: 1})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(outputs) != 2 || !strings.Contains(brief.Markdown, ReviewerDegradedMarker) {
		t.Fatalf("outputs/degraded marker = %d/%t, want 2/true", len(outputs), strings.Contains(brief.Markdown, ReviewerDegradedMarker))
	}
	want := ReviewerDegradedMarker + "\n| Reviewer | Status | Classification |\n" +
		"|---|---|---|\n" +
		"| alpha | ok | none |\n" +
		"| zeta | error | external_dependency_incident |\n"
	if !strings.Contains(brief.Markdown, want) {
		t.Fatalf("editor brief missing stable status block:\n%s", brief.Markdown)
	}
}

func TestDispatcherFailsClosedOnWhitespaceOutput(t *testing.T) {
	brief := newFakeBrief()
	d := &Dispatcher{Reviewers: map[string]Reviewer{
		"blank": reviewerFunc(func(context.Context, *Brief, ReviewerLens) (ReviewerOutput, error) {
			return ReviewerOutput{Markdown: " \n\t"}, nil
		}),
	}}
	_, err := d.Dispatch(context.Background(), brief, []ReviewerLens{{Name: "blank"}}, DispatchOptions{MinQuorum: 1})
	if err == nil || !strings.Contains(err.Error(), "no reviewer produced") {
		t.Fatalf("error = %v, want fail-closed empty-output error", err)
	}
	if !strings.Contains(brief.Markdown, ReviewerDegradedMarker) || !strings.Contains(brief.Markdown, "| blank | empty | none |") {
		t.Fatalf("empty outcome not surfaced:\n%s", brief.Markdown)
	}
}

type reviewerFunc func(context.Context, *Brief, ReviewerLens) (ReviewerOutput, error)

func (f reviewerFunc) Review(ctx context.Context, brief *Brief, lens ReviewerLens) (ReviewerOutput, error) {
	out, err := f(ctx, brief, lens)
	out.Lens = lens
	return out, err
}

func TestWorkspaceSignalExternalCoverage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		service    string
		sample     string
		dependency string
	}{
		{
			name:       "langfuse S3 failure",
			service:    "langfuse/langfuse-web",
			sample:     "ERROR Failed to upload media to S3 bucket: connection refused",
			dependency: "s3",
		},
		{
			name:       "MinIO drive failure",
			service:    "minio/minio-0",
			sample:     "ERROR Unable to use the drive /data: drive not found",
			dependency: "minio",
		},
		{
			name:       "PostgreSQL connection refused",
			service:    "database/postgresql",
			sample:     "dial tcp 10.0.0.4:5432: connect: connection refused",
			dependency: "postgres",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, matched := ClassifyRecurringInfrastructureWorkspaceSignal(WorkspaceSignal{
				Source: "loki", Service: tc.service, Sample: tc.sample,
			})
			if !matched {
				t.Fatalf("signal did not match: %+v", got)
			}
			if got.IncidentClass != CIIncidentExternalDependency {
				t.Fatalf("class = %q, want %q", got.IncidentClass, CIIncidentExternalDependency)
			}
			if got.ExternalDependency != tc.dependency {
				t.Fatalf("dependency = %q, want %q", got.ExternalDependency, tc.dependency)
			}
		})
	}
}

func TestWorkspaceSignalExternalCoverage_NearMissesAndPreclassified(t *testing.T) {
	t.Parallel()

	for _, signal := range []WorkspaceSignal{
		{Service: "loom-core/unit-tests", Sample: "dial tcp 127.0.0.1:8080: connect: connection refused"},
		{Service: "langfuse/langfuse-web", Sample: "request completed successfully"},
		{Service: "minio/minio-0", Sample: "all drives online"},
	} {
		got, matched := ClassifyRecurringInfrastructureWorkspaceSignal(signal)
		if matched || got != signal {
			t.Fatalf("near miss was classified: matched=%t got=%+v", matched, got)
		}
	}

	preclassified := WorkspaceSignal{
		Service: "database/postgresql", Sample: "connection refused",
		IncidentClass: CIIncidentRepositoryRegression, ExternalDependency: "existing",
	}
	got, matched := ClassifyRecurringInfrastructureWorkspaceSignal(preclassified)
	if matched || got != preclassified {
		t.Fatalf("preclassified signal changed: matched=%t got=%+v", matched, got)
	}
}

type roadmapIntentListerFunc func(context.Context) ([]*store.RoadmapIntent, error)

func (f roadmapIntentListerFunc) List(ctx context.Context) ([]*store.RoadmapIntent, error) {
	return f(ctx)
}

func TestIntentStorePreflightEmptyMarksBriefAndRecordsTelemetry(t *testing.T) {
	before := telemetry.CouncilIntentsMissingTotal()
	items, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return nil, nil },
	))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(items) != 0 || !missing {
		t.Fatalf("items = %v, missing = %v; want empty, true", items, missing)
	}

	body := renderIntents(items)
	if missing {
		body = IntentsMissingMarker + "\n" + body
	}
	if got := strings.Count(body, IntentsMissingMarker); got != 1 {
		t.Fatalf("marker count = %d, want 1 in %q", got, body)
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 1 {
		t.Fatalf("counter delta = %d, want 1", got)
	}
}

func TestIntentStorePreflightPopulatedIsUnchanged(t *testing.T) {
	want := []*store.RoadmapIntent{{ID: 1, Summary: "Ship the preflight"}}
	before := telemetry.CouncilIntentsMissingTotal()
	items, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return want, nil },
	))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if missing {
		t.Fatal("populated store reported missing")
	}
	if len(items) != 1 || items[0] != want[0] {
		t.Fatalf("items = %#v, want unchanged %#v", items, want)
	}
	if strings.Contains(renderIntents(items), IntentsMissingMarker) {
		t.Fatal("populated intent rendering contains missing marker")
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 0 {
		t.Fatalf("counter delta = %d, want 0", got)
	}
}

func TestIntentStorePreflightStoreError(t *testing.T) {
	wantErr := errors.New("store unavailable")
	before := telemetry.CouncilIntentsMissingTotal()
	_, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return nil, wantErr },
	))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
	if missing {
		t.Fatal("store error reported missing")
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 0 {
		t.Fatalf("counter delta = %d, want 0", got)
	}
}
