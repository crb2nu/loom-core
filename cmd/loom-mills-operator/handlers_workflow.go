package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

type workflowCanaryStartRequest struct {
	RunID     string `json:"run_id"`
	AgentType string `json:"agent_type"`
	// Merging selects the S6-full merging canary (template v3): the run's
	// script gains a single journaled merge('canary') effect after the gate.
	Merging bool `json:"merging"`
}

func readWorkflowCanaryStartRequest(r *http.Request) (workflowCanaryStartRequest, error) {
	var request workflowCanaryStartRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		return workflowCanaryStartRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return workflowCanaryStartRequest{}, errors.New("multiple JSON values are not allowed")
		}
		return workflowCanaryStartRequest{}, err
	}
	request.RunID = strings.TrimSpace(request.RunID)
	request.AgentType = strings.TrimSpace(request.AgentType)
	return request, nil
}

func validWorkflowCanaryRunID(id string) bool {
	if !strings.HasPrefix(id, "wf-canary-") || len(id) > 96 || len(id) == len("wf-canary-") {
		return false
	}
	for _, char := range id[len("wf-canary-"):] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func isMatchingWorkflowCanary(run *store.WorkflowRun, backlogID, agentType string, merging bool) bool {
	storedAgentType, err := workflow.CanaryAgentTypeFromRun(run)
	if err != nil {
		return false
	}
	storedMerging, err := workflow.CanaryMergingFromRun(run)
	if err != nil || storedMerging != merging {
		return false
	}
	wantVersion := workflow.CanaryTemplateVersion
	if merging {
		wantVersion = workflow.CanaryMergingTemplateVersion
	}
	return storedAgentType == agentType && run.BacklogID == backlogID && run.Engine == store.WorkflowEngineImperative &&
		run.Template == workflow.CanaryTemplateName && run.TemplateVersion == wantVersion &&
		run.InterpreterVersion == workflow.HostInterpreterVersion
}

func writeWorkflowCanaryStart(w http.ResponseWriter, run *store.WorkflowRun) {
	agentType, err := workflow.CanaryAgentTypeFromRun(run)
	if err != nil {
		http.Error(w, "invalid persisted canary identity", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         run.ID,
		"agent_type": agentType,
		"engine":     string(run.Engine),
		"template":   run.Template,
		"state":      string(run.State),
		"backlog_id": run.BacklogID,
		"started_at": run.StartedAt,
	})
}

// handleWorkflowCanaryStart launches the S6-min canary imperative run for the
// S1c deployed dual-crash kill-test. workflow.CreateImperativeRun is otherwise
// an in-process Go func (S7 council selection does not exist yet), so S1c had
// no remote way to enqueue an imperative run. Admin-gated + mutating. The
// endpoint is intentionally available only inside the audited S1c window:
// global policy.enabled=false closes every ordinary admission while
// policy.workflows.enabled=true advances this one singleton run.
func (o *operator) handleWorkflowCanaryStart(w http.ResponseWriter, r *http.Request) {
	request, err := readWorkflowCanaryStartRequest(r)
	if err != nil {
		http.Error(w, "invalid canary request: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := request.RunID
	if id == "" {
		id = "wf-canary-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	}
	if !validWorkflowCanaryRunID(id) {
		http.Error(w, "run_id must match wf-canary-[a-z0-9-] and be at most 96 characters", http.StatusBadRequest)
		return
	}
	agentType, err := workflow.ResolveCanaryAgentType(request.AgentType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	backlogID := r.URL.Query().Get("backlog_id")

	// The deployed crash canary is the one sanctioned exception to the global
	// work-admission barrier: policy.enabled must be false (all other producers
	// drained) while workflows.enabled remains true (this run may advance).
	// Serialize the check+create path so two admin callers cannot both observe
	// zero and enqueue competing canaries in the singleton operator.
	o.workflowCanaryMu.Lock()
	defer o.workflowCanaryMu.Unlock()
	// The caller supplies a stable id before sending the request. A response
	// loss can therefore retry the same POST without creating a second run or
	// reviving a terminal one. Identity mismatches fail closed.
	existing, getErr := o.store.Workflow.GetWorkflowRun(r.Context(), id)
	if getErr == nil {
		if !isMatchingWorkflowCanary(existing, backlogID, agentType, request.Merging) {
			http.Error(w, "run_id already belongs to a different workflow identity", http.StatusConflict)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Mills-Idempotent-Replay", "true")
		writeWorkflowCanaryStart(w, existing)
		return
	}
	if !errors.Is(getErr, store.ErrNotFound) {
		http.Error(w, "workflow lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	// Make the complete check→create transaction visible to external safety
	// readers. The internal proof below allows exactly this one admission;
	// public quiescence continues to require zero.
	o.admissionMu.Lock()
	if o.crashLeaseActiveLocked(time.Now().UTC()) {
		o.admissionMu.Unlock()
		http.Error(w, "canary admission locked during active crash lease", http.StatusLocked)
		return
	}
	o.canaryAdmissions.Add(1)
	o.admissionMu.Unlock()
	defer o.canaryAdmissions.Add(-1)
	policy := o.policy.Current()
	policyGeneration := o.policyGeneration.Load()
	if policy == nil || policy.IsEnabled() {
		http.Error(w, "close global work admission with policy.enabled=false before launching the crash canary", http.StatusConflict)
		return
	}
	if !policy.WorkflowsEnabled() {
		http.Error(w, "workflow runtime is disabled", http.StatusServiceUnavailable)
		return
	}
	snapshot, err := o.readSafetyQuiescenceWithCanaryAllowance(r.Context(), 1)
	if err != nil {
		http.Error(w, "quiescence unavailable", http.StatusServiceUnavailable)
		return
	}
	if !snapshot.Quiescent {
		http.Error(w, "Mills is not fully quiescent; refusing competing crash canary", http.StatusConflict)
		return
	}
	if snapshot.InMemory.policyIdentity != policy || snapshot.InMemory.PolicyGeneration != policyGeneration {
		http.Error(w, "policy changed during the quiescence proof; retry the canary admission", http.StatusConflict)
		return
	}
	// Re-check under the same mutex ordinary admission uses. The dedicated
	// canaryAdmissions counter already covers the full check→commit lifetime.
	o.admissionMu.Lock()
	currentPolicy := o.policy.Current()
	if currentPolicy != policy || o.policyGeneration.Load() != policyGeneration ||
		currentPolicy == nil || currentPolicy.IsEnabled() || !currentPolicy.WorkflowsEnabled() {
		o.admissionMu.Unlock()
		http.Error(w, "policy changed before canary creation; retry", http.StatusConflict)
		return
	}
	// backlog_id is OPTIONAL. workflow_runs.backlog_id is a FK to
	// backlog_items (ON DELETE SET NULL, foreign_keys=ON); a non-existent id
	// violates the constraint and 500s. Empty stores NULL,
	// which is valid (the canary run has no backing backlog item). A caller
	// that does pass an id must reference a real item.
	run, err := workflow.CreateImperativeRunWithOptions(r.Context(), o.store.Workflow, id, backlogID, agentType, request.Merging)
	if err == nil {
		currentPolicy = o.policy.Current()
		if currentPolicy != policy || o.policyGeneration.Load() != policyGeneration ||
			currentPolicy == nil || currentPolicy.IsEnabled() || !currentPolicy.WorkflowsEnabled() {
			now := time.Now().UTC()
			run.State = store.WorkflowRunError
			run.EndedAt = &now
			terminalized, transitionErr := o.store.Workflow.CompareAndSetWorkflowRunLifecycle(
				r.Context(), run, store.WorkflowRunRunning,
			)
			o.admissionMu.Unlock()
			if transitionErr != nil || !terminalized {
				http.Error(w, fmt.Sprintf("policy changed after canary creation and fail-safe terminalization failed: updated=%t error=%v",
					terminalized, transitionErr), http.StatusInternalServerError)
				return
			}
			http.Error(w, "policy changed during canary creation; the new run was terminalized", http.StatusConflict)
			return
		}
	}
	o.admissionMu.Unlock()
	if err != nil {
		if errors.Is(err, store.ErrWorkflowRunExists) {
			existing, getErr := o.store.Workflow.GetWorkflowRun(r.Context(), id)
			if getErr == nil && isMatchingWorkflowCanary(existing, backlogID, agentType, request.Merging) {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Mills-Idempotent-Replay", "true")
				writeWorkflowCanaryStart(w, existing)
				return
			}
			http.Error(w, "run_id was concurrently claimed by another workflow identity", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeWorkflowCanaryStart(w, run)
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
	ID                 string     `json:"id"`
	BacklogID          string     `json:"backlog_id,omitempty"`
	AgentType          string     `json:"agent_type,omitempty"`
	Engine             string     `json:"engine"`
	Template           string     `json:"template"`
	TemplateVersion    string     `json:"template_version"`
	InterpreterVersion string     `json:"interpreter_version"`
	State              string     `json:"state"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
	CostUSD            float64    `json:"cost_usd"`
	// StepCount is the number of journaled steps for the run. It is always
	// emitted (no omitempty): a run with no steps reports an honest 0 rather
	// than being indistinguishable from an older operator build that omitted
	// the field entirely. The HUD list keeps a `?? '—'` fallback so the
	// rollout window (old image still serving the omitted field) degrades to
	// the prior em-dash instead of a literal 0.
	StepCount int `json:"step_count"`
}

func summarizeWorkflowRun(run *store.WorkflowRun, stepCount int) (workflowRunSummary, error) {
	if run == nil {
		return workflowRunSummary{}, errors.New("nil workflow run")
	}
	agentType := ""
	if run.Engine == store.WorkflowEngineImperative && run.Template == workflow.CanaryTemplateName {
		var err error
		agentType, err = workflow.CanaryAgentTypeFromRun(run)
		if err != nil {
			return workflowRunSummary{}, err
		}
	}
	return workflowRunSummary{
		ID:                 run.ID,
		BacklogID:          run.BacklogID,
		AgentType:          agentType,
		Engine:             string(run.Engine),
		Template:           run.Template,
		TemplateVersion:    run.TemplateVersion,
		InterpreterVersion: run.InterpreterVersion,
		State:              string(run.State),
		StartedAt:          run.StartedAt,
		EndedAt:            run.EndedAt,
		CostUSD:            run.CostUSD,
		StepCount:          stepCount,
	}, nil
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

// ----- Workflow run lifecycle mutations --------------------------------------
//
// Admin-gated pause / resume / fail for one imperative run. Motivated by the
// 2026-07-09 wf-canary zombie loop: with only GET endpoints on this surface, a
// stuck run could not be mitigated live — clearing two dead canaries required
// a code deploy. These give the operator the same between-step controls the
// scheduler already honors (scheduler_min.go advance: fresh reload, skip when
// PausedAt != nil or State != running), so a pause/fail takes effect on the
// NEXT tick; an in-flight step finishes its current attempt first — the same
// eventually-consistent semantics as the policy kill switch.
//
// State machine (409 on any other transition, mirroring the Mills hand-off
// convention that 409 = state conflict):
//
//	pause:  running          → paused (PausedAt=now)
//	resume: paused           → running (ResumedAt=now, PausedAt cleared)
//	fail:   running | paused → error (EndedAt=now; terminal — the scheduler
//	        never advances it again, and there is deliberately NO un-fail)

// workflowRunLifecycleView is the mutation response: the run's lifecycle
// fields after the transition, so a caller can confirm the effect without a
// second GET.
type workflowRunLifecycleView struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	PausedAt  *time.Time `json:"paused_at,omitempty"`
	ResumedAt *time.Time `json:"resumed_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// loadWorkflowRunForMutation resolves {id} and loads the run, writing the
// appropriate 4xx/5xx itself. Returns nil when the response is already sent.
func (o *operator) loadWorkflowRunForMutation(w http.ResponseWriter, r *http.Request) *store.WorkflowRun {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return nil
	}
	run, err := o.store.Workflow.GetWorkflowRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "workflow run not found", http.StatusNotFound)
			return nil
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	return run
}

// transitionWorkflowRunAndRespond atomically persists a lifecycle mutation and
// writes the view. A concurrent runtime/manual transition returns 409 instead
// of overwriting the winner with a stale load.
func (o *operator) transitionWorkflowRunAndRespond(
	w http.ResponseWriter,
	r *http.Request,
	run *store.WorkflowRun,
	expected store.WorkflowRunState,
) bool {
	updated, err := o.store.Workflow.CompareAndSetWorkflowRunLifecycle(r.Context(), run, expected)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	if !updated {
		current, getErr := o.store.Workflow.GetWorkflowRun(r.Context(), run.ID)
		if getErr != nil {
			http.Error(w, getErr.Error(), http.StatusInternalServerError)
			return false
		}
		http.Error(w, "workflow run changed concurrently to "+string(current.State), http.StatusConflict)
		return false
	}
	writeJSON(w, http.StatusOK, workflowRunLifecycleView{
		ID:        run.ID,
		State:     string(run.State),
		PausedAt:  run.PausedAt,
		ResumedAt: run.ResumedAt,
		EndedAt:   run.EndedAt,
	})
	return true
}

// handleWorkflowRunPause pauses a running imperative run between steps.
func (o *operator) handleWorkflowRunPause(w http.ResponseWriter, r *http.Request) {
	run := o.loadWorkflowRunForMutation(w, r)
	if run == nil {
		return
	}
	if run.State != store.WorkflowRunRunning {
		http.Error(w, "workflow run is "+string(run.State)+", not running", http.StatusConflict)
		return
	}
	now := time.Now().UTC()
	run.State = store.WorkflowRunPaused
	run.PausedAt = &now
	if o.transitionWorkflowRunAndRespond(w, r, run, store.WorkflowRunRunning) {
		o.logger.Info("workflow run paused by operator", "run_id", run.ID)
		o.appendOverrideEvent(r.Context(), "pause", "workflow_run", run.ID, overrideReason(r))
	}
}

// handleWorkflowRunResume resumes a paused imperative run. The next scheduler
// tick picks it up (ListRunningImperativeRuns filters state='running').
func (o *operator) handleWorkflowRunResume(w http.ResponseWriter, r *http.Request) {
	run := o.loadWorkflowRunForMutation(w, r)
	if run == nil {
		return
	}
	if run.State != store.WorkflowRunPaused {
		http.Error(w, "workflow run is "+string(run.State)+", not paused", http.StatusConflict)
		return
	}
	now := time.Now().UTC()
	run.State = store.WorkflowRunRunning
	run.PausedAt = nil
	run.ResumedAt = &now
	if o.transitionWorkflowRunAndRespond(w, r, run, store.WorkflowRunPaused) {
		o.logger.Info("workflow run resumed by operator", "run_id", run.ID)
		o.appendOverrideEvent(r.Context(), "resume", "workflow_run", run.ID, overrideReason(r))
	}
}

// workflowRunFailRequest is the optional POST body for /fail. workflow_runs has
// no reason column; the reason rides the operator.override event payload so the
// manual intervention is durable in the events store, not only in the log.
type workflowRunFailRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleWorkflowRunFail forces a non-terminal run to state='error'. This is
// the live-mitigation path for a wedged run (e.g. a zombie whose spawn died
// terminally under an older operator build): terminal, so the scheduler never
// advances it again. Deliberately not reversible over the API.
func (o *operator) handleWorkflowRunFail(w http.ResponseWriter, r *http.Request) {
	run := o.loadWorkflowRunForMutation(w, r)
	if run == nil {
		return
	}
	if run.State != store.WorkflowRunRunning && run.State != store.WorkflowRunPaused {
		http.Error(w, "workflow run is "+string(run.State)+" (terminal)", http.StatusConflict)
		return
	}
	var req workflowRunFailRequest
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	}
	if req.Reason == "" {
		req.Reason = "manual fail"
	}
	expected := run.State
	now := time.Now().UTC()
	run.State = store.WorkflowRunError
	run.EndedAt = &now
	if o.transitionWorkflowRunAndRespond(w, r, run, expected) {
		o.logger.Warn("workflow run failed by operator", "run_id", run.ID, "reason", req.Reason)
		o.appendOverrideEvent(r.Context(), "fail", "workflow_run", run.ID, req.Reason)
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
	// Batch the per-run step count so the list carries a real step_count
	// (instead of the detail-only field) without one count query per row.
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	stepCounts, err := o.store.Workflow.CountStepsByRun(ctx, ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]workflowRunSummary, 0, len(runs))
	for _, run := range runs {
		summary, err := summarizeWorkflowRun(run, stepCounts[run.ID])
		if err != nil {
			http.Error(w, "invalid persisted workflow identity", http.StatusInternalServerError)
			return
		}
		out = append(out, summary)
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
	summary, err := summarizeWorkflowRun(run, len(stepViews))
	if err != nil {
		http.Error(w, "invalid persisted workflow identity", http.StatusInternalServerError)
		return
	}
	payload := map[string]any{
		"run":   summary,
		"steps": stepViews,
	}
	// Detail-only: the frozen selection params (S7 registry runs) or canary
	// params. Opaque JSON the store never parses; the HUD pretty-prints it so
	// an operator can see exactly what identity the run is pinned to.
	if strings.TrimSpace(run.WorkflowParams) != "" {
		payload["workflow_params"] = run.WorkflowParams
	}
	writeJSON(w, http.StatusOK, payload)
}
