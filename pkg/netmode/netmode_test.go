package netmode

import (
	"context"
	"net"
	"testing"
)

func TestResolve_EnvOverride(t *testing.T) {
	cases := map[string]Mode{
		"lan":     LAN,
		"local":   LAN,
		"on-lan":  LAN,
		"tunnel":  Tunnel,
		"remote":  Tunnel,
		"off-lan": Tunnel,
	}
	for val, want := range cases {
		t.Run(val, func(t *testing.T) {
			t.Setenv("LOOM_NETWORK_MODE", val)
			if got := Resolve(); got != want {
				t.Fatalf("Resolve()=%v want %v for LOOM_NETWORK_MODE=%q", got, want, val)
			}
		})
	}
}

func TestResolve_AutoProbe(t *testing.T) {
	// A real listener stands in for the reachable LAN sentinel.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	t.Run("reachable sentinel -> LAN", func(t *testing.T) {
		t.Setenv("LOOM_NETWORK_MODE", "auto")
		t.Setenv("LOOM_LAN_SENTINEL", ln.Addr().String())
		if got := Resolve(); got != LAN {
			t.Fatalf("Resolve()=%v want LAN (reachable sentinel)", got)
		}
	})

	t.Run("unreachable sentinel -> Tunnel", func(t *testing.T) {
		t.Setenv("LOOM_NETWORK_MODE", "auto")
		// Closed/unused port on loopback: connect refused -> Tunnel.
		t.Setenv("LOOM_LAN_SENTINEL", "127.0.0.1:1")
		if got := Resolve(); got != Tunnel {
			t.Fatalf("Resolve()=%v want Tunnel (unreachable sentinel)", got)
		}
	})
}
