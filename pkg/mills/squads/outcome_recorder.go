package squads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// SquadRoutedEventKind is the Kind written to the events log when the
// reconciler routes a queued backlog item to a squad. The OutcomeRecorder
// keys squad attribution off the first row with this Kind for a
// given pipeline_run_id.
const SquadRoutedEventKind = "reconciler.squad_routed"

// SquadRoutedWorkflowSubjectKind is the events.subject_kind for squad
// attribution of imperative (workflow-lane) runs, written at
// ClaimWorkflowStart commit time so both start lanes carry attribution.
//
// Outcome parity is DELIBERATELY deferred: every v1 registry template stops
// pre-merge, so every imperative terminal escalates its item for human
// review by design. Recording those terminals as squad outcomes would
// poison SuccessRate (whose only success signal is merged_clean) with
// structural failures. When a merging template class exists (J3 dobby
// cards), the workflow terminal settle maps done+merged → merged_clean and
// error/deadline → failed using this attribution.
const SquadRoutedWorkflowSubjectKind = "workflow_run"

// SquadRoutedEventPayload is the canonical payload shape for a squad
// attribution event. Persisted as JSON in the events table; read back
// by OutcomeRecorder.OnMerged.
type SquadRoutedEventPayload struct {
	RunID      string  `json:"run_id"`
	BacklogID  string  `json:"backlog_id"`
	SquadName  string  `json:"squad_name"`
	PathClass  string  `json:"path_class"`
	Confidence float64 `json:"confidence"`
	SampleSize int     `json:"sample_size"`
	Reason     string  `json:"reason,omitempty"`
}

// OutcomeRecorder writes squad_outcomes rows when a pipeline run merges,
// using the squad attribution emitted by the reconciler at routing time.
// It satisfies the same `OnMerged(ctx, run, item) error` shape as
// eval.OutcomeAttributor so the operator can chain both via a small
// composite hook (see WiredOnMerged).
//
// The recorder is best-effort: a missing or unparseable attribution
// event is logged and the merge proceeds without a squad_outcomes row.
// The eval Loop B attribution still fires.
type OutcomeRecorder struct {
	Store  *store.Store
	Logger *slog.Logger
}

// NewOutcomeRecorder constructs a recorder backed by the canonical store.
func NewOutcomeRecorder(st *store.Store) *OutcomeRecorder {
	return &OutcomeRecorder{Store: st}
}

// OnMerged records a squad_outcomes row attributing the merge to the
// squad chosen at routing time. It looks up the attribution event by
// run.ID; if none is found (item routed to FallbackName, or routing
// happened before squads were enabled) the call is a no-op.
//
// Errors writing the row are returned so the caller's chained hook can
// log; the returned error never blocks the merge.
func (r *OutcomeRecorder) OnMerged(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if r == nil || r.Store == nil || run == nil {
		return errors.New("squads: outcome recorder not configured")
	}
	payload, ok, err := r.lookupAttribution(ctx, run.ID)
	if err != nil {
		r.warn("attribution lookup failed", "error", err, "run", run.ID)
		return err
	}
	if !ok {
		return nil // no squad routing event for this run; not an error
	}
	if payload.SquadName == "" || payload.SquadName == FallbackName {
		return nil // routed to default; nothing to attribute
	}
	out := &store.SquadOutcome{
		SquadName:       payload.SquadName,
		PathClass:       payload.PathClass,
		PipelineRunID:   run.ID,
		Outcome:         store.SquadOutcomeMergedClean,
		Grade:           strings.TrimSpace(itemGrade(item)),
		CostUSD:         run.CostUSD,
		DurationSeconds: durationSeconds(run),
	}
	if err := r.Store.Squads.RecordOutcome(ctx, out); err != nil {
		// A unique-constraint violation here means the merge already had
		// a squad outcome recorded (idempotency). Don't surface as an
		// error; the regression-flip path keys on PipelineRunID anyway.
		if isAlreadyRecorded(err) {
			return nil
		}
		r.warn("record outcome failed", "error", err, "run", run.ID, "squad", payload.SquadName)
		return fmt.Errorf("squads: record outcome: %w", err)
	}
	// First record for this run: append a merge working-memory entry so
	// the HUD detail panel + squad planner have real history to read
	// (squad_memory previously had no production writer). Best-effort.
	r.appendMergeMemory(ctx, payload, run, item)
	return nil
}

func itemGrade(item *store.BacklogItem) string {
	if item == nil {
		return ""
	}
	return item.Grade
}

// OnEscalated records a `failed` squad outcome when a squad-routed run
// escalates (the real path — auto-retried transient escalations never
// reach this hook). Without it, squad success rate was structurally
// ~1.0: merges wrote merged_clean and failures wrote nothing, so the
// router's confidence signal carried no information.
//
// Satisfies the same hook shape as OnMerged; the operator wires it to
// pipeline.Runner.OnEscalated.
func (r *OutcomeRecorder) OnEscalated(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if r == nil || r.Store == nil || run == nil {
		return errors.New("squads: outcome recorder not configured")
	}
	_ = item // attribution is keyed on the run; item retained for hook-shape parity
	payload, ok, err := r.lookupAttribution(ctx, run.ID)
	if err != nil {
		r.warn("attribution lookup failed", "error", err, "run", run.ID)
		return err
	}
	if !ok {
		return nil // no squad routing event for this run; not an error
	}
	if payload.SquadName == "" || payload.SquadName == FallbackName {
		return nil // routed to default; nothing to attribute
	}
	out := &store.SquadOutcome{
		SquadName:       payload.SquadName,
		PathClass:       payload.PathClass,
		PipelineRunID:   run.ID,
		Outcome:         store.SquadOutcomeFailed,
		CostUSD:         run.CostUSD,
		DurationSeconds: durationSeconds(run),
	}
	if err := r.Store.Squads.RecordOutcome(ctx, out); err != nil {
		if isAlreadyRecorded(err) {
			return nil
		}
		r.warn("record failed outcome failed", "error", err, "run", run.ID, "squad", payload.SquadName)
		return fmt.Errorf("squads: record failed outcome: %w", err)
	}
	return nil
}

