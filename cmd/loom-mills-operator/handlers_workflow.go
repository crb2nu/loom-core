package main

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

// handleWorkflowCanaryStart launches the S6-min canary imperative run for the
// S1c deployed dual-crash kill-test. workflow.CreateImperativeRun is otherwise
// an in-process Go func (S7 council selection does not exist yet), so S1c had
// no remote way to enqueue an imperative run. Admin-gated + mutating. Creating
// the run while policy.workflows.enabled=false is harmless — the WorkflowScheduler
// self-gates and won't advance it until the flag flips (the canary window).
func (o *operator) handleWorkflowCanaryStart(w http.ResponseWriter, r *http.Request) {
	// backlog_id is OPTIONAL. workflow_runs.backlog_id is a FK to
	// backlog_items (ON DELETE SET NULL, foreign_keys=ON); a non-existent id
	// violates the constraint and 500s. Empty → PutWorkflowRun stores NULL,
	// which is valid (the canary run has no backing backlog item). A caller
	// that does pass an id must reference a real item.
	backlogID := r.URL.Query().Get("backlog_id")
	id := "wf-canary-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	run, err := workflow.CreateImperativeRun(r.Context(), o.store.Workflow, id, backlogID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         run.ID,
		"engine":     string(run.Engine),
		"template":   run.Template,
		"state":      string(run.State),
		"backlog_id": run.BacklogID,
		"started_at": run.StartedAt,
	})
}

// Mills durable-workflow step-log HUD backend (plan .loom/134 §S4a). These
// endpoints make the workflow_runs / workflow_steps journal (migration 004)
// queryable over the operator's REST surface so an operator can observe an
// imperative run's step log during the S1c kill-test instead of
// `kubectl exec … sqlite3 state.db`. They mirror the pipeline detail pattern
// (handlePipelineRunGet: one call returns the run + its nested children) and
// the JSON-shape style of the existing /api/mills/* read handlers.
//
// Read-only and additive: nothing here mutates the journal or touches the DAG
// pipeline endpoints.

const (
	// workflowRunsDefaultLimit / workflowRunsMaxLimit bound the list endpoint so
	// a poll can never request an unbounded scan of the journal.
	workflowRunsDefaultLimit = 50
	workflowRunsMaxLimit     = 200
)

