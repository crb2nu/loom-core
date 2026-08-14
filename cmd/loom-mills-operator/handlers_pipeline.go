package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// handlePipelineRunsList returns pipeline runs for the HUD pipeline panel.
//
// Query params (checked in precedence order):
//   - mr_iid=N      → the run(s) that produced that merge request (terminal
//     or not), powering the HUD "audit by MR iid" lookup (Loop B attribution).
//   - state=terminal → finished runs (done / escalated / paused), newest-first,
//     bounded by since= (RFC3339, default last 7d) and limit= (default 50,
//     max 200). This is the run-history view; without it the panel could only
//     ever show in-flight work and a run vanished the moment it merged.
//
// With no params it returns active (non-terminal) runs — the historical
// default, kept for back-compat with existing callers.
func (o *operator) handlePipelineRunsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if raw := q.Get("mr_iid"); raw != "" {
		mrIID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "mr_iid must be an integer", http.StatusBadRequest)
			return
		}
		runs, err := o.store.Pipeline.ListByMRIID(ctx, mrIID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if runs == nil {
			runs = []*store.PipelineRun{}
		}
		writeJSON(w, http.StatusOK, runs)
		return
	}
	if state := q.Get("state"); state == "terminal" || state == "history" {
		var since time.Time
		if raw := q.Get("since"); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				http.Error(w, "since must be an RFC3339 timestamp", http.StatusBadRequest)
				return
			}
			since = t
		} else {
			since = time.Now().Add(-7 * 24 * time.Hour)
		}
		limit := 50
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
				return
			}
			limit = n
		}
		runs, err := o.store.Pipeline.ListRecentTerminal(ctx, since, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if runs == nil {
			runs = []*store.PipelineRun{}
		}
		writeJSON(w, http.StatusOK, runs)
		return
	}
	// "Active" is the union of every non-terminal state, fetched in one
	// query — listing each state separately raced a state-transition
	// mid-call and cost 9 round-trips per HUD poll. The DAO's CountActive
	// uses the same predicate so the two endpoints agree.
	all, err := o.store.Pipeline.ListActive(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Encode an empty active set as `[]`, not `null` — matching the mr_iid and
	// terminal branches above. A bare `null` body forces every client to
	// special-case it (the mobile app surfaced an empty operator as
	// "Couldn't reach Mills").
	if all == nil {
		all = []*store.PipelineRun{}
	}
	writeJSON(w, http.StatusOK, all)
}

