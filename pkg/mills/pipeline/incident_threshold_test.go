package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

type incidentCounterStub struct {
	count int
	err   error
	ref   string
	since time.Time
}

func (s *incidentCounterStub) CountExternalDependencyIncidentClusters(_ context.Context, ref string, since time.Time) (int, error) {
	s.ref, s.since = ref, since
	return s.count, s.err
}

type incidentKPIStub struct {
	snap *store.KPISnapshot
	err  error
}

func (s *incidentKPIStub) RecordSnapshot(_ context.Context, snap *store.KPISnapshot) error {
	s.snap = snap
	return s.err
}

func TestExternalIncidentThresholdGuardBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		count      int
		threshold  int
		suppressed bool
	}{
		{name: "none", count: 0},
		{name: "below", count: 2},
		{name: "at", count: 3},
		{name: "above", count: 4, suppressed: true},
		{name: "override below", count: 4, threshold: 5},
		{name: "override at", count: 5, threshold: 5},
		{name: "override above", count: 6, threshold: 5, suppressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &incidentCounterStub{count: tc.count}
			kpi := &incidentKPIStub{}
			got, err := (ExternalIncidentThresholdGuard{
				Counter: counter,
				KPI:     kpi,
				Policy:  sharedpolicy.ExternalIncidentPolicy{Threshold: tc.threshold},
				Now:     func() time.Time { return now },
			}).Evaluate(context.Background(), "refs/heads/main")
			if err != nil {
				t.Fatal(err)
			}
			if got.Pass == tc.suppressed {
				t.Fatalf("Pass = %v, suppressed = %v: %+v", got.Pass, tc.suppressed, got)
			}
			if counter.ref != "refs/heads/main" || !counter.since.Equal(now.Add(-24*time.Hour)) {
				t.Fatalf("query = ref %q since %s", counter.ref, counter.since)
			}
			if tc.suppressed {
				if kpi.snap == nil || kpi.snap.Metrics[ExternalIncidentSuppressionsKPI] != 1 {
					t.Fatalf("suppression KPI = %+v", kpi.snap)
				}
				wantReason := `ref="refs/heads/main" count=`
				if len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], wantReason) ||
					!strings.Contains(got.Reasons[0], "window=24h0m0s") {
					t.Fatalf("Reasons = %#v", got.Reasons)
				}
			} else if kpi.snap != nil {
				t.Fatalf("KPI emitted for passing verdict: %+v", kpi.snap)
			}
		})
	}
}

func TestExternalIncidentThresholdGuardDeterministicReason(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	guard := ExternalIncidentThresholdGuard{
		Counter: &incidentCounterStub{count: 4},
		Policy:  sharedpolicy.ExternalIncidentPolicy{},
		Now:     func() time.Time { return now },
	}
	first, err := guard.Evaluate(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := guard.Evaluate(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("verdict changed: first=%+v second=%+v", first, second)
	}
	if got, want := first.Reasons[0], `ref="main" count=4 window=24h0m0s threshold=3`; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestExternalIncidentThresholdGuardErrors(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		_, err := (ExternalIncidentThresholdGuard{
			Counter: &incidentCounterStub{err: errors.New("read failed")},
		}).Evaluate(context.Background(), "main")
		if err == nil || !strings.Contains(err.Error(), "read failed") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("KPI", func(t *testing.T) {
		_, err := (ExternalIncidentThresholdGuard{
			Counter: &incidentCounterStub{count: 4},
			KPI:     &incidentKPIStub{err: errors.New("write failed")},
		}).Evaluate(context.Background(), "main")
		if err == nil || !strings.Contains(err.Error(), "write failed") {
			t.Fatalf("error = %v", err)
		}
	})
}
