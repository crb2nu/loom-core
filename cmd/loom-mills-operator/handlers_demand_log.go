package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Demand-log defaults: one day, the horizon at which "the council declined
// this yesterday" is still floor news rather than archaeology. The scan
// limit bounds the actor-window read behind it.
const (
	demandLogDefaultWindow = 24 * time.Hour
	demandLogScanLimit     = 500
	demandLogMaxRows       = 50
	// The council mutator's guard actor (main.go wires the ActionRecorder
	// with this literal) and the merged-work suppression action under it.
	// The prefix match keeps .dryrun-suffixed records visible should the
	// mutator ever run a soak.
	demandLogActor      = "council.mutator"
	demandLogKindPrefix = demandLogActor + ".merged_work_skip"
)

// demandLogRow is one demand-side decision: a proposal the council declined
// to mint because it restated recently-merged work. Fields come straight off
// the suppression event's payload — the log renders judgment, it does not
// reinterpret it.
type demandLogRow struct {
	OccurredAt    time.Time `json:"occurred_at"`
	ProposalTitle string    `json:"proposal_title"`
	MergedTitle   string    `json:"merged_title"`
	MergedURL     string    `json:"merged_url,omitempty"`
	MergedRef     string    `json:"merged_ref,omitempty"`
	Score         float64   `json:"score,omitempty"`
	Basis         string    `json:"basis,omitempty"`
	DryRun        bool      `json:"dry_run,omitempty"`
}

type demandLogResponse struct {
	Window string         `json:"window"`
	Since  time.Time      `json:"since"`
	Count  int            `json:"count"`
	Rows   []demandLogRow `json:"rows"`
}

// handleDemandLog serves the demand-side decision log — what the factory
// declined to make, and why. Open read like GET /api/mills/regressions: it
// aggregates durable audit events and exposes no secrets.
func (o *operator) handleDemandLog(w http.ResponseWriter, r *http.Request) {
	window := demandLogDefaultWindow
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
	events, err := o.store.Events.ListByActorSince(r.Context(), demandLogActor, since, demandLogScanLimit)
	if err != nil {
		o.logger.Warn("demand log list failed", "window", window.String(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	rows := make([]demandLogRow, 0, len(events))
	for _, ev := range events {
		if ev == nil || !strings.HasPrefix(ev.Kind, demandLogKindPrefix) {
			continue
		}
		p := ev.Payload
		rows = append(rows, demandLogRow{
			OccurredAt:    ev.OccurredAt,
			ProposalTitle: stringField(p, "proposal_title"),
			MergedTitle:   stringField(p, "merged_title"),
			MergedURL:     stringField(p, "merged_url"),
			MergedRef:     ev.SubjectID,
			Score:         floatField(p, "score"),
			Basis:         stringField(p, "basis"),
			DryRun:        strings.HasSuffix(ev.Kind, ".dryrun"),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].OccurredAt.After(rows[j].OccurredAt) })
	if len(rows) > demandLogMaxRows {
		rows = rows[:demandLogMaxRows]
	}

	writeJSON(w, http.StatusOK, demandLogResponse{
		Window: window.String(),
		Since:  since,
		Count:  len(rows),
		Rows:   rows,
	})
}

func stringField(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	s, _ := p[key].(string)
	return s
}

func floatField(p map[string]any, key string) float64 {
	if p == nil {
		return 0
	}
	switch v := p[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	default:
		return 0
	}
}
