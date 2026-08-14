package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type retryStateStoreStub struct {
	state store.RunRetryState
	err   error
	calls int
}

type retryVerdictStoreStub struct {
	verdict store.ClassificationVerdictRecord
	err     error
	calls   int
}

func (s *retryVerdictStoreStub) GetClassificationVerdict(
	context.Context, string,
) (store.ClassificationVerdictRecord, error) {
	s.calls++
	return s.verdict, s.err
}

func (s *retryStateStoreStub) GetRunRetryState(context.Context, string) (store.RunRetryState, error) {
	s.calls++
	return s.state, s.err
}

func TestRetryPolicyConsumesResolvedVerdict(t *testing.T) {
	tests := []struct {
		name        string
		resolved    ClassificationClass
		allowed     bool
		disposition string
	}{
		{
			name:        "external incident denies first paid retry",
			resolved:    ClassificationExternalDependencyIncident,
			disposition: RetryDispositionWaitForDependencyRecovery,
		},
		{
			name:     "repository regression retains retry path",
			resolved: ClassificationRepositoryRegression,
			allowed:  true,
		},
		{
			name:        "unresolved verdict fails closed",
			resolved:    ClassificationUnknown,
			disposition: RetryDispositionWaitForDependencyRecovery,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			persisted := store.RunRetryState{
				PaidRetryCount: 0,
			}
			source := &retryStateStoreStub{state: persisted}
			verdicts := &retryVerdictStoreStub{verdict: store.ClassificationVerdictRecord{
				FailureID: "run-1", ResolvedClass: string(tc.resolved),
			}}
			got, err := (RetryPolicy{
				Store: source, VerdictStore: verdicts,
			}).Decide(context.Background(), "run-1", true)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got.Allowed != tc.allowed || got.Disposition != tc.disposition {
				t.Fatalf("Decide = %+v, want allowed=%v disposition=%q",
					got, tc.allowed, tc.disposition)
			}
			if source.state.PaidRetryCount != persisted.PaidRetryCount {
				t.Fatalf("paid retry count = %d, want unchanged %d",
					source.state.PaidRetryCount, persisted.PaidRetryCount)
			}
		})
	}
}

func TestRetryPolicyFreeRetryDoesNotReadOrConsumePaidBudget(t *testing.T) {
	source := &retryStateStoreStub{state: store.RunRetryState{
		Classification: store.ExternalDependencyIncidentClassification,
		PaidRetryCount: 99,
	}}
	verdicts := &retryVerdictStoreStub{}
	got, err := (RetryPolicy{Store: source, VerdictStore: verdicts}).Decide(
		context.Background(), "run-1", false,
	)
	if err != nil || !got.Allowed {
		t.Fatalf("Decide = %+v, err=%v", got, err)
	}
	if source.calls != 0 {
		t.Fatalf("store calls = %d, want 0 for free retry", source.calls)
	}
	if verdicts.calls != 0 {
		t.Fatalf("verdict store calls = %d, want 0 for free retry", verdicts.calls)
	}
}

func TestRetryPolicyMissingVerdictFallsBackToExistingCap(t *testing.T) {
	source := &retryStateStoreStub{state: store.RunRetryState{
		Classification: store.ExternalDependencyIncidentClassification,
		PaidRetryCount: DefaultExternalIncidentPaidRetryCap,
	}}
	verdicts := &retryVerdictStoreStub{err: store.ErrNotFound}
	got, err := (RetryPolicy{Store: source, VerdictStore: verdicts}).Decide(
		context.Background(), "run-1", true,
	)
	if err != nil || got.Allowed ||
		got.Disposition != RetryDispositionWaitForDependencyRecovery {
		t.Fatalf("Decide = %+v, err=%v", got, err)
	}
}

func TestRetryPolicyMissingVerdictPreservesExistingAllowance(t *testing.T) {
	source := &retryStateStoreStub{state: store.RunRetryState{
		Classification: store.ExternalDependencyIncidentClassification,
		PaidRetryCount: DefaultExternalIncidentPaidRetryCap - 1,
	}}
	got, err := (RetryPolicy{
		Store: source, VerdictStore: &retryVerdictStoreStub{err: store.ErrNotFound},
	}).Decide(context.Background(), "run-1", true)
	if err != nil || !got.Allowed {
		t.Fatalf("Decide = %+v, err=%v", got, err)
	}
}

