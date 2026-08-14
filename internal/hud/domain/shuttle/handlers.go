package shuttle

import (
	"net/http"
)

// handleStatus returns the full shuttle snapshot. The snapshot embeds
// capacities, dispatch recommendations, and pending-task counts, so it is the
// single read surface for the shuttle domain.
func (d *ShuttleDomain) handleStatus(w http.ResponseWriter, _ *http.Request) {
	mon := d.deps.ShuttleMonitor()
	if mon == nil {
		d.deps.WriteError(w, http.StatusServiceUnavailable, "shuttle monitor not available", nil)
		return
	}
	snapshot := mon.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	d.deps.WriteJSON(w, http.StatusOK, snapshot)
}
