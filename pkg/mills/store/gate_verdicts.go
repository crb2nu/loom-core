package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	gateVerdictActor       = "mills.gate-evaluator"
	gateVerdictEventKind   = "pipeline.gate.verdict"
	gateVerdictSubjectKind = "pipeline_run"

	// JudgeVerdictEventKind carries one LLM judge's scored opinion behind a
	// gate: {run_id, backlog_id, gate, judge_model, role, score, threshold,
	// pass, attempt}. It is deliberately separate from gateVerdictEventKind
	// above, which records the pass/fail/skip/error enum for EVERY gate from
	// a telemetry sink that sees neither the score nor the backlog item. The
	// runner writes this one because only it holds the run's attempt counter.
	JudgeVerdictEventKind = "judge.verdict"
	// JudgeVerdictSubjectKind subjects a judge verdict to its pipeline run so
	// EventDAO.ListBySubject can replay one run's grades.
	JudgeVerdictSubjectKind = "pipeline_run"
)

// GateVerdictStore durably appends structured gate evaluations to the Mills
// event journal. The pipeline run is the event subject, making records survive
// process restarts and directly queryable with EventDAO.ListBySubject.
type GateVerdictStore struct {
	events  *EventDAO
	mu      sync.Mutex
	lastErr error
}

// NewGateVerdictStore creates a durable telemetry sink for a Mills store.
func NewGateVerdictStore(s *Store) *GateVerdictStore {
	if s == nil {
		return &GateVerdictStore{}
	}
	return &GateVerdictStore{events: s.Events}
}

// Persist validates and appends one gate verdict.
func (s *GateVerdictStore) Persist(ctx context.Context, verdict telemetry.GateEvaluation) error {
	if s == nil || s.events == nil {
		return errors.New("gate verdict store: not configured")
	}
	if verdict.RunID == "" || verdict.GateID == "" {
		return errors.New("gate verdict store: run_id + gate required")
	}
	if verdict.DurationMS < 0 {
		verdict.DurationMS = 0
	}
	err := s.events.Append(ctx, &Event{
		Actor: gateVerdictActor, Kind: gateVerdictEventKind,
		SubjectKind: gateVerdictSubjectKind, SubjectID: verdict.RunID,
		Payload: map[string]any{
			"gate": verdict.GateID, "verdict": string(verdict.Verdict),
			"reason": verdict.Reason, "run_id": verdict.RunID,
			"duration_ms": verdict.DurationMS,
		},
	})
	if err != nil {
		return fmt.Errorf("persist gate verdict: %w", err)
	}
	return nil
}

// RecordGateEvaluation implements telemetry.GateEvaluationSink. The sink
// interface is intentionally non-failing, so persistence errors are retained
// for health/debug inspection without changing the gate's verdict.
func (s *GateVerdictStore) RecordGateEvaluation(verdict telemetry.GateEvaluation) {
	err := s.Persist(context.Background(), verdict)
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

// LastError returns the latest sink persistence error, if any.
func (s *GateVerdictStore) LastError() error {
	if s == nil {
		return errors.New("gate verdict store: not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}
