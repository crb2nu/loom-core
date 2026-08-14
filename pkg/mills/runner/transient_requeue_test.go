package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestRouteTransientFailurePersistsBudgetAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	classification := pipeline.FailureClassification{Class: pipeline.FailureInfrastructure, Retryable: true, Classifier: pipeline.FailureClassifierName, ExternalDependencyID: "gitlab.runner_system_failure"}
	r := &Runner{Store: st}
	first := r.RouteTransientFailure(context.Background(), "BACKLOG-1", classification, 1)
	if first.Decision.Route != pipeline.FailureRouteRequeue || first.Classification != classification {
		t.Fatalf("first = %+v", first)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// A fresh runner after restart must terminally escalate once the durable
	// allowance is spent.
	second := (&Runner{Store: reopened}).RouteTransientFailure(context.Background(), "BACKLOG-1", classification, 1)
	if second.Decision.Route != pipeline.FailureRouteEscalate || !second.Decision.Exhausted {
		t.Fatalf("second = %+v", second)
	}
}

func TestRouteTransientFailureFailsClosedWithoutStore(t *testing.T) {
	classification := pipeline.FailureClassification{Class: pipeline.FailureTransient, Retryable: true}
	got := (*Runner)(nil).RouteTransientFailure(context.Background(), "BACKLOG-1", classification, 2)
	if got.Decision.Route != pipeline.FailureRouteEscalate || !got.Decision.Exhausted {
		t.Fatalf("decision = %+v", got.Decision)
	}
}
