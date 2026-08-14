package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hubproto"
	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/pkg/registry"
	loomtransport "github.com/crb2nu/loom/pkg/transport"
	"gitlab.flexinfer.ai/libs/mcp-go"
)

func TestHubKeepaliveLoop_ExitsOnDone(t *testing.T) {
	done := make(chan struct{})
	d := &Daemon{
		done:   done,
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		fileCfg: FileConfig{
			Hub: HubConfig{PingIntervalSeconds: 1}, // fast tick for test
		},
	}

	d.wg.Add(1)
	go d.hubKeepaliveLoop()

	// Close done immediately; loop should exit cleanly.
	close(done)
	d.wg.Wait()
}

func TestHandlePongResponse_CorrelatesAndGatesMisses(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	var live loomtransport.Liveness
	pingID, ok := live.Ping(2)
	if !ok {
		t.Fatal("initial ping unhealthy")
	}
	ping := hubproto.NewPing(pingID, "daemon", time.Now())
	pong, err := hubproto.NewPong(ping, "hub", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := hubproto.Encode(pong)
	if err != nil {
		t.Fatal(err)
	}
	resp := &mcp.Message{Result: json.RawMessage(encoded)}
	if correlated, downgraded := d.handlePongResponse(resp, &live); !correlated || downgraded {
		t.Fatalf("matching pong = correlated %v downgraded %v", correlated, downgraded)
	}
	if _, ok := live.Ping(2); !ok {
		t.Fatal("matching pong did not keep connection healthy")
	}

	wrong := hubproto.NewPing("wrong", "hub", time.Now())
	wrong.Method = hubproto.MethodPong
	wrongBytes, _ := hubproto.Encode(wrong)
	if correlated, downgraded := d.handlePongResponse(&mcp.Message{Result: wrongBytes}, &live); correlated || downgraded {
		t.Fatal("mismatched pong was accepted")
	}
	if _, ok := live.Ping(2); !ok {
		t.Fatal("first missed pong should remain below threshold")
	}
	if _, ok := live.Ping(2); ok {
		t.Fatal("absent second pong did not reach unhealthy threshold")
	}
}

func TestBuildControlPingUsesUniqueCorrelatedIDs(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	for _, id := range []string{"probe-1", "probe-2"} {
		msg := d.buildControlPing(id)
		if msg == nil || msg.Method != "hub/envelope" {
			t.Fatalf("buildControlPing(%q) = %#v", id, msg)
		}
		env, err := hubproto.Decode(msg.Params)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := hubproto.ParsePing(env); err != nil || got != id {
			t.Fatalf("ParsePing(%q) = %q, %v", id, got, err)
		}
	}
}

func TestHandlePongResponse_RawCompatibilityDowngrade(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	var live loomtransport.Liveness
	_, _ = live.Ping(1)
	correlated, downgraded := d.handlePongResponse(&mcp.Message{Result: json.RawMessage(`{"tools":[]}`)}, &live)
	if correlated || !downgraded {
		t.Fatalf("raw response = correlated %v downgraded %v", correlated, downgraded)
	}
	if _, ok := live.Ping(1); !ok {
		t.Fatal("compatibility downgrade did not reset liveness")
	}
}

func TestHubKeepalivePing_NoIdleSkips(t *testing.T) {
	// When there are no idle connections in the hub pool, the ping should be a no-op.
	hubPool := pool.New(pool.Config{
		MaxIdle:     1,
		MaxOpen:     1,
		IdleTimeout: time.Minute,
		DialFunc:    nil, // won't be called
	})
	defer hubPool.Close()

	d := &Daemon{
		hubPool: hubPool,
		logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	// Should not panic or attempt any dial.
	d.hubKeepalivePing()
}

func TestPickHubServer_NilRegistry(t *testing.T) {
	d := &Daemon{}
	if name := d.pickHubServer(); name != "" {
		t.Fatalf("expected empty server name with nil registry, got %q", name)
	}
}

func TestPickHubServer_ReturnsFirstHubCapable(t *testing.T) {
	reg := &registry.Registry{
		Servers: []*registry.Server{
			{Name: "local-only", Categories: []string{"local-only"}},
			{Name: "hub-capable", Categories: []string{"hub"}},
		},
	}

	d := &Daemon{registry: reg}
	name := d.pickHubServer()

	// The local-only server has "local-only" category, so IsLocalOnly() returns true.
	// The hub-capable server has "hub" category, so IsLocalOnly() returns false.
	if name != "hub-capable" {
		t.Fatalf("expected 'hub-capable', got %q", name)
	}
}
