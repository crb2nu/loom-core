// Package daemon provides the main Loom daemon orchestrator.
package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/router"
)

const (
	// preferHubBackoffBase is the first prefer-hub backoff after a hub failure.
	preferHubBackoffBase = 30 * time.Second
	// preferHubBackoffMax caps the exponential backoff growth.
	preferHubBackoffMax = 5 * time.Minute
)

// RoutingPreference controls how a specific server's traffic is routed.
type RoutingPreference int

const (
	// RoutingHealthBased uses the default health-aware routing (no override).
	RoutingHealthBased RoutingPreference = iota
	// RoutingLocalOnly forces all traffic to the local backend.
	RoutingLocalOnly
	// RoutingHubOnly forces all traffic to the hub backend.
	RoutingHubOnly
	// RoutingPreferLocal prefers local, falls back to hub when unhealthy.
	RoutingPreferLocal
	// RoutingPreferHub prefers hub, falls back to local when unavailable.
	RoutingPreferHub
)

// ParseRoutingPreference parses a string into a RoutingPreference.
func ParseRoutingPreference(s string) (RoutingPreference, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "health-based", "healthbased", "":
		return RoutingHealthBased, nil
	case "local-only", "localonly", "local":
		return RoutingLocalOnly, nil
	case "hub-only", "hubonly", "hub":
		return RoutingHubOnly, nil
	case "prefer-local", "preferlocal":
		return RoutingPreferLocal, nil
	case "prefer-hub", "preferhub":
		return RoutingPreferHub, nil
	default:
		return RoutingHealthBased, fmt.Errorf("unknown routing preference: %q (valid: local-only, hub-only, prefer-local, prefer-hub, health-based)", s)
	}
}

// String returns the canonical string representation.
func (r RoutingPreference) String() string {
	switch r {
	case RoutingLocalOnly:
		return "local-only"
	case RoutingHubOnly:
		return "hub-only"
	case RoutingPreferLocal:
		return "prefer-local"
	case RoutingPreferHub:
		return "prefer-hub"
	default:
		return "health-based"
	}
}

// ValidateRoutingPreferences validates a map of server name -> preference strings.
func ValidateRoutingPreferences(prefs map[string]string) error {
	for server, pref := range prefs {
		if server == "" {
			return fmt.Errorf("empty server name in routing preferences")
		}
		if _, err := ParseRoutingPreference(pref); err != nil {
			return fmt.Errorf("server %q: %w", server, err)
		}
	}
	return nil
}

// applyOffLANRouting upgrades anti-LAN routing pins to prefer-hub for hub-capable
// servers, returning the number of servers changed. It is called when the daemon
// detects it is off the home LAN: local backends that depend on LAN-only services
// (qdrant, *.lan, k8s) are then unreachable, so servers pinned local-only or
// prefer-local (e.g. the .loom/149 anti-storm pins on agent_context / gitlab)
// must route through the Cloudflare-tunnel hub instead.
//
// Only explicit pins are upgraded — servers with no preference keep their
// health-based default, whose existing local→hub fallback already covers off-LAN.
// hubCapable[name] must be false for registry local-only servers that can never
// run on the hub (e.g. godot, browserkit); those are never upgraded.
func applyOffLANRouting(prefs map[string]RoutingPreference, hubCapable map[string]bool) int {
	changed := 0
	for name, capable := range hubCapable {
		if !capable {
			continue
		}
		cur, ok := prefs[name]
		if !ok {
			continue
		}
		if cur == RoutingLocalOnly || cur == RoutingPreferLocal {
			prefs[name] = RoutingPreferHub
			changed++
		}
	}
	return changed
}

// applyRoutingPreference overrides a route decision based on per-server config.
// Returns the (possibly modified) target and whether the decision was overridden.
func applyRoutingPreference(pref RoutingPreference, original router.Target, hasHub bool) (router.Target, bool) {
	return applyRoutingPreferenceWithOptions(pref, original, hasHub, true)
}

func applyRoutingPreferenceWithOptions(pref RoutingPreference, original router.Target, hasHub bool, allowPreferHub bool) (router.Target, bool) {
	switch pref {
	case RoutingLocalOnly:
		if original != router.TargetLocal {
			return router.TargetLocal, true
		}
	case RoutingHubOnly:
		if !hasHub {
			return router.TargetUnavailable, true
		}
		if original != router.TargetHub {
			return router.TargetHub, true
		}
	case RoutingPreferHub:
		if !allowPreferHub {
			return original, false
		}
		if hasHub && original != router.TargetHub {
			return router.TargetHub, true
		}
		// Fall through to health-based if no hub
	case RoutingPreferLocal:
		if original != router.TargetLocal {
			return router.TargetLocal, true
		}
	case RoutingHealthBased:
		// No override
	}
	return original, false
}

// setPreferHubBackoff arms the prefer-hub suppression window for a server. A
// non-positive dur (the normal call-site value) selects the exponential
// backoff: 30s, 60s, 120s, ... capped at 5m, growing each time the hub fails
// again after a prior window expired. An explicit dur overrides the schedule
// (used by tests) and leaves the failure streak untouched.
func (d *Daemon) setPreferHubBackoff(serverName string, dur time.Duration) time.Time {
	if strings.TrimSpace(serverName) == "" {
		return time.Time{}
	}
	if dur <= 0 {
		dur = d.nextPreferHubBackoff(serverName)
	}
	until := time.Now().Add(dur)
	d.preferHubBackoff.Store(serverName, until)
	return until
}

// nextPreferHubBackoff increments the per-server consecutive-failure streak and
// returns the corresponding exponential duration (base << (streak-1), capped at
// preferHubBackoffMax). The streak persists across expired windows so repeated
// failures back off progressively further; clearPreferHubBackoff resets it once
// the hub serves a successful call again.
func (d *Daemon) nextPreferHubBackoff(serverName string) time.Duration {
	streak := 1
	if v, ok := d.preferHubBackoffStreak.Load(serverName); ok {
		if n, ok := v.(int); ok && n > 0 {
			streak = n + 1
		}
	}
	d.preferHubBackoffStreak.Store(serverName, streak)

	dur := preferHubBackoffBase
	for i := 1; i < streak && dur < preferHubBackoffMax; i++ {
		dur *= 2
	}
	if dur > preferHubBackoffMax {
		dur = preferHubBackoffMax
	}
	return dur
}

// clearPreferHubBackoff removes any active backoff window and resets the
// failure streak for a server (called when the hub serves a call successfully).
func (d *Daemon) clearPreferHubBackoff(serverName string) {
	if strings.TrimSpace(serverName) == "" {
		return
	}
	d.preferHubBackoff.Delete(serverName)
	d.preferHubBackoffStreak.Delete(serverName)
}

func (d *Daemon) preferHubBackoffActive(serverName string) (bool, time.Time) {
	if strings.TrimSpace(serverName) == "" {
		return false, time.Time{}
	}
	v, ok := d.preferHubBackoff.Load(serverName)
	if !ok {
		return false, time.Time{}
	}
	until, ok := v.(time.Time)
	if !ok {
		d.preferHubBackoff.Delete(serverName)
		return false, time.Time{}
	}
	if time.Now().Before(until) {
		return true, until
	}
	d.preferHubBackoff.Delete(serverName)
	return false, time.Time{}
}
