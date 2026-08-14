package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// escalationsDefaultSince is the look-back window for the relaunch-candidates
// list when the caller omits ?since. Mirrors the audit findings default:
// recent state stays useful without forcing every HUD poll to walk the full
// table.
const escalationsDefaultSince = 7 * 24 * time.Hour

// escalationsDefaultLimit / escalationsMaxLimit bound the relaunch-candidates
// list response. Values above the max are capped, not rejected.
const (
	escalationsDefaultLimit = 50
	escalationsMaxLimit     = 200
)

// handleEscalationRelaunchCandidates lists escalated backlog items whose
// LATEST pipeline run recorded EscalationRetryable=true — the escalations a
// human can requeue without a policy override. Read-only; never gates behind
// the admin token (HUD polls it).
//
// Query params:
//   - since (optional, RFC3339): window on the latest run's EndedAt.
//     Default = now-7d.
//   - limit (optional): default 50, max 200 (capped).
//
// The response is a bare JSON array of store.RelaunchCandidate rows — no
// envelope, PascalCase fields {ID, Title, EscalationClass, FailureClass,
// EndedAt} exactly as the store row serializes. Empty is [] never null.
func (o *operator) handleEscalationRelaunchCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	since := time.Now().Add(-escalationsDefaultSince)
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		since = t
	}

	limit := escalationsDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
		if limit > escalationsMaxLimit {
			limit = escalationsMaxLimit
		}
	}

	candidates, err := o.store.Backlog.ListByEndedSince(r.Context(), since, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Encode an empty candidate set as `[]`, never `null` — a bare null forces
	// every client to special-case it (it crashed the HUD drawer once already).
	if candidates == nil {
		candidates = []*store.RelaunchCandidate{}
	}
	writeJSON(w, http.StatusOK, candidates)
}
