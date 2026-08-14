package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills/spin"
)

// spinningRoomAgentID is the agent-context creator recorded on spun draft
// plans (attribution only; the store never scopes reads by agent_id).
const spinningRoomAgentID = "mills:spinning-room"

// spinRequestBudget bounds one spin end-to-end (model synthesis + plan write).
// spinWriteDeadline is the connection's write deadline for the spin route — a
// hair over the request budget so the budget's timeout error can still be
// written back to the client before the connection deadline trips.
const (
	spinRequestBudget = 10 * time.Minute
	spinWriteDeadline = spinRequestBudget + 30*time.Second
)

// spinRequest is the admin POST body for POST /api/mills/spin (Live Beam slice
// 3 / F2). brief is the roving; frame selects one of the policy-allowed models;
// priority/project/namespace scope + steer the resulting draft plan. Only brief
// and one frame are required.
//
// frames (competitive spinning, the deferred F2 item) spins the same brief on
// every listed frame concurrently — one draft plan per frame, each recording
// its competitors — and switches the response to spin.CompetitiveResult.
// Requests without frames keep the original single-Result response shape, so
// pre-competitive clients are unaffected.
type spinRequest struct {
	Brief     string   `json:"brief"`
	Frame     string   `json:"frame"`
	Frames    []string `json:"frames,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Project   string   `json:"project,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	// RespunFrom, when set, links the resulting draft to the source plan_id it
	// redoes (a HUD "respin"). Recorded on the draft for the supersede flow.
	RespunFrom string `json:"respun_from,omitempty"`
}

// handleSpinningRoomFrames returns the allowed spinning frames + room state so
// the HUD can populate the frame selector. Open read (policy), mirroring
// GET /api/mills/policy — it never 503s so a poll always renders the room's
// availability rather than an error.
func (o *operator) handleSpinningRoomFrames(w http.ResponseWriter, _ *http.Request) {
	pol := o.policy.Current()
	frames := pol.SpinningRoomFrames()
	out := make([]map[string]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, map[string]string{"name": f.Name, "model": f.Model, "backend": f.Backend})
	}
	// This reflects LIVE policy (frames change whenever spinning_room.frames is
	// edited), so it must never be cached — a heuristically-cached response
	// (browser or Cloudflare edge; the endpoint otherwise sends no Cache-Control)
	// would leave the HUD frame picker showing a stale/empty list after a policy
	// edit. The HUD reverse-proxy forwards this header to the browser.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":          pol.SpinningRoomEnabled(),
		"default_priority": pol.SpinningRoomDefaultPriority(),
		"frames":           out,
		// available reflects whether a spin could actually run right now — the
		// room is enabled AND the operator wired a spinner (MCP hub reachable).
		"available": o.spinner != nil && pol.SpinningRoomEnabled(),
	})
}

// handleSpin turns a brief into a draft plan via a policy-chosen frame and
// returns the spun plan summary (plan_id, frame, model, slice_count, cost). The
// draft lands phase=draft so the plan-slice emitter leaves it alone until the
// operator advances it to planned. Returns 503 when the spinner isn't wired,
// and maps the Spinner's typed errors onto status codes.
func (o *operator) handleSpin(w http.ResponseWriter, r *http.Request) {
	// A spin holds the connection during a live model synthesis + a plan-store
	// write over the MCP hub, which routinely exceeds the server's default 30s
	// WriteTimeout (httpServer in server.go). Without extending this
	// connection's write deadline, the handler runs past 30s and the eventual
	// response write fails silently — the client sees the connection close with
	// NO HTTP response (curl reports HTTP 000), which reads as a hang/error even
	// on a successful spin. Extend it to cover the whole spin (just over the
	// per-request context budget below). Best-effort: a wrapped ResponseWriter
	// that doesn't support deadlines just keeps the server default.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(spinWriteDeadline))

	if o.spinner == nil {
		http.Error(w, "spinning room not configured on this operator instance (needs the MCP hub)",
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

	// A spin drives a live model synthesis; cap so a stalled frame can't hold
	// the request open indefinitely. Competitive spins run their frames
	// concurrently, so the cap holds regardless of frame count. The Spinner
	// additionally bounds each phase (editor / plan write) so a single hung
	// dependency fails fast with a clear error well inside this budget.
	ctx, cancel := context.WithTimeout(r.Context(), spinRequestBudget)
	defer cancel()

	sreq := spin.Request{
		Brief:      req.Brief,
		Frame:      req.Frame,
		Frames:     req.Frames,
		Priority:   req.Priority,
		Project:    req.Project,
		Namespace:  req.Namespace,
		RespunFrom: req.RespunFrom,
	}

	// Legacy shape: no frames list → single-frame spin, bare Result response.
	if len(req.Frames) == 0 {
		res, err := o.spinner.Spin(ctx, sreq)
		if err != nil {
			writeSpinError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	cr, err := o.spinner.SpinAll(ctx, sreq)
	if err != nil {
		writeSpinError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cr)
}

// writeSpinError maps the Spinner's typed sentinel errors onto HTTP status
// codes (shared by the single and competitive spin paths).
func writeSpinError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, spin.ErrDisabled):
		http.Error(w, "spinning room disabled in policy (spinning_room.enabled)", http.StatusServiceUnavailable)
	case errors.Is(err, spin.ErrInvalidRequest), errors.Is(err, spin.ErrUnknownFrame):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, spin.ErrNoOutput):
		http.Error(w, "spin failed: "+err.Error(), http.StatusBadGateway)
	case errors.Is(err, spin.ErrEditorTimeout), errors.Is(err, spin.ErrAuthorTimeout):
		http.Error(w, "spin failed: "+err.Error(), http.StatusGatewayTimeout)
	default:
		http.Error(w, "spin failed: "+err.Error(), http.StatusInternalServerError)
	}
}
