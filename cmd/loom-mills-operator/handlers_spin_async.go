package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	// spinAsyncBudget caps one async spin's wall-clock. Matches the synchronous
	// /spin request budget so behaviour is identical bar the connection model.
	spinAsyncBudget = 10 * time.Minute
	// spinMaxInFlight bounds pending+running spins so a burst of POSTs can't
	// queue unbounded behind the concurrency semaphore. Beyond this the POST
	// returns 429 rather than accepting work it can't get to.
	spinMaxInFlight = 32
)

// handleSpinAsync accepts a spin, persists a pending status row, launches the
// spin in a detached operator-lifetime goroutine, and returns 202 + the spin
// id immediately. This is the durable path for slow frontier frames: the
// client never holds a connection open past the proxy timeout — it polls
// GET /api/mills/spin/runs/{id} instead (plan .loom/166).
func (o *operator) handleSpinAsync(w http.ResponseWriter, r *http.Request) {
	if o.spinner == nil {
		http.Error(w, "spinning room not configured on this operator instance (needs the MCP hub)",
			http.StatusServiceUnavailable)
		return
	}
	// Synchronous fail-fast for a disabled room, using the SAME enablement source
	// the Spinner's gate uses (the injected func → policy.spinning_room.enabled),
	// so an async POST reports 503 up front instead of a delayed failed row.
	if o.spinner.Enabled == nil || !o.spinner.Enabled() {
		http.Error(w, "spinning room disabled in policy (spinning_room.enabled)",
			http.StatusServiceUnavailable)
		return
	}

	var req spinRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if strings.TrimSpace(req.Brief) == "" {
		http.Error(w, "brief is required", http.StatusBadRequest)
		return
	}
	framesList := mergeRequestFrames(req)
	if len(framesList) == 0 {
		http.Error(w, "a frame or frames[] is required", http.StatusBadRequest)
		return
	}

	// Queue bound: refuse rather than pile up unbounded work behind the
	// concurrency semaphore. Counted from the durable store so it survives a
	// restart mid-burst.
	active, err := o.store.Spin.CountActive(r.Context())
	if err != nil {
		http.Error(w, "spin queue check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if active >= spinMaxInFlight {
		http.Error(w, fmt.Sprintf("too many spins in flight (%d); retry shortly", active),
			http.StatusTooManyRequests)
		return
	}

	run := &store.SpinRun{
		ID:          newSpinID(),
		Brief:       strings.TrimSpace(req.Brief),
		Frames:      framesList,
		Priority:    strings.TrimSpace(req.Priority),
		Project:     strings.TrimSpace(req.Project),
		Namespace:   strings.TrimSpace(req.Namespace),
		Status:      store.SpinPending,
		PlanIDs:     []string{},
		Competitive: len(req.Frames) > 0,
		StartedAt:   time.Now().UTC(),
	}
	if err := o.store.Spin.Put(r.Context(), run); err != nil {
		http.Error(w, "record spin failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Capture the id BEFORE handing run to the goroutine: the goroutine takes
	// exclusive ownership of *run (it mutates Status/PlanIDs/EndedAt), so the
	// handler must not read run again after the `go` below.
	spinID := run.ID

	sreq := spin.Request{
		Brief:      req.Brief,
		Frame:      req.Frame,
		Frames:     req.Frames,
		Priority:   req.Priority,
		Project:    req.Project,
		Namespace:  req.Namespace,
		RespunFrom: req.RespunFrom,
	}
	go o.runSpinAsync(run, sreq)
	mills.AsyncSpinsTotal.WithLabelValues("accepted").Inc()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"spin_id":    spinID,
		"status":     string(store.SpinPending),
		"status_url": "/api/mills/spin/runs/" + spinID,
	})
}

// runSpinAsync runs one accepted spin to completion in the background and
// records the terminal status. Rooted at the operator lifetime context (not the
// request), so a client disconnect can't cancel an in-flight spin; operator
// shutdown can (and the next startup sweep reconciles anything left running).
func (o *operator) runSpinAsync(run *store.SpinRun, sreq spin.Request) {
	budget := o.asyncBudget()
	ctx, cancel := context.WithTimeout(o.spinBaseContext(), budget)
	defer cancel()

	// Acquire a concurrency slot. While we wait the row stays pending; if the
	// operator is shutting down, abort — the startup sweep will fail the row.
	select {
	case o.spinSem <- struct{}{}:
		defer func() { <-o.spinSem }()
	case <-ctx.Done():
		o.finishSpin(run, nil, "", ctx.Err())
		return
	}

	run.Status = store.SpinRunning
	if err := o.store.Spin.Put(ctx, run); err != nil {
		// Non-fatal: the terminal Put below is the write that matters. Log so a
		// stuck 'pending'→missing 'running' transition is diagnosable.
		o.logger.Error("async spin: mark running failed", "spin_id", run.ID, "err", err)
	}

	// Run the spin in a monitored goroutine and enforce the budget with a
	// watchdog. A dependency that ignores context cancellation can otherwise
	// hold the goroutine — and the 'running' row — indefinitely, past the
	// budget, with no terminal status: the live failure mode is the MCP-hub Recv
	// (libs/mcp-go WebSocketTransport.Recv), which blocks on a raw websocket
	// ReadMessage and never consults ctx, so when the hub goes silent the 45s
	// author timeout AND this budget are both cosmetic and the spin wedges until
	// the operator restarts orphans it. The select below GUARANTEES a terminal
	// outcome at the budget even when the worker is still blocked — the row can
	// never wedge in 'running', and the HUD always shows why (timeout). The
	// orphaned worker settles when the hub recovers or the operator restarts; the
	// semaphore slot is released by the defer above.
	type spinOutcome struct {
		planIDs []string
		note    string
		err     error
	}
	done := make(chan spinOutcome, 1) // buffered: the worker never blocks on send
	o.spinWorkers.Add(1)
	go func() {
		defer o.spinWorkers.Add(-1)
		var out spinOutcome
		if !run.Competitive {
			res, err := o.spinner.Spin(ctx, sreq)
			if err != nil {
				out.err = err
			} else {
				out.planIDs = []string{res.PlanID}
			}
		} else {
			cr, err := o.spinner.SpinAll(ctx, sreq)
			if err != nil {
				out.err = err
			} else {
				for _, res := range cr.Results {
					out.planIDs = append(out.planIDs, res.PlanID)
				}
				if len(cr.Failures) > 0 {
					out.note = summarizeFrameFailures(cr.Failures)
				}
			}
		}
		done <- out
	}()

	select {
	case out := <-done:
		o.finishSpin(run, out.planIDs, out.note, out.err)
	case <-ctx.Done():
		// Budget elapsed but the worker is still blocked (a dependency ignored
		// cancellation). Record the terminal status NOW so the row never stays
		// 'running' and the HUD shows a clear reason. On a deadline that means
		// SpinTimeout with an explanatory message; on shutdown cancellation the
		// bare ctx error maps to SpinFailed and the next startup sweep reconciles.
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			o.logger.Warn("async spin watchdog fired; recording timeout while worker still blocked",
				"spin_id", run.ID, "budget", budget, "frames", strings.Join(run.Frames, ","))
			err = fmt.Errorf("spin exceeded %s budget: a dependency (likely the MCP hub plan-store write) stopped responding and ignored cancellation: %w", budget, err)
		}
		o.finishSpin(run, nil, "", err)
	}
}

