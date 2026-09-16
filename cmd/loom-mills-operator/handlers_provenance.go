package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/fleetgate"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// backlogItemSubjectKind is the events-table subject_kind every backlog
// lifecycle writer stamps (reconciler bootstrap escalation, auto-requeue,
// groomer actions, operator overrides, agent routing). Declared here as the
// read-side counterpart to those scattered literals.
const backlogItemSubjectKind = "backlog_item"

// backlogEventsDefaultLimit bounds one item's ledger read. An item's own
// history is O(10) events even for a much-requeued escalation, so this is
// generous; ?limit= narrows it further for callers that only want the tail.
const backlogEventsDefaultLimit = 200

// backlogEventsMaxLimit caps the caller-supplied ?limit so a hand-typed URL
// can't turn one HUD drawer open into an unbounded scan.
const backlogEventsMaxLimit = 500

// handleBacklogItemEvents returns one backlog item's recorded event ledger,
// newest-first. It backs the HUD's provenance/journey strip: the
// queued→running→escalated→picked-up→requeued→merged arc an operator needs to
// reconstruct why an item took the path it did.
//
// Open read, matching GET /api/mills/backlog/{id} and the pipeline run
// endpoints: the payloads carry state names, failure classes, and actor labels
// the operator already sees elsewhere, and diagnosing an item at 2am must not
// require minting an admin token.
//
// IMPORTANT — this is a ledger of RECORDED events, not a synthesized lifecycle.
// Only transitions routed through BacklogDAO.TransitionStateWithEvent (bootstrap
// escalation, auto-requeue, groomer actions) plus the explicit appends
// (operator overrides, agent routing) land here; a plain queued→running claim
// advances ClaimVersion without writing an event. Callers must render this as
// "what was recorded" and interleave pipeline runs for the rest, never as a
// complete state history — implying completeness it can't prove is how a
// debugging aid becomes a lie.
//
// The 404 on an unknown item is deliberate: an empty ledger for a real item and
// a typo'd id are different answers, and collapsing both to `[]` sends the
// operator hunting for a missing writer that was never the problem.
func (o *operator) handleBacklogItemEvents(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if _, err := o.store.Backlog.Get(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "backlog item not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	limit := backlogEventsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, backlogEventsMaxLimit)
	}

	events, err := o.store.Events.ListBySubject(ctx, backlogItemSubjectKind, id, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Empty ledgers encode as `[]`, never `null` — the same wire contract every
	// other list branch in this binary honours (a bare null has crashed the HUD
	// drawer before).
	if events == nil {
		events = []*store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backlog_id": id,
		"events":     events,
		// partial=true is a permanent property of this endpoint, not a
		// transient state: see the completeness note above. The HUD reads it to
		// caption the strip honestly instead of hardcoding the caveat client-side.
		"partial": true,
	})
}

// waiverResponse is one benchmark waiver decorated with the expiry arithmetic
// the HUD would otherwise have to redo (and get subtly wrong — Until is
// INCLUSIVE, so a waiver is still live on its own expiry date).
type waiverResponse struct {
	Benchmark      string  `json:"benchmark"`
	MaxTimePercent float64 `json:"max_time_percent"`
	Until          string  `json:"until"`
	Reason         string  `json:"reason"`
	// DaysRemaining counts whole days from today to Until inclusive: 0 means
	// "expires today, still active", negative means already inert.
	DaysRemaining int  `json:"days_remaining"`
	Expired       bool `json:"expired"`
}

// waiversResponse is the fleet-gate waiver card's payload.
type waiversResponse struct {
	Waivers []waiverResponse `json:"waivers"`
	// GlobalTimePercent is the threshold each waiver raises. Without it the
	// card shows "cap 60%" with nothing to compare against, and the operator
	// can't tell a generous waiver from a rounding-error one.
	GlobalTimePercent float64 `json:"global_time_percent"`
	// SuiteVersion + ManifestModified let the card state how fresh its source
	// is. The operator's checkout is hard-aligned to origin/main on every boot
	// but DEGRADES to the existing (possibly stale) clone when origin is
	// unreachable, so the card must be able to show its own age rather than
	// implying live truth.
	SuiteVersion     int    `json:"suite_version"`
	ManifestModified string `json:"manifest_modified,omitempty"`
}

// fleetGateManifestRelPath is the manifest's location inside the loom-core
// checkout the operator maintains at cfg.RepoRoot.
const fleetGateManifestRelPath = "scripts/ci/fleet_reliability_suite_v1.json"

// handleFleetGateWaivers serves the active benchmark waivers from the fleet
// reliability manifest in the operator's repo checkout.
//
// Waivers are self-expiring exceptions that raise one benchmark's
// time-regression cap, and until now they were legible only to CI. That makes
// the expiry a silent cliff: past Until the gate snaps back to the global
// threshold on its own and the next benchmark run fails with no warning anyone
// was shown. Surfacing them with days-remaining is the whole point of the
// endpoint — the list alone is much less useful than the countdown.
//
// Read-only and fail-soft: a missing RepoRoot, absent file, or unparseable
// manifest returns 503 with a reason rather than 500, because this is a nice-to-
// have card on a panel whose other content must keep rendering.
func (o *operator) handleFleetGateWaivers(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(o.repoRoot)
	if root == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "repo root not configured; fleet-gate manifest unavailable",
		})
		return
	}
	path := filepath.Join(root, fleetGateManifestRelPath)
	manifest, err := fleetgate.LoadManifest(path)
	if err != nil {
		o.logger.Warn("fleet-gate waiver read failed", "path", path, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "fleet-gate manifest unavailable: " + err.Error(),
		})
		return
	}

	var modified string
	if info, statErr := os.Stat(path); statErr == nil {
		modified = info.ModTime().UTC().Format(time.RFC3339)
	}

	// Compare dates, not instants: Until is a bare YYYY-MM-DD and the gate
	// treats it as inclusive, so truncating today to midnight UTC is what makes
	// "expires today" read as 0 days rather than -1.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]waiverResponse, 0, len(manifest.Waivers))
	for _, wv := range manifest.Waivers {
		entry := waiverResponse{
			Benchmark:      wv.Benchmark,
			MaxTimePercent: wv.MaxTimePercent,
			Until:          wv.Until,
			Reason:         wv.Reason,
		}
		if until, perr := time.Parse("2006-01-02", strings.TrimSpace(wv.Until)); perr == nil {
			days := int(until.UTC().Truncate(24*time.Hour).Sub(today).Hours() / 24)
			entry.DaysRemaining = days
			entry.Expired = days < 0
		}
		out = append(out, entry)
	}

	writeJSON(w, http.StatusOK, waiversResponse{
		Waivers:           out,
		GlobalTimePercent: manifest.Thresholds.TimePercent,
		SuiteVersion:      manifest.SuiteVersion,
		ManifestModified:  modified,
	})
}
