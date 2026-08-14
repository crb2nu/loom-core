package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// gatedCouncilEditor blocks inside the editor stage until release is closed,
// signalling on entered when it is first reached. It lets a test cut the client
// connection while the council pass is demonstrably mid-flight — the exact
// shape of the 2026-07-16 incident.
type gatedCouncilEditor struct {
	base    council.Editor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *gatedCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	e.once.Do(func() { close(e.entered) })
	select {
	case <-e.release:
		return e.base.Edit(ctx, brief, reviews)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// councilAsyncOperator wires a test operator with the fake-participant council
// runner (buildCouncilRunner's no-FlexInfer fallback) so the async endpoint has
// something real to admit and execute.
func councilAsyncOperator(t *testing.T) (*operator, func()) {
	t.Helper()
	op, cleanup := newTestOperator(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".loom"), 0o755); err != nil {
		cleanup()
		t.Fatalf("mkdir repo: %v", err)
	}
	// A healthy operator checkout has a ROADMAP.md. The runner's roadmap stage
	// extracts it into the canonical intent store, which is what keeps the
	// fail-closed intent preflight from blocking these runs — and exercises
	// that wiring through buildCouncilRunner's real RepoRoot plumbing.
	const roadmap = "# Roadmap\n\n## Tier 1: Now\n\n- [ ] keep the async council path exercised\n"
	if err := os.WriteFile(filepath.Join(repo, "ROADMAP.md"), []byte(roadmap), 0o644); err != nil {
		cleanup()
		t.Fatalf("write roadmap: %v", err)
	}
	r, _ := buildCouncilRunner(op.store, op.policy, op.budget, repo, nil, nil, nil, "",
		runner.DefaultStageBudgets(), discardLogger())
	if r == nil {
		cleanup()
		t.Fatal("buildCouncilRunner returned nil runner")
	}
	op.withRunner(r)
	return op, cleanup
}

func postCouncilAsync(op *operator, ctx context.Context, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/council/async", strings.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	op.handleCouncilAsync(rec, req)
	return rec
}

// eventuallyCouncilTerminal polls until the run leaves the provisional state.
func eventuallyCouncilTerminal(t *testing.T, op *operator, id string) *store.CouncilRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := op.store.Council.Get(context.Background(), id)
		if err == nil && run.Outcome != store.CouncilOutcomeRunning && run.EndedAt != nil {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("council run %s never reached a terminal outcome", id)
	return nil
}

// Acceptance criterion 6, first test: the client goes away right after the 202
// and the run still reaches a terminal outcome — a successful one, because the
// disconnect never touched the detached execution context.
func TestHandleCouncilAsync_ClientDisconnectDoesNotCancelRun(t *testing.T) {
	op, cleanup := councilAsyncOperator(t)
	defer cleanup()

	gate := &gatedCouncilEditor{
		base:    op.runner.Editor,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	op.runner.Editor = gate

	reqCtx, disconnect := context.WithCancel(context.Background())
	rec := postCouncilAsync(op, reqCtx, `{"trigger":"manual","reason":"disconnect regression"}`)
	if rec.Code != http.StatusAccepted {
		disconnect()
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	var resp struct {
		RunID     string `json:"run_id"`
		Status    string `json:"status"`
		StatusURL string `json:"status_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		disconnect()
		t.Fatalf("decode 202: %v; body=%s", err, rec.Body.String())
	}
	if resp.RunID == "" || resp.Status != string(store.CouncilOutcomeRunning) {
		disconnect()
		t.Fatalf("202 body = %+v", resp)
	}
	if resp.StatusURL != "/api/mills/council/runs/"+resp.RunID {
		t.Errorf("status_url = %q", resp.StatusURL)
	}

	// The 202 is a fact: the row is already durable and the documented poll
	// endpoint resolves it before anything long has run.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/mills/council/runs/"+resp.RunID, nil)
	getReq.SetPathValue("id", resp.RunID)
	op.handleCouncilRunGet(getRec, getReq)
	if getRec.Code != http.StatusOK {
		disconnect()
		t.Fatalf("GET run status = %d (%s), want 200", getRec.Code, getRec.Body.String())
	}

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		disconnect()
		t.Fatal("editor was never reached — the detached goroutine did not start")
	}

	// Cut the client connection mid-run, exactly as the edge does at ~100s.
	disconnect()
	time.Sleep(50 * time.Millisecond)
	close(gate.release)

	done := eventuallyCouncilTerminal(t, op, resp.RunID)
	if done.Outcome == store.CouncilOutcomeError {
		t.Fatalf("run = %+v, want the disconnect NOT to have failed the run (notes=%q)", done.Outcome, done.Notes)
	}
	if strings.Contains(done.Notes, context.Canceled.Error()) {
		t.Fatalf("notes = %q, want no cancellation from the client disconnect", done.Notes)
	}
	assertNoActiveCouncilReservationsInOperator(t, op)
}

func TestHandleCouncilAsync_NoRunnerReturns503(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := postCouncilAsync(op, nil, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503", rec.Code, rec.Body.String())
	}
}

func TestHandleCouncilAsync_DryrunRejected(t *testing.T) {
	op, cleanup := councilAsyncOperator(t)
	defer cleanup()

	rec := postCouncilAsync(op, nil, `{"dryrun":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dryrun") {
		t.Errorf("body = %q, want the dryrun reason", rec.Body.String())
	}
	runs, err := op.store.Council.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("list council runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("rejected dryrun admitted a run: %+v", runs)
	}
}

func TestHandleCouncilAsync_BudgetDeniedReturns429(t *testing.T) {
	op, cleanup := councilAsyncOperator(t)
	defer cleanup()

	// Burn the daily council budget (policy: max_usd_per_day 5) with a
	// terminal prior run so the read-only preflight denies admission.
	ended := time.Now().UTC().Add(-30 * time.Minute)
	if err := op.store.Council.Put(context.Background(), &store.CouncilRun{
		ID:              "COUNCIL-PRIOR-ASYNC-SPEND",
		Trigger:         store.CouncilTriggerCron,
		StartedAt:       time.Now().UTC().Add(-time.Hour),
		EndedAt:         &ended,
		Outcome:         store.CouncilOutcomeError,
		CostFrontierUSD: 5,
	}); err != nil {
		t.Fatalf("seed prior spend: %v", err)
	}

	rec := postCouncilAsync(op, nil, `{"trigger":"manual"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%s), want 429", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget") {
		t.Errorf("body = %q, want the budget reasons", rec.Body.String())
	}
}

func TestSweepOrphanedCouncilRuns_TerminalizesAndReleasesReservation(t *testing.T) {
	op, cleanup := councilAsyncOperator(t)
	defer cleanup()

	// Admit a run and abandon it, exactly as a pod kill mid-council would.
	adm, err := op.runner.Admit(context.Background(), runner.RunInput{
		Trigger: store.CouncilTriggerManual,
		Reason:  "orphan fixture",
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	swept, err := op.sweepOrphanedCouncilRuns(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	got, err := op.store.Council.Get(context.Background(), adm.RunID)
	if err != nil {
		t.Fatalf("get swept run: %v", err)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("swept run = %+v, want terminal error", got)
	}
	if !strings.Contains(got.Notes, "orphaned") {
		t.Errorf("notes = %q, want the orphan marker", got.Notes)
	}
	assertNoActiveCouncilReservationsInOperator(t, op)

	// Idempotent: a second sweep must not touch the now-terminal row.
	again, err := op.sweepOrphanedCouncilRuns(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep swept %d rows, want 0", again)
	}
}

// assertNoActiveCouncilReservationsInOperator fails when any budget reservation
// is still held — a leaked reservation starves the daily cap in every later
// admission snapshot until its 6-hour lease expires.
func assertNoActiveCouncilReservationsInOperator(t *testing.T, op *operator) {
	t.Helper()
	var active int
	if err := op.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM council_budget_reservations WHERE state = 'active'`,
	).Scan(&active); err != nil {
		t.Fatalf("count active council reservations: %v", err)
	}
	if active != 0 {
		t.Fatalf("active council reservations = %d, want 0", active)
	}
}
