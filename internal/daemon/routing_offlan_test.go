package daemon

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/crb2nu/loom/pkg/netmode"
)

func newOffLANTestDaemon(offLAN bool) *Daemon {
	d := &Daemon{
		logger: slog.Default(),
		routingPreferences: map[string]RoutingPreference{
			"agent_context": RoutingPreferLocal, // .loom/149 anti-storm pin
			"gitlab":        RoutingLocalOnly,   // hard local pin
			"weaver":        RoutingHubOnly,     // already hub
			"already_hub":   RoutingPreferHub,
			"godot":         RoutingLocalOnly, // registry local-only (not hub-capable)
		},
		offLANHubCapable: map[string]bool{
			"agent_context": true,
			"gitlab":        true,
			"weaver":        true,
			"already_hub":   true,
			"godot":         false, // can never run on the hub
			"prometheus":    true,  // hub-capable but unpinned
		},
	}
	d.offLAN.Store(offLAN)
	return d
}

func TestEffectiveRoutingPreference_OnLANHonorsPins(t *testing.T) {
	d := newOffLANTestDaemon(false)

	for server, want := range map[string]RoutingPreference{
		"agent_context": RoutingPreferLocal,
		"gitlab":        RoutingLocalOnly,
		"weaver":        RoutingHubOnly,
		"godot":         RoutingLocalOnly,
		"prometheus":    RoutingHealthBased, // no pin
	} {
		got, upgraded := d.effectiveRoutingPreference(server)
		if got != want || upgraded {
			t.Errorf("%s: got (%v, upgraded=%v) want (%v, upgraded=false)", server, got, upgraded, want)
		}
	}
}

func TestEffectiveRoutingPreference_OffLANUpgradesLocalPins(t *testing.T) {
	d := newOffLANTestDaemon(true)

	for server, want := range map[string]struct {
		pref     RoutingPreference
		upgraded bool
	}{
		"agent_context": {RoutingPreferHub, true},    // prefer-local upgraded
		"gitlab":        {RoutingPreferHub, true},    // local-only upgraded
		"weaver":        {RoutingHubOnly, false},     // unchanged
		"already_hub":   {RoutingPreferHub, false},   // unchanged
		"godot":         {RoutingLocalOnly, false},   // not hub-capable: never upgraded
		"prometheus":    {RoutingHealthBased, false}, // no pin: health-based fallback covers off-LAN
	} {
		got, upgraded := d.effectiveRoutingPreference(server)
		if got != want.pref || upgraded != want.upgraded {
			t.Errorf("%s: got (%v, upgraded=%v) want (%v, upgraded=%v)",
				server, got, upgraded, want.pref, want.upgraded)
		}
	}
}

func TestOffLANPinnedServers(t *testing.T) {
	d := newOffLANTestDaemon(false)
	want := []string{"agent_context", "gitlab"}
	if got := d.offLANPinnedServers(); !reflect.DeepEqual(got, want) {
		t.Errorf("offLANPinnedServers()=%v want %v", got, want)
	}
}

func TestNetmodeWatchTick_TransientTunnelDoesNotFlip(t *testing.T) {
	// A single failed sentinel probe (Wi-Fi blip, router restart) must not
	// reroute pinned servers: this is the startup race that silently sent
	// agent_context vendor-session reads to the empty-rooted cluster pod.
	d := newOffLANTestDaemon(false)
	streak := 0

	d.netmodeWatchTick(&streak, netmode.Tunnel)
	if d.offLAN.Load() {
		t.Fatal("one Tunnel probe flipped offLAN; want confirmation required")
	}
	d.netmodeWatchTick(&streak, netmode.LAN) // recovery resets the streak
	d.netmodeWatchTick(&streak, netmode.Tunnel)
	if d.offLAN.Load() {
		t.Fatal("non-consecutive Tunnel probes flipped offLAN")
	}
}

func TestNetmodeWatchTick_ConsecutiveTunnelFlips(t *testing.T) {
	d := newOffLANTestDaemon(false)
	streak := 0

	for i := 0; i < netmodeTunnelConfirmations; i++ {
		d.netmodeWatchTick(&streak, netmode.Tunnel)
	}
	if !d.offLAN.Load() {
		t.Fatalf("offLAN not set after %d consecutive Tunnel probes", netmodeTunnelConfirmations)
	}
	if streak != 0 {
		t.Errorf("streak=%d want 0 after flip", streak)
	}
}

func TestNetmodeWatchTick_LANRestoresImmediately(t *testing.T) {
	// Recovery must not require confirmations: a completed TCP connect to the
	// sentinel cannot be a false positive, and every off-LAN minute on the
	// home LAN routes pinned-local reads to a hub backend with different data.
	d := newOffLANTestDaemon(true)
	streak := 0

	d.netmodeWatchTick(&streak, netmode.LAN)
	if d.offLAN.Load() {
		t.Fatal("one LAN probe did not restore local routing")
	}
}

func TestNetmodeWatchTick_SteadyStatesAreQuiet(t *testing.T) {
	onLAN := newOffLANTestDaemon(false)
	streak := 0
	onLAN.netmodeWatchTick(&streak, netmode.LAN)
	if onLAN.offLAN.Load() || streak != 0 {
		t.Errorf("steady LAN: offLAN=%v streak=%d", onLAN.offLAN.Load(), streak)
	}

	offLAN := newOffLANTestDaemon(true)
	streak = 5
	offLAN.netmodeWatchTick(&streak, netmode.Tunnel)
	if !offLAN.offLAN.Load() || streak != 0 {
		t.Errorf("steady Tunnel: offLAN=%v streak=%d (streak should reset)", offLAN.offLAN.Load(), streak)
	}
}
