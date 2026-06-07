package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// DAOJournal adapts the runtime's Journal interface onto the durable
// store.WorkflowDAO (workflow_runs + workflow_steps, migration 004). It replaces
// the spike's in-memory map + atomic effect-counter with the merged store:
//
//   - Get      -> WorkflowDAO.GetStep
//   - Append   -> WorkflowDAO.AppendStep  (pending->success advances in place
//     via UNIQUE(run_id, step_key); a call_hash mismatch surfaces
//     store.ErrStepCallHashMismatch which we translate to *QuarantineError)
//   - SuccessCount -> COUNT(*) of success rows for the run (the spike's atomic
//     effect-counter becomes a durable count of committed effects)
//
// The store enforces a foreign key workflow_steps.run_id -> workflow_runs(id),
// so DAOJournal lazily inserts an 'imperative' workflow_runs row the first time
// a step is appended for a run id.
type DAOJournal struct {
	ctx     context.Context
	dao     *store.WorkflowDAO
	ensured map[string]bool // run ids whose workflow_runs row exists
}

// NewDAOJournal builds a journal backed by st.Workflow. The context governs all
// DAO calls.
func NewDAOJournal(ctx context.Context, st *store.Store) *DAOJournal {
	return NewDAOJournalFromDAO(ctx, st.Workflow)
}

// NewDAOJournalFromDAO builds a journal directly over a *store.WorkflowDAO. The
// Layer-3 runtime (runtime.go) holds a WorkflowDAO rather than the whole Store,
// so it constructs its per-run journal through this variant. The context
// governs all DAO calls.
func NewDAOJournalFromDAO(ctx context.Context, dao *store.WorkflowDAO) *DAOJournal {
	return &DAOJournal{
		ctx:     ctx,
		dao:     dao,
		ensured: map[string]bool{},
	}
}

// Get returns the prior record for (runID, stepKey). The bool is false when no
// step exists yet (a LIVE call); a real DAO error (not ErrNotFound) propagates.
func (j *DAOJournal) Get(runID, stepKey string) (Record, bool, error) {
	st, err := j.dao.GetStep(j.ctx, runID, stepKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("workflow journal get %s/%s: %w", runID, stepKey, err)
	}
	return toRecord(st), true, nil
}

// Append upserts a runtime Record into the durable step journal. It maps the
// runtime Status onto store.WorkflowStepStatus, ensures the parent run row
// exists (FK), and translates a durable call_hash mismatch into the runtime's
// *QuarantineError. The mismatch path mirrors the spike: the existing row is
// never overwritten; the runtime freezes the step.
func (j *DAOJournal) Append(r Record) error {
	if err := j.ensureRun(r.RunID); err != nil {
		return err
	}

	step := &store.WorkflowStep{
		RunID:     r.RunID,
		StepKey:   r.StepKey,
		EventType: eventTypeFor(r.PrimName),
		CallHash:  r.CallHash,
		Status:    statusFor(r.Status),
		// Effect provenance (S6-min). spawn_id + cost_source + cost_usd are set
		// by a real executor on the terminal step; empty/zero for the stub.
		// AppendStep's updateStep uses COALESCE(?, spawn_id) so a later replay
		// append that carries no spawn id never clobbers the recorded one.
		SpawnID:    r.SpawnID,
		CostSource: store.WorkflowCostSource(r.CostSource),
		CostUSD:    r.CostUSD,
	}
	if len(r.ResultBlob) > 0 {
		step.ResultBlob = string(r.ResultBlob)
	}
	switch r.Status {
	case StatusPending:
		now := time.Now().UTC()
		step.StartedAt = &now
	case StatusSuccess:
		now := time.Now().UTC()
		step.EndedAt = &now
		step.EffectCount = 1
	case StatusQuarantined:
		// A quarantine append re-asserts the EXISTING (recorded) call_hash so it
		// is a same-hash advance to a terminal status, not a mismatch. The DAO
		// treats this as a pending->terminal transition of the recorded row.
	}

	_, err := j.dao.AppendStep(j.ctx, step)
	if err != nil {
		if errors.Is(err, store.ErrStepCallHashMismatch) {
			// Durable nondeterminism tripwire. Surface as the runtime's
			// QuarantineError; readThrough already handles the freeze. We do not
			// reach here on the normal quarantine-append path (that re-uses the
			// recorded hash), only when a fresh append disagrees with a recorded
			// row — which is exactly the spike's mismatch case.
			return &QuarantineError{StepKey: r.StepKey, Want: r.CallHash, Got: r.CallHash}
		}
		return fmt.Errorf("workflow journal append %s/%s: %w", r.RunID, r.StepKey, err)
	}
	return nil
}

