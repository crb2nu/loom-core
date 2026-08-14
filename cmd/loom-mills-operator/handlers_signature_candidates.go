package main

import (
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Signature-candidate listing defaults: two weeks, matching the miner's own
// lookback so the endpoint shows the proposals derived from the window the
// sweep is currently mining. The scan limit bounds the event window read.
const (
	signatureCandidatesDefaultWindow = 336 * time.Hour
	signatureCandidatesScanLimit     = 500
)

// signatureCandidateView is one proposed classifier signature: the normalized
// phrase, how many unexplained escalations share it, what they looked like, and
// how many escalations across the mined window the phrase would match if it
// were promoted to a live classifier signature.
type signatureCandidateView struct {
	Fingerprint      string    `json:"fingerprint"`
	Phrase           string    `json:"phrase"`
	MemberCount      int64     `json:"member_count"`
	WindowMatchCount int64     `json:"window_match_count"`
	SampleEvidence   []string  `json:"sample_evidence"`
	FirstSeen        string    `json:"first_seen,omitempty"`
	LastSeen         string    `json:"last_seen,omitempty"`
	ProposedAt       time.Time `json:"proposed_at"`
}

type signatureCandidatesResponse struct {
	Window     string                   `json:"window"`
	Since      time.Time                `json:"since"`
	Count      int                      `json:"count"`
	Candidates []signatureCandidateView `json:"candidates"`
}

// handleSignatureCandidatesList serves the classifier-signature candidates
// mined over ?window=, newest first. Open read like GET /api/mills/regressions:
// it aggregates durable events and exposes no secrets. Nothing here promotes a
// candidate — promotion is a reviewed change to the classifier corpus.
func (o *operator) handleSignatureCandidatesList(w http.ResponseWriter, r *http.Request) {
	window := signatureCandidatesDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	if o.store == nil || o.store.Events == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "events store unavailable"})
		return
	}

	since := time.Now().UTC().Add(-window)
	// The sweep is the sole writer of this actor, so selecting on it uses the
	// indexed window scan and the kind filter below is a cheap belt-and-braces
	// guard against a future actor reuse.
	events, err := o.store.Events.ListByActorSince(r.Context(), mills.SignatureMinerActor, since, signatureCandidatesScanLimit)
	if err != nil {
		o.logger.Warn("signature candidate list failed", "window", window.String(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resp := signatureCandidatesResponse{
		Window:     window.String(),
		Since:      since,
		Candidates: make([]signatureCandidateView, 0, len(events)),
	}
	for _, ev := range events {
		if ev == nil || ev.Kind != mills.SignatureCandidateEventKind {
			continue
		}
		resp.Candidates = append(resp.Candidates, signatureCandidateView{
			Fingerprint:      ev.SubjectID,
			Phrase:           eventPayloadString(ev, "phrase"),
			MemberCount:      eventPayloadInt64(ev, "member_count"),
			WindowMatchCount: eventPayloadInt64(ev, "window_match_count"),
			SampleEvidence:   eventPayloadStrings(ev, "sample_evidence"),
			FirstSeen:        eventPayloadString(ev, "first_seen"),
			LastSeen:         eventPayloadString(ev, "last_seen"),
			ProposedAt:       ev.OccurredAt,
		})
	}
	resp.Count = len(resp.Candidates)
	writeJSON(w, http.StatusOK, resp)
}

// eventPayloadStrings reads a string-list payload field. Payloads round-trip
// through JSON, so a []string written by the sweep is read back as []any; both
// forms are accepted so the value survives regardless of which side produced it.
func eventPayloadStrings(ev *store.Event, key string) []string {
	if ev == nil {
		return nil
	}
	switch v := ev.Payload[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
