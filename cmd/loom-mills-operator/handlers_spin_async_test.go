package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// blockingSpinAuthor blocks AuthorDraftPlan until release is closed, signalling
// on called when it is first entered. Lets a test observe the running state of
// an async spin before letting it complete.
type blockingSpinAuthor struct {
	release chan struct{}
	called  chan struct{}
	once    sync.Once
	planID  string
}

func (a *blockingSpinAuthor) AuthorDraftPlan(ctx context.Context, _ spin.DraftPlanInput) (string, error) {
	a.once.Do(func() { close(a.called) })
	select {
	case <-a.release:
		return a.planID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// wedgedSpinAuthor blocks AuthorDraftPlan FOREVER, deliberately ignoring ctx —
// it models the live failure: the MCP-hub Recv blocks on a raw websocket read
// (libs/mcp-go WebSocketTransport.Recv) and never consults the context, so
// neither the author timeout nor the async budget can interrupt it. Used to
// prove the operator watchdog records a terminal timeout even when the worker
// goroutine can't be cancelled.
type wedgedSpinAuthor struct {
	called chan struct{}
	once   sync.Once
}

func (a *wedgedSpinAuthor) AuthorDraftPlan(context.Context, spin.DraftPlanInput) (string, error) {
	a.once.Do(func() { close(a.called) })
	select {} // block forever, ignoring ctx — the wedge under test
}

// eventuallySpinStatus polls the store until the spin reaches want or times out.
func eventuallySpinStatus(t *testing.T, op *operator, id string, want store.SpinStatus) *store.SpinRun {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, err := op.store.Spin.Get(context.Background(), id)
		if err == nil && run.Status == want {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("spin %s never reached status %q", id, want)
	return nil
}

func postSpinAsync(op *operator, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin/async", strings.NewReader(body))
	op.handleSpinAsync(rec, r)
	return rec
}

func TestHandleSpinAsync_AcceptedThenSucceeds(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	author := &blockingSpinAuthor{
		release: make(chan struct{}),
		called:  make(chan struct{}),
		planID:  "plan-async-1",
	}
	op.withSpinner(spinnerWithFakes(author))

	rec := postSpinAsync(op, `{"brief":"Ship a thing","frame":"opus","priority":"P0"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	var resp struct {
		SpinID    string `json:"spin_id"`
		Status    string `json:"status"`
		StatusURL string `json:"status_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202: %v; body=%s", err, rec.Body.String())
	}
	if resp.SpinID == "" || resp.Status != string(store.SpinPending) {
		t.Fatalf("202 body = %+v", resp)
	}
	if resp.StatusURL != "/api/mills/spin/runs/"+resp.SpinID {
		t.Errorf("status_url = %q", resp.StatusURL)
	}

	// The spin is running while the author is blocked.
	select {
	case <-author.called:
	case <-time.After(2 * time.Second):
		t.Fatal("author was never called — goroutine did not start the spin")
	}
	eventuallySpinStatus(t, op, resp.SpinID, store.SpinRunning)

	// Releasing the author lets the spin finish; the row goes succeeded with the
	// authored plan id.
	close(author.release)
	done := eventuallySpinStatus(t, op, resp.SpinID, store.SpinSucceeded)
	if len(done.PlanIDs) != 1 || done.PlanIDs[0] != "plan-async-1" {
		t.Errorf("plan_ids = %v, want [plan-async-1]", done.PlanIDs)
	}
	if done.EndedAt == nil {
		t.Errorf("ended_at should be set on a finished spin")
	}
	if done.Error != "" {
		t.Errorf("error = %q, want empty on success", done.Error)
	}
}

func TestHandleSpinAsync_CompetitiveSucceeds(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(competitiveSpinnerWithFakes(&competitiveFakeAuthor{}))

	rec := postSpinAsync(op, `{"brief":"Ship a thing","frames":["mule","ring"],"namespace":"mills/spun"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	var resp struct {
		SpinID string `json:"spin_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	run := eventuallySpinStatus(t, op, resp.SpinID, store.SpinSucceeded)
	if !run.Competitive {
		t.Errorf("competitive flag should be set for a frames[] spin")
	}
	if len(run.PlanIDs) != 2 {
		t.Fatalf("plan_ids = %v, want 2 draft plans", run.PlanIDs)
	}
}

// TestHandleSpinAsync_WatchdogTimesOutWhenWorkerIgnoresContext is the
// regression for the wedge the HUD showed: a spin that ran past the budget stuck
// in 'running' with no reason. The author here blocks forever ignoring ctx (the
// mcp-hub Recv failure mode), so without the watchdog runSpinAsync would hang
// and the row would never leave 'running'. The watchdog must record a terminal
// SpinTimeout at the budget with an explanatory error, regardless.
func TestHandleSpinAsync_WatchdogTimesOutWhenWorkerIgnoresContext(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	author := &wedgedSpinAuthor{called: make(chan struct{})}
	op.withSpinner(spinnerWithFakes(author))
	op.spinAsyncBudget = 60 * time.Millisecond // fire the watchdog fast

	rec := postSpinAsync(op, `{"brief":"wedge me","frame":"opus"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	var resp struct {
		SpinID string `json:"spin_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202: %v", err)
	}

	// The worker reached the (wedged) author — the spin genuinely started.
	select {
	case <-author.called:
	case <-time.After(2 * time.Second):
		t.Fatal("author was never called — the spin never started")
	}

	// ...but the watchdog records a terminal timeout without waiting for the
	// (permanently) blocked worker.
	run := eventuallySpinStatus(t, op, resp.SpinID, store.SpinTimeout)
	if run.EndedAt == nil {
		t.Error("ended_at must be set on a watchdog timeout")
	}
	if run.Error == "" || !strings.Contains(run.Error, "budget") {
		t.Errorf("timeout error should explain the budget breach, got %q", run.Error)
	}
	if got := op.spinWorkers.Load(); got != 1 {
		t.Fatalf("detached worker count = %d, want 1 while author remains wedged", got)
	}
	snapshot, err := op.readSafetyQuiescence(context.Background())
	if err != nil {
		t.Fatalf("read safety quiescence: %v", err)
	}
	if snapshot.Quiescent || snapshot.InMemory.SpinWorkers != 1 {
		t.Fatalf("wedged post-watchdog worker reported quiescent: %+v", snapshot)
	}
}

func TestHandleSpinAsync_503WhenNoSpinner(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	// op.spinner is nil by default.
	rec := postSpinAsync(op, `{"brief":"b","frame":"opus"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleSpinAsync_503WhenDisabled(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	sp := spinnerWithFakes(&spinFakeAuthor{planID: "x"})
	sp.Enabled = func() bool { return false }
	op.withSpinner(sp)

	rec := postSpinAsync(op, `{"brief":"b","frame":"opus"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for disabled room", rec.Code)
	}
}

func TestHandleSpinAsync_400OnMissingBriefOrFrame(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(spinnerWithFakes(&spinFakeAuthor{planID: "x"}))

	if rec := postSpinAsync(op, `{"frame":"opus"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing brief: status = %d, want 400", rec.Code)
	}
	if rec := postSpinAsync(op, `{"brief":"b"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing frame: status = %d, want 400", rec.Code)
	}
}

func TestHandleSpinRunGet_404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/mills/spin/runs/spin-nope", nil)
	r.SetPathValue("id", "spin-nope")
	op.handleSpinRunGet(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown spin id", rec.Code)
	}
}

func TestHandleSpinRunsList_EmptyIsJSONArray(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/mills/spin/runs", nil)
	op.handleSpinRunsList(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty list body = %q, want []", got)
	}
}
