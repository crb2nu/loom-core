package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	// councilOrphanNote marks a run whose worker died with the previous
	// process. The operator is a singleton, so any row still 'running' at boot
	// is definitionally orphaned — the same argument the async-spin sweep makes.
	councilOrphanNote = "orphaned: operator restarted before the council run finished"
	// councilShutdownNote marks an admitted run that never got a worker slot
	// because the operator was already shutting down.
	councilShutdownNote = "shutdown before execute"
	// councilOrphanSweepLimit bounds the startup scan. Far above the daily
	// council run cap, so a full sweep is a single small query.
	councilOrphanSweepLimit = 200
	// councilTerminalizeTimeout bounds the detached terminal write. Matches the
	// runner's own cleanup budget.
	councilTerminalizeTimeout = 10 * time.Second
)

// councilAsyncRequest is the /council/async body: councilRunRequest plus an
// explicit dryrun field so an async dryrun gets a legible 400 instead of an
// "unknown field" parse error. A dryrun writes no council_runs row (it stays
// deliberately nonpersistent), so there would be nothing to poll — dryrun keeps
// using the synchronous endpoint.
type councilAsyncRequest struct {
	Trigger string `json:"trigger,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Dryrun  bool   `json:"dryrun,omitempty"`
}

// handleCouncilAsync admits a council run synchronously, then executes it in a
// detached operator-lifetime goroutine and returns 202 + the run id
// immediately (#334).
//
// The 202 is a fact, not a promise: Admit has already committed the
// council_runs row and its budget reservation, so GET
// /api/mills/council/runs/{id} resolves before the response is written. That
// is what makes this path safe behind an edge that cuts idle connections at
// ~100s while a council pass takes minutes — the client never holds the
// connection open, and a disconnect can no longer cancel the run.
func (o *operator) handleCouncilAsync(w http.ResponseWriter, r *http.Request) {
	if o.runner == nil {
		http.Error(w, "council runner not configured on this operator instance",
			http.StatusServiceUnavailable)
		return
	}

	// Body is optional; empty body parses to a zero councilAsyncRequest.
	var req councilAsyncRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Dryrun {
		http.Error(w, "dryrun is not supported on the async endpoint: a dryrun persists no council run, so there is nothing to poll; POST /api/mills/council/dryrun instead",
			http.StatusBadRequest)
		return
	}

	// Admission is one budget read plus one SQLite transaction — fast enough to
	// hold the request open for, and the only part that must happen before the
	// 202 so the returned id is already durable.
	adm, err := o.runner.Admit(r.Context(), runner.RunInput{
		Trigger: store.CouncilTrigger(strings.TrimSpace(req.Trigger)),
		Reason:  strings.TrimSpace(req.Reason),
	})
	if err != nil {
		var exceeded *store.CouncilBudgetExceededError
		if errors.As(err, &exceeded) {
			http.Error(w, "council budget denied: "+strings.Join(exceeded.Reasons, "; "),
				http.StatusTooManyRequests)
			return
		}
		if errors.Is(err, runner.ErrBudgetDenied) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		http.Error(w, "council admission failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	go o.runCouncilAsync(adm)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id":     adm.RunID,
		"status":     string(store.CouncilOutcomeRunning),
		"status_url": "/api/mills/council/runs/" + adm.RunID,
	})
}

// runCouncilAsync executes one admitted council run in the background. It is
// rooted at the operator lifetime (never the request), so a client disconnect
// cannot cancel it; operator shutdown can, and the next startup sweep
// reconciles anything left running.
//
// There is deliberately no watchdog select here (unlike runSpinAsync): spins
// need one because the MCP-hub websocket Recv ignores context cancellation,
// while every council participant is an HTTP client that honours it. The
// runner's per-stage deadlines plus its deferred finalizer — which terminalizes
// on a fresh context on every exit path — already guarantee a terminal outcome.
func (o *operator) runCouncilAsync(adm *runner.Admission) {
	ctx, cancel := context.WithCancel(o.councilBaseContext())
	defer cancel()

	// Acquire a worker slot. While we wait the row stays 'running'; if the
	// operator is shutting down, terminalize now rather than leaving the
	// reservation held until the next boot's sweep.
	select {
	case o.councilSem <- struct{}{}:
		defer func() { <-o.councilSem }()
	case <-ctx.Done():
		o.terminalizeCouncilRun(adm.Run, councilShutdownNote+": "+ctx.Err().Error())
		return
	}

	if _, err := o.runner.Execute(ctx, adm); err != nil {
		o.logger.Error("async council run failed", "run_id", adm.RunID, "err", err)
		return
	}
	o.logger.Info("async council run finished", "run_id", adm.RunID)
}

// councilBaseContext returns the operator-lifetime context async council runs
// root at, falling back to context.Background() when unset (handler tests).
func (o *operator) councilBaseContext() context.Context {
	if o.councilBaseCtx != nil {
		return o.councilBaseCtx
	}
	return context.Background()
}

// sweepOrphanedCouncilRuns terminalizes council runs left 'running' by a
// previous process and releases their budget reservations. Leaving a
// reservation active is not cosmetic: it counts against the daily cap in every
// later admission snapshot until the 6-hour lease expires.
//
// Per-row failures are logged and skipped (a legacy row with no active
// reservation cannot be finalized); only the initial listing error is returned.
func (o *operator) sweepOrphanedCouncilRuns(ctx context.Context) (int, error) {
	runs, err := o.store.Council.List(ctx, councilOrphanSweepLimit)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, run := range runs {
		if run == nil || run.Outcome != store.CouncilOutcomeRunning || run.EndedAt != nil {
			continue
		}
		if o.terminalizeCouncilRun(run, councilOrphanNote) {
			swept++
		}
	}
	return swept, nil
}

// terminalizeCouncilRun writes a terminal error outcome for a still-provisional
// council run and releases its reservation, on a fresh short-lived context: the
// run's own context may be the very thing that died. Reports whether the write
// landed.
func (o *operator) terminalizeCouncilRun(run *store.CouncilRun, note string) bool {
	if run == nil || run.ID == "" {
		return false
	}
	ended := time.Now().UTC()
	run.EndedAt = &ended
	run.Outcome = store.CouncilOutcomeError
	run.Notes = appendCouncilRunNote(run.Notes, note)

	ctx, cancel := context.WithTimeout(context.Background(), councilTerminalizeTimeout)
	defer cancel()
	if err := o.store.FinalizeCouncilRun(ctx, run); err != nil {
		o.logger.Warn("council run terminalize failed", "run_id", run.ID, "note", note, "err", err)
		return false
	}
	o.logger.Info("council run terminalized", "run_id", run.ID, "note", note)
	return true
}

// appendCouncilRunNote joins notes the way the runner and the store do, so a
// swept row reads the same as one the runner finalized.
func appendCouncilRunNote(existing, note string) string {
	existing = strings.TrimSpace(existing)
	note = strings.TrimSpace(note)
	switch {
	case existing == "":
		return note
	case note == "" || note == existing:
		return existing
	default:
		return existing + "; " + note
	}
}
