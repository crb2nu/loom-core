package crossrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type recordingStampAuthorizer struct {
	projects []string
	err      error
}

func (a *recordingStampAuthorizer) AuthorizeStamp(_ context.Context, project string) error {
	a.projects = append(a.projects, project)
	return a.err
}

type deliveryStampWriter struct {
	calls int
	stamp *store.Stamp
	err   error
}

func (w *deliveryStampWriter) Put(_ context.Context, stamp *store.Stamp) error {
	w.calls++
	w.stamp = stamp
	return w.err
}

func newTestDeliverer(authorizer StampAuthorizer, writer StampWriter) (*Deliverer, *telemetry.CrossRepoStampDeliveryMetrics) {
	metrics := telemetry.NewCrossRepoStampDeliveryMetrics(prometheus.NewRegistry())
	return &Deliverer{Authorizer: authorizer, Writer: writer, Metrics: metrics}, metrics
}

func TestDelivererAuthorizesAndWritesExactTarget(t *testing.T) {
	authorizer := &recordingStampAuthorizer{}
	writer := &deliveryStampWriter{}
	deliverer, metrics := newTestDeliverer(authorizer, writer)
	stamp := &store.Stamp{ID: "stamp-widget", TargetProject: "services/widgets"}

	if err := deliverer.Deliver(context.Background(), stamp); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(authorizer.projects) != 1 || authorizer.projects[0] != "services/widgets" {
		t.Fatalf("authorized projects = %v, want exact target", authorizer.projects)
	}
	if writer.calls != 1 || writer.stamp != stamp || writer.stamp.TargetProject != "services/widgets" {
		t.Fatalf("writer = calls:%d stamp:%+v, want exact input stamp", writer.calls, writer.stamp)
	}
	if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(telemetry.CrossRepoStampDeliverySuccess)); got != 1 {
		t.Fatalf("success deliveries = %v, want 1", got)
	}
}

func TestDelivererRejectsTargetsBeforeWriteWithoutFallback(t *testing.T) {
	tests := []struct {
		name     string
		stamp    *store.Stamp
		authErr  error
		wantAuth bool
	}{
		{name: "missing stamp"},
		{name: "missing project", stamp: &store.Stamp{ID: "missing"}},
		{name: "whitespace", stamp: &store.Stamp{ID: "blank", TargetProject: " services/widgets "}},
		{name: "malformed traversal", stamp: &store.Stamp{ID: "bad", TargetProject: "services/../widgets"}},
		{name: "denied", stamp: &store.Stamp{ID: "denied", TargetProject: "services/widgets"}, authErr: errors.New("policy denied"), wantAuth: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &recordingStampAuthorizer{err: tc.authErr}
			writer := &deliveryStampWriter{}
			deliverer, metrics := newTestDeliverer(authorizer, writer)
			if err := deliverer.Deliver(context.Background(), tc.stamp); !errors.Is(err, ErrStampDeliveryDenied) {
				t.Fatalf("Deliver error = %v, want ErrStampDeliveryDenied", err)
			}
			if writer.calls != 0 {
				t.Fatalf("writer called %d times; home/default fallback reached the sink", writer.calls)
			}
			if got := len(authorizer.projects); (got == 1) != tc.wantAuth {
				t.Fatalf("authorization calls = %d, wantAuth=%v", got, tc.wantAuth)
			}
			if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(telemetry.CrossRepoStampDeliveryDenial)); got != 1 {
				t.Fatalf("denied deliveries = %v, want 1", got)
			}
		})
	}
}

func TestDelivererRecordsAuthorizedWriteFailure(t *testing.T) {
	writer := &deliveryStampWriter{err: errors.New("transport unavailable")}
	deliverer, metrics := newTestDeliverer(&recordingStampAuthorizer{}, writer)
	err := deliverer.Deliver(context.Background(), &store.Stamp{ID: "stamp-widget", TargetProject: "services/widgets"})
	if !errors.Is(err, ErrStampDeliveryFailed) {
		t.Fatalf("Deliver error = %v, want ErrStampDeliveryFailed", err)
	}
	if writer.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", writer.calls)
	}
	if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(telemetry.CrossRepoStampDeliveryFailure)); got != 1 {
		t.Fatalf("failed deliveries = %v, want 1", got)
	}
}
