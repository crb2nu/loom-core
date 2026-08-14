package main

import (
	"net/http"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills/overseer"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// overseerRecentActionsWindow bounds the recent-actions read on the status
// endpoint; the events table is the durable archive.
const (
	overseerRecentActionsWindow = 24 * time.Hour
	overseerRecentActionsLimit  = 25
)

// overseerAgentView is one agent's row in GET /api/mills/overseers.
type overseerAgentView struct {
	overseer.AgentStatus
	// Enabled reflects the policy gates (master ∧ per-agent), not the
	// harness's composed admission barrier.
	Enabled bool `json:"enabled"`
	DryRun  bool `json:"dry_run"`
	// Suppression is the sentinel's live admission-suppression lease (nil
	// when none) — it explains an idle mill at a glance.
	Suppression *overseer.Suppression `json:"suppression,omitempty"`
}

// overseerEntry binds one supervisory agent's harness to its policy
// accessors. The status endpoint previously resolved enabled/dry-run through
// a hardcoded name switch, so a fourth registered agent would silently
// report enabled=false — the accessors travel with the registration instead.
type overseerEntry struct {
	Harness *overseer.Harness
	// Enabled reports the policy gates (master ∧ per-agent) for the status
	// view — NOT the harness's admission-composed Enabled closure.
	Enabled func() bool
	// DryRun reports the agent's live dry-run state.
	DryRun func() bool
	// Suppression exposes the agent's live admission-suppression lease;
	// nil for agents that never suppress (groomer).
	Suppression func() *overseer.Suppression
}

type overseersStatusResponse struct {
	Enabled       bool                      `json:"enabled"` // master gate
	Agents        []overseerAgentView       `json:"agents"`
	RecentActions map[string][]*store.Event `json:"recent_actions"`
}

// handleOverseersStatus is the open read: policy gates + harness snapshots +
// each agent's 24h audit trail.
func (o *operator) handleOverseersStatus(w http.ResponseWriter, r *http.Request) {
	pol := o.policy.Current()
	resp := overseersStatusResponse{
		Agents:        make([]overseerAgentView, 0, len(o.overseers)),
		RecentActions: make(map[string][]*store.Event, len(o.overseers)),
	}
	if pol != nil {
		resp.Enabled = pol.Overseers.Enabled
	}
	names := make([]string, 0, len(o.overseers))
	for name := range o.overseers {
		names = append(names, name)
	}
	sort.Strings(names)
	since := time.Now().UTC().Add(-overseerRecentActionsWindow)
	for _, name := range names {
		entry := o.overseers[name]
		view := overseerAgentView{AgentStatus: entry.Harness.Status()}
		if entry.Enabled != nil {
			view.Enabled = entry.Enabled()
		}
		if entry.DryRun != nil {
			view.DryRun = entry.DryRun()
		}
		if entry.Suppression != nil {
			view.Suppression = entry.Suppression()
		}
		resp.Agents = append(resp.Agents, view)
		if o.store != nil && o.store.Events != nil {
			events, err := o.store.Events.ListByActorSince(r.Context(), "overseer."+name, since, overseerRecentActionsLimit)
			if err != nil {
				o.logger.Warn("overseers status: recent actions read failed", "agent", name, "error", err)
				continue
			}
			resp.RecentActions[name] = events
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// overseerFromPath resolves the {agent} path segment or writes a 404.
func (o *operator) overseerFromPath(w http.ResponseWriter, r *http.Request) (string, *overseer.Harness) {
	name := r.PathValue("agent")
	entry, ok := o.overseers[name]
	if !ok || entry.Harness == nil {
		http.Error(w, "unknown overseer agent: "+name, http.StatusNotFound)
		return "", nil
	}
	return name, entry.Harness
}

// handleOverseerPause soft-pauses one agent's loop (in-memory; a restart or
// resume clears it — durable disablement is the policy ConfigMap).
func (o *operator) handleOverseerPause(w http.ResponseWriter, r *http.Request) {
	name, h := o.overseerFromPath(w, r)
	if h == nil {
		return
	}
	h.SetPaused(true)
	o.logger.Info("overseer paused via API", "agent", name)
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "paused": true})
}

// handleOverseerResume clears the runtime soft-pause.
func (o *operator) handleOverseerResume(w http.ResponseWriter, r *http.Request) {
	name, h := o.overseerFromPath(w, r)
	if h == nil {
		return
	}
	h.SetPaused(false)
	o.logger.Info("overseer resumed via API", "agent", name)
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "paused": false})
}

// handleOverseerTick drives one bounded tick through the same path the loop
// uses — the ops/testing aid for verifying a policy change without waiting
// out the interval. The policy gates still apply inside the agent.
func (o *operator) handleOverseerTick(w http.ResponseWriter, r *http.Request) {
	name, h := o.overseerFromPath(w, r)
	if h == nil {
		return
	}
	res, err := h.TickOnce(r.Context())
	if err != nil {
		o.logger.Warn("overseer manual tick failed", "agent", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"agent": name, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "result": res})
}
