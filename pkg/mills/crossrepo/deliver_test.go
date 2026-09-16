package crossrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/audit"
	"github.com/crb2nu/loom/pkg/mills/store"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type recordingStampAuthorizer struct {
	routes [][2]string
	err    error
}

func (a *recordingStampAuthorizer) AuthorizeStamp(_ context.Context, source, target string) error {
	a.routes = append(a.routes, [2]string{source, target})
	return a.err
}

type recordingStampAudit struct{ events []audit.StampWriteEvent }

func (a *recordingStampAudit) EmitStampWrite(_ context.Context, event audit.StampWriteEvent) {
	a.events = append(a.events, event)
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

func newTestDeliverer(authorizer StampAuthorizer, writer StampWriter) (*Deliverer, *telemetry.CrossRepoStampDeliveryMetrics, *recordingStampAudit) {
	metrics := telemetry.NewCrossRepoStampDeliveryMetrics(prometheus.NewRegistry())
	auditSink := &recordingStampAudit{}
	return &Deliverer{SourceProject: "services/loom-core", Authorizer: authorizer, Writer: writer, Audit: auditSink, Metrics: metrics}, metrics, auditSink
}

func TestDelivererAuthorizesAndWritesExactTarget(t *testing.T) {
	authorizer := &recordingStampAuthorizer{}
	writer := &deliveryStampWriter{}
	deliverer, metrics, auditSink := newTestDeliverer(authorizer, writer)
	stamp := &store.Stamp{ID: "stamp-widget", TargetProject: "services/widgets"}

	if err := deliverer.Deliver(context.Background(), stamp); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(authorizer.routes) != 1 || authorizer.routes[0] != [2]string{"services/loom-core", "services/widgets"} {
		t.Fatalf("authorized routes = %v, want exact source and target", authorizer.routes)
	}
	if writer.calls != 1 || writer.stamp != stamp || writer.stamp.TargetProject != "services/widgets" {
		t.Fatalf("writer = calls:%d stamp:%+v, want exact input stamp", writer.calls, writer.stamp)
	}
	if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(telemetry.CrossRepoStampDeliverySuccess)); got != 1 {
		t.Fatalf("success deliveries = %v, want 1", got)
	}
	if len(auditSink.events) != 1 || auditSink.events[0] != (audit.StampWriteEvent{SourceProject: "services/loom-core", TargetProject: "services/widgets", StampID: "stamp-widget", Decision: audit.StampWriteAllowed, Reason: "written"}) {
		t.Fatalf("audit events = %+v, want one allowed event", auditSink.events)
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
		{name: "whitespace", stamp: &store.Stamp{ID: "blank", TargetProject: " \t\n"}},
		{name: "malformed traversal", stamp: &store.Stamp{ID: "bad", TargetProject: "services/../widgets"}},
		{name: "denied", stamp: &store.Stamp{ID: "denied", TargetProject: "services/widgets"}, authErr: errors.New("policy denied"), wantAuth: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &recordingStampAuthorizer{err: tc.authErr}
			writer := &deliveryStampWriter{}
			deliverer, metrics, auditSink := newTestDeliverer(authorizer, writer)
			if err := deliverer.Deliver(context.Background(), tc.stamp); !errors.Is(err, ErrStampDeliveryDenied) {
				t.Fatalf("Deliver error = %v, want ErrStampDeliveryDenied", err)
			}
			if writer.calls != 0 {
				t.Fatalf("writer called %d times; home/default fallback reached the sink", writer.calls)
			}
			if got := len(authorizer.routes); (got == 1) != tc.wantAuth {
				t.Fatalf("authorization calls = %d, wantAuth=%v", got, tc.wantAuth)
			}
			if len(auditSink.events) != 1 || auditSink.events[0].Decision != audit.StampWriteRejected || auditSink.events[0].Reason == "" {
				t.Fatalf("audit events = %+v, want one rejected event with reason", auditSink.events)
			}
			if got := testutil.ToFloat64(metrics.DeliveriesTotal.WithLabelValues(telemetry.CrossRepoStampDeliveryDenial)); got != 1 {
				t.Fatalf("denied deliveries = %v, want 1", got)
			}
		})
	}
}

func TestDelivererRecordsAuthorizedWriteFailure(t *testing.T) {
	writer := &deliveryStampWriter{err: errors.New("transport unavailable")}
	deliverer, metrics, auditSink := newTestDeliverer(&recordingStampAuthorizer{}, writer)
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
	if len(auditSink.events) != 1 || auditSink.events[0].Reason != "write_failed" {
		t.Fatalf("audit events = %+v, want one write_failed event", auditSink.events)
	}
}

func TestDelivererPolicyAllowsSameProjectAndExplicitCrossProjectOnly(t *testing.T) {
	policy := sharedpolicy.StampTargetPolicy{AllowedTargets: map[string][]string{
		"services/loom-core": {"services/flexdeck"},
	}}
	for _, tc := range []struct {
		name, target string
		wantErr      bool
	}{
		{name: "same project", target: "services/loom-core"},
		{name: "allowlisted cross project", target: "services/flexdeck"},
		{name: "unlisted cross project", target: "services/unknown", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &deliveryStampWriter{}
			deliverer, _, auditSink := newTestDeliverer(policy, writer)
			err := deliverer.Deliver(context.Background(), &store.Stamp{ID: "stamp-1", TargetProject: tc.target})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Deliver error = %v, wantErr %v", err, tc.wantErr)
			}
			if writer.calls != boolInt(!tc.wantErr) {
				t.Fatalf("writer calls = %d", writer.calls)
			}
			if len(auditSink.events) != 1 {
				t.Fatalf("audit events = %d, want 1", len(auditSink.events))
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