// handlePipelineRunGet returns a pipeline run with its stage results
// and gate outcomes nested inline. One call replaces three so HUD can
// render a detail drawer in a single request.
func (o *operator) handlePipelineRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, err := o.store.Pipeline.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "pipeline run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stages, _ := o.store.Pipeline.ListStages(ctx, id)
	gates, _ := o.store.Pipeline.ListGates(ctx, id)
	// Encode empty stage/gate sets as `[]`, not `null` — same contract as the
	// list endpoint above. Live runs have no gate outcomes until a stage
	// completes, so `null` here is the common case, and it crashed the HUD
	// drawer for every in-flight run.
	if stages == nil {
		stages = []*store.StageResult{}
	}
	if gates == nil {
		gates = []*store.GateOutcome{}
	}
	payload := map[string]any{
		"run":      run,
		"evidence": o.runEvidence(ctx, run),
		"stages":   stages,
		"gates":    gates,
	}
	// Merge-queue projection: present only when the run has an entry, so
	// pre-queue runs and disabled deployments serve the exact prior shape.
	if o.store.MergeQueue != nil {
		if entry, qerr := o.store.MergeQueue.Get(ctx, id); qerr == nil {
			position := 0
			if !entry.State.IsTerminal() {
				position, _ = o.store.MergeQueue.Position(ctx, id)
			}
			payload["merge_queue"] = map[string]any{
				"state":           string(entry.State),
				"position":        position,
				"eviction_reason": entry.EvictionReason,
				"enqueued_at":     entry.EnqueuedAt,
				"current_sha":     entry.CurrentSHA,
				"merged_sha":      entry.MergedSHA,
			}
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

type pipelineRunVerdictRequest struct {
	Class   string `json:"class"`
	Outcome string `json:"outcome"`
	MRIID   int64  `json:"mr_iid"`
	Note    string `json:"note"`
}

// handlePipelineRunVerdict appends an immutable, replay-safe correction after
// independently confirming that the manually rescued run's MR is merged.
func (o *operator) handlePipelineRunVerdict(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req pipelineRunVerdictRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid verdict request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid verdict request: request body must contain one JSON object", http.StatusBadRequest)
		return
	}
	if req.Class != mills.RunVerdictClassMergedAfterEscalation || strings.TrimSpace(req.Outcome) == "" || req.MRIID <= 0 {
		http.Error(w, "class must be merged_after_escalation, outcome is required, and mr_iid must be positive", http.StatusUnprocessableEntity)
		return
	}
	ctx := r.Context()
	run, err := o.store.Pipeline.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "pipeline run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if run.State != store.PipelineEscalated || run.EndedAt == nil {
		http.Error(w, "pipeline run is not terminal-escalated", http.StatusConflict)
		return
	}
	if run.MRIID != nil && *run.MRIID > 0 && *run.MRIID != req.MRIID {
		http.Error(w, "mr_iid does not match the pipeline run", http.StatusConflict)
		return
	}
	if o.verdictMRStateForProject == nil {
		http.Error(w, "GitLab MR verification unavailable", http.StatusServiceUnavailable)
		return
	}
	project := ""
	if o.verdictProjectResolver != nil {
		project, err = o.verdictProjectResolver.AuthorizedProject(ctx, run.ID)
		// Only legacy pre-MR runs may use the configured home-project
		// fallback. Resolver/DB failures and runs that already name an MR fail
		// closed instead of silently changing verification scope.
		if err != nil && (!errors.Is(err, store.ErrPipelineProjectUnavailable) || run.MRIID != nil) {
			http.Error(w, "GitLab project verification unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if strings.TrimSpace(project) == "" {
		project = o.verdictDefaultProject
	}
	if strings.TrimSpace(project) == "" {
		http.Error(w, "GitLab project verification unavailable", http.StatusServiceUnavailable)
		return
	}
	mrClient := o.verdictMRStateForProject(project)
	if mrClient == nil {
		http.Error(w, "GitLab MR verification unavailable", http.StatusServiceUnavailable)
		return
	}
	state, err := mrClient.MRState(ctx, req.MRIID)
	if err != nil {
		http.Error(w, "GitLab MR verification failed", http.StatusBadGateway)
		return
	}
	if state != "merged" {
		http.Error(w, "merge request is not merged", http.StatusConflict)
		return
	}
	priorClass := run.EscalationClass
	if priorClass == "" {
		priorClass = run.FailureClass
	}
	if priorClass == "" {
		priorClass = "code"
	}
	appended, err := o.store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor: "operator", Kind: mills.RunVerdictKindOperatorOverride,
		SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: run.ID,
		OccurredAt: time.Now().UTC(),
		Payload: map[string]any{
			"class": req.Class, "prior_class": priorClass, "outcome": strings.TrimSpace(req.Outcome),
			"actor": "operator", "mr_iid": req.MRIID, "note": strings.TrimSpace(req.Note),
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := o.store.Events.ListBySubject(ctx, store.JudgeVerdictSubjectKind, run.ID, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if appended {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"appended": appended, "verdict": mills.ResolveRunVerdict(run, events)})
}

// runEvidence assembles the ground-truth evidence block for a run's detail
// payload: judge verdicts and the provenance stamp read off the run's own
// event subject, and — when the run produced an MR — the post-merge
// regression attribution keyed on that MR. Best-effort by contract: a
// missing or failing events read yields the empty shape, never an error,
// because the drawer must render for live runs that have no evidence yet.
func (o *operator) runEvidence(ctx context.Context, run *store.PipelineRun) map[string]any {
	evidence := map[string]any{
		"verdicts":   []map[string]any{},
		"provenance": nil,
		"regression": nil,
		"verdict":    nil,
	}
	if run == nil || o.store == nil || o.store.Events == nil {
		if run != nil {
			evidence["verdict"] = mills.ResolveRunVerdict(run, nil)
		}
		return evidence
	}
	events, err := o.store.Events.ListBySubject(ctx, store.JudgeVerdictSubjectKind, run.ID, 200)
	if err != nil {
		o.logger.Warn("run evidence: list events failed", "run", run.ID, "error", err)
		evidence["verdict"] = mills.ResolveRunVerdict(run, nil)
		return evidence
	}
	// The run's current-belief verdict (Trustworthy Verdicts S1): resolved
	// from the same event subject, so a ghost-spark-corrected escalation
	// reads merged_after_escalation here while the run row keeps its
	// immutable terminal record.
	evidence["verdict"] = mills.ResolveRunVerdict(run, events)
	// Oldest-first so retried gates read as a chronology in the drawer.
	verdicts := make([]map[string]any, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev == nil {
			continue
		}
		switch ev.Kind {
		case store.JudgeVerdictEventKind:
			verdicts = append(verdicts, eventWithTime(ev))
		case mills.RunProvenanceEventKind:
			// First-writer stamp; keep the earliest if several survived.
			if evidence["provenance"] == nil {
				evidence["provenance"] = eventWithTime(ev)
			}
		}
	}
	evidence["verdicts"] = verdicts
	if run.MRIID != nil && *run.MRIID > 0 {
		reg, err := o.store.Events.FirstBySubjectKind(ctx,
			"merge_request", strconv.FormatInt(*run.MRIID, 10), mills.RegressionAttributedEventKind)
		if err == nil && reg != nil {
			evidence["regression"] = eventWithTime(reg)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			o.logger.Warn("run evidence: regression lookup failed", "run", run.ID, "error", err)
		}
	}
	return evidence
}

// eventWithTime flattens an event's payload plus its occurred_at stamp — the
// drawer renders payload fields and needs the when without the audit plumbing.
func eventWithTime(ev *store.Event) map[string]any {
	out := make(map[string]any, len(ev.Payload)+1)
	for k, v := range ev.Payload {
		out[k] = v
	}
	out["occurred_at"] = ev.OccurredAt
	return out
}

// handlePipelineRunTransitions returns the run's MR head-movement ledger
// (migration 016), newest-first. Open read, like GET /pipeline/runs/{id}: it
// exposes SHAs and branch names the run row already carries, and an operator
// diagnosing a fail-closed merge needs it without an admin token.
//
// Empty ledgers encode as `[]`, never `null` — the same contract every other
// list branch in this file honours, because a bare null forces each client to
// special-case it (it crashed the HUD drawer once already).
func (o *operator) handlePipelineRunTransitions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if _, err := o.store.Pipeline.GetRun(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "pipeline run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transitions, err := o.store.MRHeadTransitions.ListByRun(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if transitions == nil {
		transitions = []*store.MRHeadTransition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":      id,
		"transitions": transitions,
	})
}

type pipelineStartResponse struct {
	RunID     string   `json:"run_id,omitempty"`
	BacklogID string   `json:"backlog_id"`
	Decision  string   `json:"decision"`
	State     string   `json:"state,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Blockers  []string `json:"blockers,omitempty"`
}

// handlePipelineStart asks the reconciler to start one queued backlog item now.
// This uses the same fail-closed autonomy, dependency, budget, squad routing,
// and PipelineStarter path as the scheduler tick; the endpoint only narrows the
// target to one backlog id so humans can prove the operator on demand.
//
// ?requeue=1 additionally flips an escalated item back to queued before
// starting — the human re-run-after-escalation path (a plan hand-off whose
// previous run escalated used to dead-end on 409 with no recovery). Items in
// other non-queued states still return 409 with the state in `reason`.
func (o *operator) handlePipelineStart(w http.ResponseWriter, r *http.Request) {
	if o.reconciler == nil {
		http.Error(w, "reconciler not configured", http.StatusServiceUnavailable)
		return
	}
	backlogID := r.PathValue("backlog_id")
	if backlogID == "" {
		http.Error(w, "missing backlog id", http.StatusBadRequest)
		return
	}
	requeue := truthyQuery(r, "requeue")
	res, err := o.reconciler.StartQueuedItemOpts(r.Context(), backlogID, mills.StartQueuedOptions{
		RequeueEscalated: requeue,
	})
	resp := pipelineStartResponse{
		BacklogID: res.BacklogID,
		Decision:  res.Decision,
		Reason:    res.Reason,
		Blockers:  res.Blockers,
	}
	if resp.BacklogID == "" {
		resp.BacklogID = backlogID
	}
	if res.Run != nil {
		resp.RunID = res.Run.ID
		resp.State = string(res.Run.State)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "backlog item not found", http.StatusNotFound)
		case errors.Is(err, mills.ErrPolicyDisabled):
			writeJSON(w, http.StatusForbidden, resp)
		case errors.Is(err, mills.ErrBacklogNotQueued):
			writeJSON(w, http.StatusConflict, resp)
		default:
			var blocked *mills.AutonomyBlockedError
			if errors.As(err, &blocked) {
				writeJSON(w, http.StatusForbidden, resp)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if res.Run == nil {
		writeJSON(w, http.StatusConflict, resp)
		return
	}
	// The reconciler's own requeue event is attributed to "reconciler"; label
	// the human decision separately so autonomous and manual starts stay
	// distinguishable in the events store.
	action := "start"
	if requeue {
		action = "requeue"
	}
	o.appendOverrideEvent(r.Context(), action, "backlog_item", resp.BacklogID, overrideReason(r))
	writeJSON(w, http.StatusCreated, resp)
}

type pipelinePauseRequest struct {
	Reason string `json:"reason"`
}

func (o *operator) handlePipelinePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req pipelinePauseRequest
	if r.Body != nil && json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	run, err := o.store.Pipeline.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "pipeline run not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if run.State == store.PipelinePaused {
		writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "state": "paused", "reason": req.Reason})
		return
	}
	if store.IsPipelineTerminalState(run.State) {
		http.Error(w, "pipeline run is "+string(run.State)+", not active", http.StatusConflict)
		return
	}
	item, err := o.store.Backlog.Get(r.Context(), run.BacklogID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "backlog item not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	if err := o.store.Pipeline.PauseRunWithBacklog(r.Context(), run, item.State, now); err != nil {
		var stale *store.StaleWriteError
		if errors.As(err, &stale) {
			http.Error(w, "pipeline run changed; retry", http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if o.spawnClient != nil {
		stages, _ := o.store.Pipeline.ListStages(r.Context(), run.ID)
		for _, stage := range stages {
			if stage.SpawnID != "" {
				if err := o.spawnClient.Stop(r.Context(), stage.SpawnID); err != nil {
					o.logger.Warn("pipeline pause spawn stop failed", "run", run.ID, "spawn", stage.SpawnID, "error", err)
				}
			}
		}
	}
	// Surface the operator's reason in the existing detail payload. The drawer
	// already renders stage artifacts, so this avoids a second audit endpoint.
	pauseOutcome := store.StageOutcomeSuccess
	if err := o.store.Pipeline.PutStage(r.Context(), &store.StageResult{
		PipelineRunID: run.ID, Stage: "operator_pause", Attempt: 1,
		StartedAt: now, EndedAt: &now, Outcome: &pauseOutcome,
		Artifacts: map[string]any{"reason": req.Reason}, LogTail: "paused by operator: " + req.Reason,
	}); err != nil {
		o.logger.Warn("pipeline pause reason artifact failed", "run", run.ID, "error", err)
	}
	_ = o.store.Events.Append(r.Context(), &store.Event{
		Actor:       "operator",
		Kind:        "pipeline.paused",
		SubjectKind: "pipeline_run",
		SubjectID:   run.ID,
		Payload:     map[string]any{"reason": req.Reason},
	})
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "backlog_id": run.BacklogID, "state": "paused", "reason": req.Reason})
}

func (o *operator) handlePipelineResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	run, err := o.store.Pipeline.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "pipeline run not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if run.State != store.PipelinePaused {
		http.Error(w, "pipeline run is "+string(run.State)+", not paused", http.StatusConflict)
		return
	}
	item, err := o.store.Backlog.Get(r.Context(), run.BacklogID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "backlog item not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := o.store.Pipeline.ResumePausedRunWithBacklog(r.Context(), run, item.State); err != nil {
		http.Error(w, "pipeline run changed; retry", http.StatusConflict)
		return
	}
	o.appendOverrideEvent(r.Context(), "resume", "pipeline_run", run.ID, overrideReason(r))
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "backlog_id": run.BacklogID, "state": "queued"})
}

type pipelineEscalateRequest struct {
	Reason string `json:"reason,omitempty"`
}

type pipelineGradeRequest struct {
	Grade string `json:"grade"`
	Note  string `json:"note,omitempty"`
}

func (o *operator) handlePipelineGrade(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req pipelineGradeRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid grade request", http.StatusUnprocessableEntity)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		http.Error(w, "invalid grade request", http.StatusUnprocessableEntity)
		return
	}
	item, err := mills.GradeRun(r.Context(), o.store, id, req.Grade, req.Note, operatorOverrideActor)
	if err != nil {
		switch {
		case errors.Is(err, mills.ErrInvalidGrade), errors.Is(err, mills.ErrInvalidGradeNote), errors.Is(err, mills.ErrNotGradable):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "pipeline run not found", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": id, "item_id": item.ID, "grade": item.Grade,
		"note": item.GradeNote, "actor": item.GradeActor, "graded_at": item.GradedAt,
	})
}

func (o *operator) handlePipelineEscalate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	run, err := o.store.Pipeline.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "pipeline run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var req pipelineEscalateRequest
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	}
	if req.Reason == "" {
		req.Reason = "manual escalation"
	}
	now := time.Now().UTC()
	run.State = store.PipelineEscalated
	run.EndedAt = &now
	if err := o.store.Pipeline.PutRun(ctx, run); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Manual escalation is the fast-kill path for an open dependency dwell.
	// Its completion races timeout through the same first-writer-wins CAS.
	if o.reconciler != nil {
		if _, _, err := o.reconciler.CompleteExternalIncidentDwell(ctx, run, store.ExternalIncidentDwellFastKill, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	item, itemErr := o.store.Backlog.Get(ctx, run.BacklogID)
	if itemErr == nil {
		_, err := o.store.Backlog.TransitionState(
			ctx, item.ID, run.AggregateVersion, item.State, store.BacklogEscalated,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Freeze the item's TargetProject at escalation time so the ghost-spark
		// merged-branch sweep can authorize a cross-repo lookup for manually
		// escalated runs too. Best-effort — the escalation stands without it.
		if _, err := mills.AppendEscalationTargetBinding(ctx, o.store.Events, "operator", run, item); err != nil {
			o.logger.Warn("escalation target binding append failed", "run", run.ID, "error", err)
		}
	} else if !errors.Is(itemErr, store.ErrNotFound) {
		http.Error(w, itemErr.Error(), http.StatusInternalServerError)
		return
	}
	o.appendOverrideEvent(ctx, "force_escalate", "pipeline_run", run.ID, req.Reason)
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":     run.ID,
		"backlog_id": run.BacklogID,
		"state":      string(run.State),
		"reason":     req.Reason,
	})
}

// subrunCreateRequest is the admin POST body for the Phase 6 recursion
// entry point. Field names match the JSON the spawn-driver MCP tool
// (mills_pipeline_subrun_create, slice 6.2) will produce.
type subrunCreateRequest struct {
	BacklogID   string  `json:"backlog_id"`
	Template    string  `json:"template"`
	EstimateUSD float64 `json:"estimate_usd,omitempty"`
	SliceSpec   string  `json:"slice_spec,omitempty"`
}

// subrunCreateResponse is what we return on success. Carries the new
// run id so the caller can poll status; carries the parent depth +
// computed new depth as an audit hint.
type subrunCreateResponse struct {
	RunID       string `json:"run_id"`
	ParentRunID string `json:"parent_run_id"`
	Depth       int    `json:"depth"`
}

// handlePipelineSubrunCreate is the operator's POST entry point for
// v2 bounded recursion (Phase 6 slice 6.1).
//
// Errors map cleanly to the spec's acceptance strings:
//   - GuardDepthExceeded         → 400 + body "recursion_depth_exceeded: ..."
//   - GuardBudgetSubrunTooLarge  → 400 + body "budget_subrun_too_large: ..."
//   - GuardCycleDetected         → 400 + body "recursion_cycle_detected: ..."
//   - GuardRecursionDisabled     → 403 + body "recursion_disabled: ..."
//   - GuardParentNotFound        → 404 + body "recursion_parent_not_found: ..."
//   - any other GuardError       → 400 + body "<code>: ..."
//
// The Code prefix is stable so callers can switch on a string
// response or scrape the prefix in logs.
func (o *operator) handlePipelineSubrunCreate(w http.ResponseWriter, r *http.Request) {
	if o.subrunGuard == nil {
		http.Error(w, "subrun guard not configured", http.StatusServiceUnavailable)
		return
	}
	parentID := r.PathValue("id")
	if parentID == "" {
		http.Error(w, "missing parent run id", http.StatusBadRequest)
		return
	}

	var req subrunCreateRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	newID, err := o.subrunGuard.SubrunCreate(r.Context(), pipeline.SubrunRequest{
		ParentRunID: parentID,
		BacklogID:   req.BacklogID,
		Template:    req.Template,
		EstimateUSD: req.EstimateUSD,
		SliceSpec:   req.SliceSpec,
	})
	if err != nil {
		var ge *pipeline.GuardError
		if errors.As(err, &ge) {
			status := http.StatusBadRequest
			switch ge.Code {
			case pipeline.GuardRecursionDisabled:
				status = http.StatusForbidden
			case pipeline.GuardParentNotFound:
				status = http.StatusNotFound
			}
			http.Error(w, ge.Error(), status)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read back depth so the response matches what's persisted.
	persisted, perr := o.store.Pipeline.GetRun(r.Context(), newID)
	depth := 0
	if perr == nil && persisted != nil {
		depth = persisted.Depth
	}
	writeJSON(w, http.StatusCreated, subrunCreateResponse{
		RunID:       newID,
		ParentRunID: parentID,
		Depth:       depth,
	})
}