// asyncBudget is the wall-clock cap on one async spin. Overridable via
// o.spinAsyncBudget (tests set a short budget to exercise the watchdog);
// defaults to the spinAsyncBudget const.
func (o *operator) asyncBudget() time.Duration {
	if o.spinAsyncBudget > 0 {
		return o.spinAsyncBudget
	}
	return spinAsyncBudget
}

// finishSpin records the terminal status of an async spin. note carries a
// partial-failure summary on an otherwise-successful competitive spin.
func (o *operator) finishSpin(run *store.SpinRun, planIDs []string, note string, err error) {
	now := time.Now().UTC()
	run.EndedAt = &now
	if planIDs == nil {
		planIDs = []string{}
	}
	run.PlanIDs = planIDs
	switch {
	case err == nil:
		run.Status = store.SpinSucceeded
		run.Error = note // "" for a clean spin; failure summary for a partial competitive spin
	case errors.Is(err, context.DeadlineExceeded):
		run.Status = store.SpinTimeout
		run.Error = err.Error()
	default:
		run.Status = store.SpinFailed
		run.Error = err.Error()
	}
	// run.Status is one of the 3 terminal SpinStatus values here — a bounded
	// label set (see AsyncSpinsTotal). "accepted" is counted at the 202.
	mills.AsyncSpinsTotal.WithLabelValues(string(run.Status)).Inc()

	// Persist on a fresh short-lived context: the spin's own ctx may be the very
	// thing that expired/cancelled, and reusing it would drop the terminal write.
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if putErr := o.store.Spin.Put(pctx, run); putErr != nil {
		o.logger.Error("async spin: persist terminal status failed",
			"spin_id", run.ID, "status", string(run.Status), "err", putErr)
	}
	o.logger.Info("async spin finished",
		"spin_id", run.ID, "status", string(run.Status),
		"plan_ids", strings.Join(planIDs, ","), "frames", strings.Join(run.Frames, ","))
}

// handleSpinRunsList returns the most recent N async spin runs, newest-first.
func (o *operator) handleSpinRunsList(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r.URL.Query().Get("limit"), 50)
	runs, err := o.store.Spin.List(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []*store.SpinRun{} // JSON [] not null
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleSpinRunGet returns one async spin run by id, or 404.
func (o *operator) handleSpinRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	run, err := o.store.Spin.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "spin run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// spinBaseContext returns the operator-lifetime context async spins root at,
// falling back to context.Background() when unset (handler tests).
func (o *operator) spinBaseContext() context.Context {
	if o.spinBaseCtx != nil {
		return o.spinBaseCtx
	}
	return context.Background()
}

// mergeRequestFrames returns the requested frame set for display/tracking: the
// deduped, order-preserving union of req.Frame (first) + req.Frames. Mirrors
// spin.mergeFrameNames (unexported) so the stored Frames match what the Spinner
// will actually spin.
func mergeRequestFrames(req spinRequest) []string {
	seen := map[string]bool{}
	var names []string
	for _, name := range append([]string{req.Frame}, req.Frames...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// summarizeFrameFailures renders a competitive spin's per-frame failures into a
// single note stored on the (partially) successful run.
func summarizeFrameFailures(fs []spin.FrameFailure) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, f.Frame+": "+f.Error)
	}
	return fmt.Sprintf("%d frame(s) failed — %s", len(fs), strings.Join(parts, "; "))
}

// newSpinID mints a sortable, unique async-spin id: spin-<UTC ts>-<random>.
func newSpinID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("spin-%s-%s", time.Now().UTC().Format("20060102-150405"), hex.EncodeToString(b[:]))
}
