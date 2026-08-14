package monitor

import (
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func TestOTelMonitorSnapshotWithoutStart(t *testing.T) {
	// OTelMonitor should return a zero snapshot when never started.
	m := NewOTelMonitor(nil, nil)
	snap := m.Snapshot()
	if snap.RuntimeConfigured {
		t.Error("expected snapshot.RuntimeConfigured=false before any refresh")
	}
	if snap.TracedServers != 0 {
		t.Errorf("expected TracedServers=0, got %d", snap.TracedServers)
	}
}

func TestOTelMonitorStopIdempotent(t *testing.T) {
	m := NewOTelMonitor(nil, nil)
	m.Stop()
	m.Stop()
}

func TestOTelMonitorOnRefreshFires(t *testing.T) {
	m := NewOTelMonitor(nil, nil)
	var got bridge.OTelStatusResult
	called := false
	m.OnRefresh(func(snap bridge.OTelStatusResult) {
		got = snap
		called = true
	})
	m.Update(bridge.OTelStatusResult{RuntimeConfigured: true, TracedServers: 3})
	if !called {
		t.Fatal("expected OnRefresh callback to fire on Update")
	}
	if !got.RuntimeConfigured || got.TracedServers != 3 {
		t.Errorf("callback got unexpected snapshot: %+v", got)
	}
}
