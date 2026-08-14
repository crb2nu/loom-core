package pipeline

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestResolveClassification(t *testing.T) {
	tests := []struct {
		name             string
		primary          SourceClassification
		secondary        SourceClassification
		want             ClassificationClass
		wantDisagreement bool
		wantReason       string
	}{
		{
			name:      "external incident agreement",
			primary:   SourceClassification{Source: "runner", Class: ClassificationExternalDependencyIncident},
			secondary: SourceClassification{Source: "ci", Class: ClassificationExternalDependencyIncident},
			want:      ClassificationExternalDependencyIncident, wantReason: "source_agreement",
		},
		{
			name:      "repository regression agreement",
			primary:   SourceClassification{Source: "runner", Class: ClassificationRepositoryRegression},
			secondary: SourceClassification{Source: "ci", Class: ClassificationRepositoryRegression},
			want:      ClassificationRepositoryRegression, wantReason: "source_agreement",
		},
		{
			name:      "disagreement fails closed",
			primary:   SourceClassification{Source: "runner", Class: ClassificationRepositoryRegression},
			secondary: SourceClassification{Source: "ci", Class: ClassificationExternalDependencyIncident},
			want:      ClassificationUnknown, wantDisagreement: true, wantReason: "source_disagreement",
		},
		{
			name:      "missing source fails closed",
			primary:   SourceClassification{Source: "runner", Class: ClassificationRepositoryRegression},
			secondary: SourceClassification{},
			want:      ClassificationUnknown, wantReason: "missing_or_unknown_source",
		},
		{
			name:      "unknown class fails closed",
			primary:   SourceClassification{Source: "runner", Class: "new_unreviewed_class"},
			secondary: SourceClassification{Source: "ci", Class: ClassificationRepositoryRegression},
			want:      ClassificationUnknown, wantReason: "missing_or_unknown_source",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveClassification("run-1", tc.primary, tc.secondary)
			if got.ResolvedClass != tc.want || got.Disagreement != tc.wantDisagreement ||
				got.ResolutionReason != tc.wantReason {
				t.Fatalf("ResolveClassification() = %+v", got)
			}
			again := ResolveClassification("run-1", tc.primary, tc.secondary)
			got.ResolvedAt, again.ResolvedAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(got, again) {
				t.Fatalf("resolution is not deterministic:\n%+v\n%+v", got, again)
			}
		})
	}
}

type recordingPublisher struct {
	types    []string
	payloads []any
}

func (p *recordingPublisher) Publish(eventType string, payload any) {
	p.types = append(p.types, eventType)
	p.payloads = append(p.payloads, payload)
}

func TestVerdictReconcilerPersistsRoundTripAndEmitsDisagreementOnce(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	pub := &recordingPublisher{}
	r := VerdictReconciler{
		Store: st.ClassificationVerdicts, Publisher: pub,
		Now: func() time.Time { return now },
	}
	primary := SourceClassification{Source: "runner", Class: ClassificationRepositoryRegression}
	secondary := SourceClassification{Source: "ci", Class: ClassificationExternalDependencyIncident}

	first, err := r.Reconcile(ctx, "run-conflict", primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Reconcile(ctx, "run-conflict", primary, secondary)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("persisted round trip changed verdict:\n%+v\n%+v", first, second)
	}
	if len(pub.types) != 1 || pub.types[0] != ClassificationDisagreementEvent {
		t.Fatalf("published events = %v, want one disagreement event", pub.types)
	}
	if got, ok := pub.payloads[0].(ClassificationVerdict); !ok || got.FailureID != "run-conflict" {
		t.Fatalf("event payload = %#v", pub.payloads[0])
	}
}

func TestVerdictReconcilerAgreementDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pub := &recordingPublisher{}
	r := VerdictReconciler{Store: st.ClassificationVerdicts, Publisher: pub}
	source := SourceClassification{Source: "runner", Class: ClassificationRepositoryRegression}
	if _, err := r.Reconcile(ctx, "run-agreement", source,
		SourceClassification{Source: "ci", Class: source.Class}); err != nil {
		t.Fatal(err)
	}
	if len(pub.types) != 0 {
		t.Fatalf("agreement published events: %v", pub.types)
	}
}
