package coordinator

import (
	"errors"
	"net/http"

	hudcoord "github.com/crb2nu/loom/internal/hud/coordinator"
)

// handleCoordinatorCompress triggers an on-demand memory compression cycle.
// POST /api/coordinator/compress
func (d *CoordinatorDomain) handleCoordinatorCompress(w http.ResponseWriter, r *http.Request) {
	coord := d.deps.Coordinator()
	if coord == nil {
		d.deps.WriteError(w, http.StatusServiceUnavailable, "coordinator is not enabled", nil)
		return
	}

	result, err := coord.RunCompression(r.Context())
	if err != nil {
		d.deps.WriteError(w, coordinatorErrStatus(err), "compression failed", err)
		return
	}
	if result == nil {
		d.deps.WriteJSON(w, http.StatusOK, map[string]string{"status": "nothing_to_compress"})
		return
	}

	d.deps.WriteJSON(w, http.StatusOK, result)
}

// coordinatorErrStatus returns 503 for ErrUnavailable, 502 otherwise.
func coordinatorErrStatus(err error) int {
	if errors.Is(err, hudcoord.ErrUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}