// workflowRunSummary is the per-row shape returned by
// GET /api/mills/workflow/runs. Deliberately a flat summary (no nested steps)
// so the list view stays cheap; the detail endpoint carries the steps.
type workflowRunSummary struct {
	ID        string     `json:"id"`
	BacklogID string     `json:"backlog_id,omitempty"`
	Engine    string     `json:"engine"`
	Template  string     `json:"template"`
	State     string     `json:"state"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	CostUSD   float64    `json:"cost_usd"`
	StepCount int        `json:"step_count,omitempty"`
}

// workflowStepView is one step in the detail response. It carries the raw
// journal fields that matter for S1c plus a server-derived `badge` hint so a
// future UI (S4b) can render cache-hit / live / quarantined without
// re-implementing the derivation client-side.
type workflowStepView struct {
	ID          int64      `json:"id"`
	StepKey     string     `json:"step_key"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	SpawnID     string     `json:"spawn_id,omitempty"`
	CallHash    string     `json:"call_hash"`
	CostUSD     float64    `json:"cost_usd"`
	CostSource  string     `json:"cost_source,omitempty"`
	EffectCount int        `json:"effect_count"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// Badge is the server-derived render hint: "quarantined" | "cache_hit" |
	// "live" | "pending" | "failed". See deriveStepBadge for the rules.
	Badge string `json:"badge"`
}

// Step badge values. Computed server-side so the UI never has to re-derive the
// cache-hit heuristic (and so the heuristic has exactly one home).
const (
	stepBadgeQuarantined = "quarantined"
	stepBadgeCacheHit    = "cache_hit"
	stepBadgeLive        = "live"
	stepBadgePending     = "pending"
	stepBadgeFailed      = "failed"
)

// deriveStepBadge computes the per-step render hint from the journal row plus
// the owning run's state. The rules, in priority order:
//
//   - runQuarantined → "quarantined": the run was quarantined for
//     nondeterminism (call_hash mismatch). Every step under it is flagged so the
//     UI can shade the whole run, regardless of the individual step status.
//   - status not terminal (pending) → "pending": the record-before-result row
//     whose effect has not completed (or was interrupted by a crash).
//   - status is a failure (error / gate_fail) → "failed".
//   - terminal success with effect_count == 0 → "cache_hit": a success that
//     performed no live side effect is a replay / memoized step (the durable
//     journal short-circuited it on resume). This is the replay==cache-hit
//     signal §S4 calls for.
//   - terminal success with effect_count > 0 → "live": the step actually ran an
//     effect this execution.
//
// `skipped` steps are treated as cache-hits (no live effect, deterministically
// short-circuited) so they render the same neutral badge as a replay.
func deriveStepBadge(st *store.WorkflowStep, runQuarantined bool) string {
	if runQuarantined {
		return stepBadgeQuarantined
	}
	switch st.Status {
	case store.WorkflowStepPending:
		return stepBadgePending
	case store.WorkflowStepError, store.WorkflowStepGateFail:
		return stepBadgeFailed
	case store.WorkflowStepSkipped:
		return stepBadgeCacheHit
	case store.WorkflowStepSuccess:
		if st.EffectCount == 0 {
			return stepBadgeCacheHit
		}
		return stepBadgeLive
	default:
		// Unknown / empty status: fall back to live so an operator still sees
		// the row rather than a blank badge.
		return stepBadgeLive
	}
}

// handleWorkflowRunsList returns the most-recent workflow runs (summary shape),
// newest-first, bounded by limit= (default 50, max 200). It is the read-only
// list view for the HUD step-log panel; without it the panel could only ever
// fetch a run by id it already knew.
func (o *operator) handleWorkflowRunsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := workflowRunsDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > workflowRunsMaxLimit {
		limit = workflowRunsMaxLimit
	}

	runs, err := o.store.Workflow.ListWorkflowRuns(ctx, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]workflowRunSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, workflowRunSummary{
			ID:        run.ID,
			BacklogID: run.BacklogID,
			Engine:    string(run.Engine),
			Template:  run.Template,
			State:     string(run.State),
			StartedAt: run.StartedAt,
			EndedAt:   run.EndedAt,
			CostUSD:   run.CostUSD,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// handleWorkflowRunGet returns one run plus its nested steps (from
// WorkflowDAO.ListByRun, the same append-ordered replay log the runtime
// consumes) with a server-derived per-step badge. One call replaces a run
// lookup + a step list so the HUD can render a step-log drawer in a single
// request — mirroring handlePipelineRunGet.
func (o *operator) handleWorkflowRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, err := o.store.Workflow.GetWorkflowRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "workflow run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	steps, err := o.store.Workflow.ListByRun(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	quarantined := run.State == store.WorkflowRunQuarantined
	stepViews := make([]workflowStepView, 0, len(steps))
	for _, st := range steps {
		stepViews = append(stepViews, workflowStepView{
			ID:          st.ID,
			StepKey:     st.StepKey,
			EventType:   string(st.EventType),
			Status:      string(st.Status),
			SpawnID:     st.SpawnID,
			CallHash:    st.CallHash,
			CostUSD:     st.CostUSD,
			CostSource:  string(st.CostSource),
			EffectCount: st.EffectCount,
			StartedAt:   st.StartedAt,
			EndedAt:     st.EndedAt,
			Badge:       deriveStepBadge(st, quarantined),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run": workflowRunSummary{
			ID:        run.ID,
			BacklogID: run.BacklogID,
			Engine:    string(run.Engine),
			Template:  run.Template,
			State:     string(run.State),
			StartedAt: run.StartedAt,
			EndedAt:   run.EndedAt,
			CostUSD:   run.CostUSD,
			StepCount: len(stepViews),
		},
		"steps": stepViews,
	})
}
