package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// writeJSON is the canonical JSON writer for every operator handler.
// Sets the content type, encodes with default options, and silently
// swallows write errors — the caller has already disconnected if
// Encode fails, and noise in the logs from a closed conn isn't
// actionable.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// operatorOverrideActor labels every event produced by a human-driven
// mutation. The admin bearer carries no per-user identity, so the actor names
// the surface — "a human overrode the autonomous decision" — not the person.
// Keeping it distinct from the legacy "operator" actor lets a reader select
// supervised override labels without also matching automated operator writes.
const operatorOverrideActor = "operator.manual"

// overrideReason reads the optional free-text reason a caller may attach to a
// manual override from the query string. Absent reason is not an error: the
// label is worth recording even unexplained.
func overrideReason(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("reason"))
}

// appendOverrideEvent records one manual operator override in the events store.
//
// Overrides are the factory's supervised training labels, so they are written
// on a best-effort basis: an append failure is logged and never fails the
// mutation the operator asked for. Losing a label must not lose the action.
func (o *operator) appendOverrideEvent(ctx context.Context, action, subjectKind, subjectID, reason string) {
	payload := map[string]any{
		"action":       action,
		"subject_kind": subjectKind,
		"subject_id":   subjectID,
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		payload["reason"] = reason
	}
	if err := o.store.Events.Append(ctx, &store.Event{
		Actor:       operatorOverrideActor,
		Kind:        "operator.override." + action,
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	}); err != nil {
		o.logger.Warn("operator override event append failed",
			"action", action, "subject_kind", subjectKind, "subject_id", subjectID, "error", err)
	}
}

// notImplemented is the standard 501 response for endpoints whose
// happy-path implementation lands in a later slice. The body names the
// slice that will fill it in so callers can grep for it in the plan.
func notImplemented(w http.ResponseWriter, slice string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "not implemented",
		"slice": slice,
	})
}