// mergeMemoryImportance is the default importance for append-on-merge
// working-memory entries: above the prune threshold, below convention/
// tech-debt entries a planner would author deliberately.
const mergeMemoryImportance = 0.5

// memoryPruneThreshold / memoryPruneAge implement the documented retention
// policy (store.SquadMemory: "the weekly pruner drops rows with
// importance < 0.3 older than 30 days"). No standalone pruner exists, so
// the recorder prunes opportunistically after each merge append.
const memoryPruneThreshold = 0.3

const memoryPruneAge = 30 * 24 * time.Hour

// appendMergeMemory writes the merge working-memory entry and prunes
// stale low-importance rows for the squad. Both operations are
// best-effort: failures log and never affect the merge path.
func (r *OutcomeRecorder) appendMergeMemory(ctx context.Context, payload SquadRoutedEventPayload, run *store.PipelineRun, item *store.BacklogItem) {
	title := "merge: " + run.ID
	body := fmt.Sprintf("Pipeline run %s merged (path class %s).", run.ID, payload.PathClass)
	refs := []string{run.ID}
	if item != nil {
		if item.Title != "" {
			title = "merge: " + item.Title
		}
		body = fmt.Sprintf("Backlog %q merged via run %s (path class %s).", item.Title, run.ID, payload.PathClass)
		refs = append(refs, item.ID)
	}
	mem := &store.SquadMemory{
		SquadName:  payload.SquadName,
		Kind:       store.SquadMemoryMerge,
		Title:      title,
		Body:       body,
		Refs:       refs,
		Importance: mergeMemoryImportance,
	}
	if err := r.Store.Squads.PutMemory(ctx, mem); err != nil {
		r.warn("append merge memory failed", "error", err, "run", run.ID, "squad", payload.SquadName)
		return
	}
	if _, err := r.Store.Squads.PruneMemory(ctx, payload.SquadName, memoryPruneThreshold, time.Now().UTC().Add(-memoryPruneAge)); err != nil {
		r.warn("prune squad memory failed", "error", err, "squad", payload.SquadName)
	}
}

// SquadRoutedSubjectKind is the events.subject_kind we use when
// recording squad attribution. Pairing it with the pipeline_run id
// puts the row in the existing idx_events_subject index so the
// recorder's lookup is one indexed query.
const SquadRoutedSubjectKind = "pipeline_run"

// lookupAttribution finds the first SquadRoutedEventKind row for a pipeline
// run. First-writer semantics keep attribution stable across outbox retries and
// also make legacy duplicate rows deterministic.
func (r *OutcomeRecorder) lookupAttribution(ctx context.Context, runID string) (SquadRoutedEventPayload, bool, error) {
	event, err := r.Store.Events.FirstBySubjectKind(
		ctx, SquadRoutedSubjectKind, runID, SquadRoutedEventKind,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SquadRoutedEventPayload{}, false, nil
		}
		return SquadRoutedEventPayload{}, false, err
	}
	if event == nil || event.Payload == nil {
		return SquadRoutedEventPayload{}, false, nil
	}
	return decodePayload(event.Payload), true, nil
}

// decodePayload extracts the typed fields from the stored map[string]any
// payload. Defensive: missing or wrong-typed fields default to their zero
// value rather than failing the lookup.
func decodePayload(p map[string]any) SquadRoutedEventPayload {
	out := SquadRoutedEventPayload{}
	if v, ok := p["run_id"].(string); ok {
		out.RunID = v
	}
	if v, ok := p["backlog_id"].(string); ok {
		out.BacklogID = v
	}
	if v, ok := p["squad_name"].(string); ok {
		out.SquadName = v
	}
	if v, ok := p["path_class"].(string); ok {
		out.PathClass = v
	}
	if v, ok := p["confidence"].(float64); ok {
		out.Confidence = v
	}
	if v, ok := p["sample_size"].(float64); ok {
		// JSON unmarshals all numbers to float64.
		out.SampleSize = int(v)
	} else if v, ok := p["sample_size"].(int); ok {
		out.SampleSize = v
	}
	if v, ok := p["reason"].(string); ok {
		out.Reason = v
	}
	return out
}

// durationSeconds returns the wall-clock seconds between StartedAt and
// EndedAt, or 0 if EndedAt is unset.
func durationSeconds(run *store.PipelineRun) int64 {
	if run == nil || run.EndedAt == nil {
		return 0
	}
	return int64(run.EndedAt.Sub(run.StartedAt).Seconds())
}

// isAlreadyRecorded reports whether the DAO error is a unique-constraint
// violation on squad_outcomes(pipeline_run_id). The recorder treats it
// as a benign no-op so OnMerged stays idempotent under double-fire.
func isAlreadyRecorded(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, "squad_outcomes")
}

func (r *OutcomeRecorder) warn(msg string, kv ...any) {
	if r == nil || r.Logger == nil {
		return
	}
	r.Logger.Warn("squads: "+msg, kv...)
}
