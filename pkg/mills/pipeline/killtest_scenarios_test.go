package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAssertKilltestScenario(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	base := KilltestEvidence{CapturedAt: now.Add(-time.Second), Deadline: now.Add(time.Minute)}
	queued := base
	queued.Scenario = KilltestQueuedProof
	queued.QueuedProof = &QueuedProofEvidence{RunID: "run-1", BacklogID: "item-1", Transitions: []KilltestTransition{
		{RunID: "run-1", BacklogID: "item-1", State: "queued", ObservedAt: now.Add(-4 * time.Second)},
		{RunID: "run-1", BacklogID: "item-1", State: "picked_up", ObservedAt: now.Add(-3 * time.Second)},
		{RunID: "run-1", BacklogID: "item-1", State: "paused", ObservedAt: now.Add(-2 * time.Second)},
	}}
	mr := base
	mr.Scenario = KilltestMRAwareness
	mr.MRAwareness = &MRAwarenessEvidence{Repo: "services/loom-core", IID: 42, SourceBranch: "feat/x", Recognitions: []MRRecognition{
		{Repo: "services/loom-core", IID: 42, SourceBranch: "feat/x", State: "ok", Recognized: true, ObservedAt: now.Add(-2 * time.Second)},
	}}

	tests := []struct {
		name     string
		scenario KilltestScenario
		evidence KilltestEvidence
		want     string
	}{
		{"queued proof", KilltestQueuedProof, queued, "picked_up"},
		{"MR recognition", KilltestMRAwareness, mr, "recognized as ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := AssertKilltestScenario(tt.scenario, tt.evidence, now, time.Minute)
			if err != nil || !report.Passed || report.Verdict != "PASS" || !strings.Contains(strings.Join(report.Evidence, "\n"), tt.want) {
				t.Fatalf("report=%+v err=%v, want passing evidence containing %q", report, err, tt.want)
			}
		})
	}

	bad := []struct {
		name     string
		scenario KilltestScenario
		mutate   func(*KilltestEvidence)
	}{
		{"unknown scenario", "other", func(*KilltestEvidence) {}},
		{"missing evidence", KilltestQueuedProof, func(e *KilltestEvidence) { e.QueuedProof = nil }},
		{"contradictory identity", KilltestQueuedProof, func(e *KilltestEvidence) { e.QueuedProof.Transitions[1].RunID = "other" }},
		{"ambiguous duplicate", KilltestQueuedProof, func(e *KilltestEvidence) {
			e.QueuedProof.Transitions = append(e.QueuedProof.Transitions, e.QueuedProof.Transitions[2])
		}},
		{"stale", KilltestQueuedProof, func(e *KilltestEvidence) { e.CapturedAt = now.Add(-2 * time.Minute) }},
		{"timed out", KilltestQueuedProof, func(e *KilltestEvidence) { e.Deadline = now.Add(-time.Nanosecond) }},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			e := cloneQueuedEvidence(queued)
			tt.mutate(&e)
			if _, err := AssertKilltestScenario(tt.scenario, e, now, time.Minute); err == nil {
				t.Fatal("expected fail-closed error")
			}
		})
	}
}

func TestKilltestFailureCodeIsTyped(t *testing.T) {
	err := killtestError("stable_code", "wording can change")
	if got := KilltestFailureCode(err); got != "stable_code" {
		t.Fatalf("KilltestFailureCode() = %q, want stable_code", got)
	}
	if got := KilltestFailureCode(fmt.Errorf("wrapped: %w", err)); got != "stable_code" {
		t.Fatalf("wrapped KilltestFailureCode() = %q, want stable_code", got)
	}
	if got := KilltestFailureCode(errors.New("stale or future-dated")); got != "invalid_evidence" {
		t.Fatalf("untyped KilltestFailureCode() = %q, want invalid_evidence", got)
	}
}

func cloneQueuedEvidence(in KilltestEvidence) KilltestEvidence {
	out := in
	if in.QueuedProof != nil {
		copyProof := *in.QueuedProof
		copyProof.Transitions = append([]KilltestTransition(nil), in.QueuedProof.Transitions...)
		out.QueuedProof = &copyProof
	}
	return out
}