// SuccessCount returns the number of committed effects for a run: the count of
// workflow_steps rows in 'success' status. This is the durable replacement for
// the spike's atomic effect-counter — a fresh host bound to the same store sees
// the same count, so dropping the interpreter never resets it.
func (j *DAOJournal) SuccessCount(runID string) (int64, error) {
	steps, err := j.dao.ListByRun(j.ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("workflow journal success count %s: %w", runID, err)
	}
	var n int64
	for _, s := range steps {
		if s.Status == store.WorkflowStepSuccess {
			n++
		}
	}
	return n, nil
}

// ensureRun lazily inserts the parent workflow_runs row so step FKs resolve. It
// is idempotent (PutWorkflowRun upserts) and cached per run id to avoid a write
// on every append.
func (j *DAOJournal) ensureRun(runID string) error {
	if j.ensured[runID] {
		return nil
	}
	now := time.Now().UTC()
	run := &store.WorkflowRun{
		ID:                 runID,
		Engine:             store.WorkflowEngineImperative,
		Template:           "workflow-seed",
		TemplateVersion:    "v0",
		InterpreterVersion: HostInterpreterVersion,
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := j.dao.PutWorkflowRun(j.ctx, run); err != nil {
		return fmt.Errorf("workflow journal ensure run %s: %w", runID, err)
	}
	j.ensured[runID] = true
	return nil
}

// toRecord translates a durable step into the runtime Record the host consumes.
func toRecord(s *store.WorkflowStep) Record {
	r := Record{
		RunID:      s.RunID,
		StepKey:    s.StepKey,
		PrimName:   string(s.EventType),
		CallHash:   s.CallHash,
		Status:     runtimeStatus(s.Status),
		SpawnID:    s.SpawnID,
		CostSource: string(s.CostSource),
		CostUSD:    s.CostUSD,
	}
	if s.ResultBlob != "" {
		r.ResultBlob = []byte(s.ResultBlob)
	}
	return r
}

// statusFor maps a runtime Status onto the durable step status.
func statusFor(s Status) store.WorkflowStepStatus {
	switch s {
	case StatusPending:
		return store.WorkflowStepPending
	case StatusSuccess:
		return store.WorkflowStepSuccess
	case StatusQuarantined:
		// The durable journal has no dedicated quarantined step status; a frozen
		// step is recorded as an error terminal so it never re-executes (the run
		// itself moves to WorkflowRunQuarantined at a higher layer).
		return store.WorkflowStepError
	default:
		return store.WorkflowStepPending
	}
}

// runtimeStatus maps a durable step status back to the runtime view. Any
// terminal status other than success reads as quarantined (frozen) so the host
// never re-executes it; success and pending map straight through.
func runtimeStatus(s store.WorkflowStepStatus) Status {
	switch s {
	case store.WorkflowStepPending:
		return StatusPending
	case store.WorkflowStepSuccess:
		return StatusSuccess
	default:
		return StatusQuarantined
	}
}

// eventTypeFor maps a runtime primitive name to a durable WorkflowEventType.
// The journal requires a non-empty event_type; unmapped primitives fall back to
// tool_call so the append always satisfies the NOT NULL constraint.
func eventTypeFor(primName string) store.WorkflowEventType {
	switch primName {
	case "agent":
		return store.WorkflowEventSpawnRequested
	case "gate":
		return store.WorkflowEventGateEval
	case "ctx_now":
		return store.WorkflowEventCtxNow
	case "ctx_uuid":
		return store.WorkflowEventCtxUUID
	default:
		return store.WorkflowEventToolCall
	}
}