func TestRetryPolicyStoreFailureFailsSafeAndEmitsTelemetry(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := telemetry.NewRetryMetrics(reg)
	source := &retryStateStoreStub{err: errors.New("database unavailable")}
	got, err := (RetryPolicy{
		Store: source, VerdictStore: &retryVerdictStoreStub{}, Metrics: metrics,
	}).Decide(
		context.Background(), "run-1", true,
	)
	if err == nil || got.Allowed || got.Disposition != RetryDispositionWaitForDependencyRecovery {
		t.Fatalf("Decide = %+v, err=%v", got, err)
	}
	if value := testutil.ToFloat64(metrics.CapRefusalsTotal.WithLabelValues(
		telemetry.RetryIncidentClassUnknown,
		telemetry.RetryDispositionWaitForRecovery,
	)); value != 1 {
		t.Fatalf("refusal counter = %v, want 1", value)
	}
}

func TestRetryPolicyCapRefusalEmitsBoundedTelemetry(t *testing.T) {
	metrics := telemetry.NewRetryMetrics(nil)
	source := &retryStateStoreStub{state: store.RunRetryState{
		PaidRetryCount: 0,
	}}
	verdicts := &retryVerdictStoreStub{verdict: store.ClassificationVerdictRecord{
		FailureID: "run-1", ResolvedClass: string(ClassificationExternalDependencyIncident),
	}}
	got, err := (RetryPolicy{Store: source, VerdictStore: verdicts, Metrics: metrics}).Decide(
		context.Background(), "run-1", true,
	)
	if err != nil || got.Allowed {
		t.Fatalf("Decide = %+v, err=%v", got, err)
	}
	if value := testutil.ToFloat64(metrics.CapRefusalsTotal.WithLabelValues(
		telemetry.RetryIncidentClassExternalDependency,
		telemetry.RetryDispositionWaitForRecovery,
	)); value != 1 {
		t.Fatalf("refusal counter = %v, want 1", value)
	}
}

func TestRetryPolicyUsesPersistedClassificationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Backlog.Put(ctx, &store.BacklogItem{
		ID: "item-1", Title: "retry test", State: store.BacklogRunning, Priority: store.P2,
	}); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	runs := []*store.PipelineRun{
		{
			ID: "initial", BacklogID: "item-1", State: store.PipelineQueued,
			Attempts: 1, FailureClass: string(FailureCode), StartedAt: time.Unix(1, 0),
		},
		{
			ID: "free", BacklogID: "item-1", State: store.PipelineQueued,
			Attempts: 2, FailureClass: string(FailureTransient), StartedAt: time.Unix(2, 0),
		},
		{
			ID: "paid", BacklogID: "item-1", State: store.PipelineEscalated,
			Attempts: 3, FailureClass: string(FailureInfrastructure),
			ExternalDependencyID: "external_dependency.gitlab.api_unavailable", StartedAt: time.Unix(3, 0),
		},
		{
			ID: "current", BacklogID: "item-1", State: store.PipelineQueued,
			Attempts: 4, FailureClass: string(FailureInfrastructure),
			ExternalDependencyID: "external_dependency.gitlab.api_unavailable",
			StartedAt:            time.Unix(4, 0),
		},
	}
	for _, run := range runs {
		if err := st.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("put run %s: %v", run.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	if inserted, err := reopened.ClassificationVerdicts.PutClassificationVerdict(
		ctx, store.ClassificationVerdictRecord{
			FailureID:      "current",
			PrimaryClass:   string(ClassificationExternalDependencyIncident),
			SecondaryClass: string(ClassificationExternalDependencyIncident),
			ResolvedClass:  string(ClassificationExternalDependencyIncident),
			ResolvedAt:     time.Unix(5, 0),
		},
	); err != nil || !inserted {
		t.Fatalf("put verdict: inserted=%v err=%v", inserted, err)
	}

	got, err := (RetryPolicy{
		Store: reopened.Pipeline, VerdictStore: reopened.ClassificationVerdicts,
	}).Decide(ctx, "current", true)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Allowed || got.PaidRetries != 1 ||
		got.Disposition != RetryDispositionWaitForDependencyRecovery {
		t.Fatalf("Decide = %+v, want persisted paid retry count 1 parked", got)
	}
}
