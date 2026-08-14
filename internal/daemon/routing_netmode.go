package daemon

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/netmode"
)

const (
	// netmodeWatchInterval is how often the watcher re-probes the LAN sentinel.
	// The probe is one TCP dial bounded at 600ms, so this is cheap.
	netmodeWatchInterval = 30 * time.Second
	// netmodeTunnelConfirmations is how many consecutive Tunnel probe results
	// are required before flipping LAN→off-LAN. A single failed probe can be a
	// transient outage (Wi-Fi reassociation, router blip) — exactly the race
	// that used to permanently flip prefer-local pins at startup — so one flaky
	// probe must not reroute pinned servers through the hub. The reverse
	// transition (off-LAN→LAN) applies on the first successful probe: a
	// completed TCP connect to the sentinel cannot be a false positive.
	netmodeTunnelConfirmations = 2
)

// netmodeWatchLoop keeps the offLAN flag current for the daemon's lifetime.
// Started from Start() only when off-LAN upgrades are armed (hub configured;
// offLANHubCapable non-nil). Without this loop a wrong startup probe result
// would stick until the next daemon restart.
func (d *Daemon) netmodeWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(netmodeWatchInterval)
	defer ticker.Stop()

	tunnelStreak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.done:
			return
		case <-ticker.C:
			d.netmodeWatchTick(&tunnelStreak, netmode.Resolve())
		}
	}
}

// netmodeWatchTick folds one probe result into the offLAN flag. Split from
// the loop so tests can drive probe sequences directly.
func (d *Daemon) netmodeWatchTick(tunnelStreak *int, mode netmode.Mode) {
	offLAN := d.offLAN.Load()

	if mode == netmode.LAN {
		*tunnelStreak = 0
		if offLAN {
			d.offLAN.Store(false)
			d.logger.Info("netmode: home LAN reachable again; restoring local routing pins",
				"servers", d.offLANPinnedServers())
		}
		return
	}

	if offLAN {
		*tunnelStreak = 0
		return
	}
	*tunnelStreak++
	if *tunnelStreak < netmodeTunnelConfirmations {
		d.logger.Debug("netmode: LAN sentinel unreachable; awaiting confirmation before rerouting",
			"streak", *tunnelStreak, "required", netmodeTunnelConfirmations)
		return
	}
	*tunnelStreak = 0
	d.offLAN.Store(true)
	d.logger.Info("netmode: off-LAN detected; routing LAN-pinned servers via hub",
		"servers", d.offLANPinnedServers())
}
