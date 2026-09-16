package gates

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type preflightProbe func(Dependency) (DependencyEvidence, error)

func (p preflightProbe) Probe(_ context.Context, dep Dependency) (DependencyEvidence, error) {
	return p(dep)
}

func TestDependencyPreflightFailClosedStates(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		evidence DependencyEvidence
		err      error
		want     DependencyReasonCode
		pass     bool
	}{
		{"healthy", DependencyEvidence{DependencyHealthy, now.Add(-time.Minute)}, nil, ReasonHealthy, true},
		{"degraded", DependencyEvidence{DependencyDegraded, now.Add(-time.Minute)}, nil, ReasonDegraded, false},
		{"down", DependencyEvidence{DependencyDown, now.Add(-time.Minute)}, nil, ReasonDown, false},
		{"unknown", DependencyEvidence{DependencyUnknown, now.Add(-time.Minute)}, nil, ReasonUnknown, false},
		{"missing", DependencyEvidence{DependencyMissing, now.Add(-time.Minute)}, nil, ReasonMissing, false},
		{"stale", DependencyEvidence{DependencyHealthy, now.Add(-6 * time.Minute)}, nil, ReasonStale, false},
		{"probe error", DependencyEvidence{}, errors.New("secret transport detail"), ReasonProbeError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &DependencyPreflight{Dependencies: []Dependency{{Name: "gitlab"}}, Now: func() time.Time { return now }, Prober: preflightProbe(func(Dependency) (DependencyEvidence, error) { return tt.evidence, tt.err })}
			out, err := g.Evaluate(context.Background(), StageInput{})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if out.Pass != tt.pass {
				t.Fatalf("Pass = %v, want %v", out.Pass, tt.pass)
			}
			var got DependencyPreflightVerdict
			if err := json.Unmarshal([]byte(out.Reasons[0]), &got); err != nil {
				t.Fatalf("unmarshal verdict: %v", err)
			}
			if got.Dependencies[0].ReasonCode != tt.want {
				t.Fatalf("reason = %q, want %q", got.Dependencies[0].ReasonCode, tt.want)
			}
		})
	}
}

func TestDependencyPreflightDeterministicOrderingAndJSON(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	probe := preflightProbe(func(Dependency) (DependencyEvidence, error) { return DependencyEvidence{DependencyHealthy, now}, nil })
	g := &DependencyPreflight{Dependencies: []Dependency{{Name: "prometheus"}, {Name: "gitlab"}}, Prober: probe, Now: func() time.Time { return now }}
	first, _ := g.Evaluate(context.Background(), StageInput{})
	second, _ := g.Evaluate(context.Background(), StageInput{})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("verdict changed: %#v != %#v", first, second)
	}
	var decoded DependencyPreflightVerdict
	if err := json.Unmarshal([]byte(first.Reasons[0]), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := []string{decoded.Dependencies[0].Name, decoded.Dependencies[1].Name}; !reflect.DeepEqual(got, []string{"gitlab", "prometheus"}) {
		t.Fatalf("order = %v", got)
	}
	if !decoded.Admitted || decoded.Status != "admitted" {
		t.Fatalf("verdict = %+v", decoded)
	}
}

func TestDependencyPreflightMissingDeclarationsAndInvalidConfig(t *testing.T) {
	out, err := (&DependencyPreflight{}).Evaluate(context.Background(), StageInput{})
	if err != nil || out.Pass {
		t.Fatalf("missing declarations must fail closed: out=%+v err=%v", out, err)
	}
	_, err = (&DependencyPreflight{Dependencies: []Dependency{{Name: ""}}}).Evaluate(context.Background(), StageInput{})
	if err == nil {
		t.Fatal("empty dependency name must be a configuration error")
	}
}

func TestDependencyPreflightRegisteredByDefault(t *testing.T) {
	g, err := Default().Get(DependencyPreflightGateName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if g.Name() != DependencyPreflightGateName {
		t.Fatalf("Name = %q", g.Name())
	}
}
