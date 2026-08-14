package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func workflowClaimSelection() WorkflowSelection {
	return WorkflowSelection{
		Engine:             WorkflowEngineImperative,
		Template:           "implement-gate",
		TemplateVersion:    "v1",
		InterpreterVersion: "starlark-go@test",
		ParamsJSON:         `{"registry_template":{"content_hash":"abc","params":{"budget_usd":1}}}`,
	}
}

func workflowClaimRequest(id string) ClaimWorkflowStartRequest {
	return ClaimWorkflowStartRequest{
		BacklogID:            id,
		ExpectedClaimVersion: 0,
		ExpectedRevision:     1,
		Selection:            workflowClaimSelection(),
		EstimateUSD:          1,
		Limits: PipelineStartLimits{
			MaxUSDPerRun:      10,
			MaxUSDPerDay:      1000,
			MaxRunsPerDay:     1000,
			MaxConcurrentRuns: 1000,
		},
		// Align with claimTestNow so cross-lane budget windows overlap in
		// tests that mix pipeline and workflow claims.
		Now: claimTestNow,
	}
}

// TestClaimWorkflowStart_CommitsCompleteBoundary: one transaction yields the
// running item, the imperative run with the frozen selection verbatim, an
// active reservation, and the aggregate transition — and deliberately NO
// pipeline run and NO dispatch intent.
func TestClaimWorkflowStart_CommitsCompleteBoundary(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-WF-CLAIM")

	res, err := st.ClaimWorkflowStart(ctx, workflowClaimRequest(item.ID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Backlog.State != BacklogRunning {
		t.Fatalf("item state = %q, want running", res.Backlog.State)
	}
	run := res.Run
	sel := workflowClaimSelection()
	if run.Engine != WorkflowEngineImperative || run.Template != sel.Template ||
		run.TemplateVersion != sel.TemplateVersion ||
		run.InterpreterVersion != sel.InterpreterVersion ||
		run.WorkflowParams != sel.ParamsJSON {
		t.Fatalf("frozen selection not stamped verbatim: %+v", run)
	}
	if !strings.HasPrefix(run.ID, "WF-"+item.ID+"-") {
		t.Fatalf("run id %q missing WF-<item>- prefix", run.ID)
	}
	if res.Reservation == nil || res.Reservation.State != reservationStateActive {
		t.Fatalf("reservation missing or inactive: %+v", res.Reservation)
	}

	// Durable state: run row present + imperative-listable.
	fresh, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil || fresh.State != WorkflowRunRunning {
		t.Fatalf("stored run: %+v err=%v", fresh, err)
	}
	running, err := st.Workflow.ListRunningImperativeRuns(ctx)
	if err != nil || len(running) != 1 || running[0].ID != run.ID {
		t.Fatalf("imperative listing = %v err=%v, want the claimed run", running, err)
	}

	// No pipeline lane artifacts.
	var pipelineRuns, dispatches, transitions int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_runs WHERE backlog_id = ?`, item.ID).Scan(&pipelineRuns); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_dispatches WHERE backlog_id = ?`, item.ID).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_transitions WHERE backlog_id = ? AND kind = ?`,
		item.ID, WorkflowStartKind).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if pipelineRuns != 0 || dispatches != 0 || transitions != 1 {
		t.Fatalf("lane artifacts: pipeline_runs=%d dispatches=%d workflow_start_transitions=%d, want 0/0/1",
			pipelineRuns, dispatches, transitions)
	}
}

func TestClaimWorkflowStart_ConcurrentExactlyOne(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-WF-RACE")

	const racers = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := st.ClaimWorkflowStart(ctx, workflowClaimRequest(item.ID))
			outcomes <- err
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	successes, conflicts := 0, 0
	for err := range outcomes {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrClaimConflict):
			conflicts++
		default:
			t.Fatalf("unexpected racer error: %v", err)
		}
	}
	if successes != 1 || conflicts != racers-1 {
		t.Fatalf("race outcomes: successes=%d conflicts=%d want 1/%d", successes, conflicts, racers-1)
	}
}

func TestClaimWorkflowStart_ValidationFailsClosed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-WF-VALID")

	for name, mutate := range map[string]func(*ClaimWorkflowStartRequest){
		"dag engine":       func(r *ClaimWorkflowStartRequest) { r.Selection.Engine = WorkflowEngineDAG },
		"empty template":   func(r *ClaimWorkflowStartRequest) { r.Selection.Template = "" },
		"empty version":    func(r *ClaimWorkflowStartRequest) { r.Selection.TemplateVersion = "" },
		"empty interp":     func(r *ClaimWorkflowStartRequest) { r.Selection.InterpreterVersion = "" },
		"empty params":     func(r *ClaimWorkflowStartRequest) { r.Selection.ParamsJSON = "  " },
		"negative dollars": func(r *ClaimWorkflowStartRequest) { r.EstimateUSD = -1 },
	} {
		req := workflowClaimRequest(item.ID)
		mutate(&req)
		if _, err := st.ClaimWorkflowStart(ctx, req); !errors.Is(err, ErrInvalidClaim) {
			t.Fatalf("%s: error = %v, want ErrInvalidClaim", name, err)
		}
	}
	// The rejected attempts must not have consumed the claim.
	fresh, err := st.Backlog.Get(ctx, item.ID)
	if err != nil || fresh.State != BacklogQueued {
		t.Fatalf("item after rejected claims: %+v err=%v, want still queued", fresh, err)
	}
}

// TestClaimWorkflowStart_SharedBudgetCrossLane: a reservation held by one lane
// is visible to the other lane's admission, in both directions — the two
// kernels cannot jointly oversubscribe the shared daily cap.
func TestClaimWorkflowStart_SharedBudgetCrossLane(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Workflow reservation first: 6 of a 10-dollar day.
	wfItem := seedClaimBacklog(t, st, "MILLS-WF-BUDGET-A")
	wfReq := workflowClaimRequest(wfItem.ID)
	wfReq.EstimateUSD = 6
	wfReq.Limits.MaxUSDPerDay = 10
	if _, err := st.ClaimWorkflowStart(ctx, wfReq); err != nil {
		t.Fatalf("workflow claim: %v", err)
	}

	// A pipeline claim asking for 5 more must now exceed the cap.
	pipeItem := seedClaimBacklog(t, st, "MILLS-WF-BUDGET-B")
	pipeReq := claimTestRequest(pipeItem.ID)
	pipeReq.EstimateUSD = 5
	pipeReq.Limits.MaxUSDPerDay = 10
	var exceeded *BudgetExceededError
	if _, err := st.ClaimPipelineStart(ctx, pipeReq); !errors.As(err, &exceeded) {
		t.Fatalf("pipeline claim after workflow reservation: err = %v, want BudgetExceededError", err)
	}

	// And the reverse: a pipeline reservation blocks a workflow claim.
	st2 := newTestStore(t)
	pipeItem2 := seedClaimBacklog(t, st2, "MILLS-WF-BUDGET-C")
	pipeReq2 := claimTestRequest(pipeItem2.ID)
	pipeReq2.EstimateUSD = 6
	pipeReq2.Limits.MaxUSDPerDay = 10
	if _, err := st2.ClaimPipelineStart(ctx, pipeReq2); err != nil {
		t.Fatalf("pipeline claim: %v", err)
	}
	wfItem2 := seedClaimBacklog(t, st2, "MILLS-WF-BUDGET-D")
	wfReq2 := workflowClaimRequest(wfItem2.ID)
	wfReq2.EstimateUSD = 5
	wfReq2.Limits.MaxUSDPerDay = 10
	if _, err := st2.ClaimWorkflowStart(ctx, wfReq2); !errors.As(err, &exceeded) {
		t.Fatalf("workflow claim after pipeline reservation: err = %v, want BudgetExceededError", err)
	}
}

// TestWorkflowTerminalSettle: terminalizing a claim-started run in the
// lifecycle CAS releases the reservation and escalates the item — atomically,
// idempotently, and only for claim-started runs.
func TestWorkflowTerminalSettle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-WF-SETTLE")

	res, err := st.ClaimWorkflowStart(ctx, workflowClaimRequest(item.ID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	run := res.Run

	now := time.Now().UTC()
	run.State = WorkflowRunDone
	run.EndedAt = &now
	won, err := st.Workflow.CompareAndSetWorkflowRunLifecycle(ctx, run, WorkflowRunRunning)
	if err != nil || !won {
		t.Fatalf("terminal CAS: won=%t err=%v", won, err)
	}

	var reservationState string
	if err := st.db.QueryRowContext(ctx,
		`SELECT state FROM pipeline_budget_reservations WHERE run_id = ?`, run.ID).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if reservationState != "released" {
		t.Fatalf("reservation state = %q, want released", reservationState)
	}
	freshItem, err := st.Backlog.Get(ctx, item.ID)
	if err != nil || freshItem.State != BacklogEscalated {
		t.Fatalf("item after settle: %+v err=%v, want escalated", freshItem, err)
	}
	var settleTransitions int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_transitions WHERE backlog_id = ? AND kind = ?`,
		item.ID, WorkflowTerminalKind).Scan(&settleTransitions); err != nil {
		t.Fatal(err)
	}
	if settleTransitions != 1 {
		t.Fatalf("settle transitions = %d, want 1", settleTransitions)
	}
	// The escalation context event points the reviewer at the work product.
	var payload string
	if err := st.db.QueryRowContext(ctx,
		`SELECT payload_json FROM events WHERE kind = 'workflow.terminal_settle' AND subject_id = ?`,
		item.ID).Scan(&payload); err != nil {
		t.Fatalf("settle event missing: %v", err)
	}
	for _, want := range []string{run.ID, WorkflowRunBranchPrefix + run.ID, "review branch"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("settle event payload missing %q: %s", want, payload)
		}
	}

	// Idempotence: a second terminalization attempt loses the CAS and must
	// not double-settle.
	run.State = WorkflowRunError
	won, err = st.Workflow.CompareAndSetWorkflowRunLifecycle(ctx, run, WorkflowRunRunning)
	if err != nil || won {
		t.Fatalf("second terminal CAS: won=%t err=%v, want lost cleanly", won, err)
	}
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_transitions WHERE backlog_id = ? AND kind = ?`,
		item.ID, WorkflowTerminalKind).Scan(&settleTransitions); err != nil {
		t.Fatal(err)
	}
	if settleTransitions != 1 {
		t.Fatalf("settle transitions after replay = %d, want 1", settleTransitions)
	}
}

// TestWorkflowTerminalSettle_CanaryWithoutReservationIsInert: a run that was
// NOT claim-started (no reservation — the canary/admin path) can carry a
// backlog id of a running item without releasing or escalating it. The
// reservation is the claim-provenance discriminator.
func TestWorkflowTerminalSettle_CanaryWithoutReservationIsInert(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// The item is running under a legitimate PIPELINE claim.
	item := seedClaimBacklog(t, st, "MILLS-WF-CANARY-GUARD")
	if _, err := st.ClaimPipelineStart(ctx, claimTestRequest(item.ID)); err != nil {
		t.Fatalf("pipeline claim: %v", err)
	}

	// A canary-style imperative run references the same item WITHOUT a claim.
	now := time.Now().UTC()
	canary := &WorkflowRun{
		ID: "wf-canary-guard", BacklogID: item.ID,
		Engine: WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: "v2", InterpreterVersion: "starlark-go@test",
		WorkflowParams: `{"agent_type":"claude-code"}`,
		State:          WorkflowRunRunning, StartedAt: &now,
	}
	if err := st.Workflow.CreateWorkflowRun(ctx, canary); err != nil {
		t.Fatalf("create canary: %v", err)
	}
	canary.State = WorkflowRunDone
	canary.EndedAt = &now
	won, err := st.Workflow.CompareAndSetWorkflowRunLifecycle(ctx, canary, WorkflowRunRunning)
	if err != nil || !won {
		t.Fatalf("canary terminal CAS: won=%t err=%v", won, err)
	}

	// The pipeline-claimed item must be untouched, its reservation active.
	freshItem, err := st.Backlog.Get(ctx, item.ID)
	if err != nil || freshItem.State != BacklogRunning {
		t.Fatalf("item after canary settle: %+v err=%v, want still running", freshItem, err)
	}
	var reservationState string
	if err := st.db.QueryRowContext(ctx,
		`SELECT state FROM pipeline_budget_reservations WHERE backlog_id = ?`, item.ID).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if reservationState != "active" {
		t.Fatalf("pipeline reservation = %q, want still active", reservationState)
	}
}

// TestWorkflowPauseDoesNotSettle: paused is resumable durable work — the
// lifecycle CAS must not release the reservation or move the item.
func TestWorkflowPauseDoesNotSettle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := seedClaimBacklog(t, st, "MILLS-WF-PAUSE")

	res, err := st.ClaimWorkflowStart(ctx, workflowClaimRequest(item.ID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	run := res.Run
	now := time.Now().UTC()
	run.State = WorkflowRunPaused
	run.PausedAt = &now
	won, err := st.Workflow.CompareAndSetWorkflowRunLifecycle(ctx, run, WorkflowRunRunning)
	if err != nil || !won {
		t.Fatalf("pause CAS: won=%t err=%v", won, err)
	}
	var reservationState string
	if err := st.db.QueryRowContext(ctx,
		`SELECT state FROM pipeline_budget_reservations WHERE run_id = ?`, run.ID).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	freshItem, _ := st.Backlog.Get(ctx, item.ID)
	if reservationState != "active" || freshItem.State != BacklogRunning {
		t.Fatalf("pause settled: reservation=%q item=%q, want active/running", reservationState, freshItem.State)
	}
}
