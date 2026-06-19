package daemon

import (
	"context"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/internal/router"
)

// TestLocalRecvTimeoutBreaker_CounterHelpers verifies the streak counter
// increments and resets per server.
func TestLocalRecvTimeoutBreaker_CounterHelpers(t *testing.T) {
	d := newCallPipelineTestDaemon()

	if got := d.recordLocalRecvTimeout("devbox"); got != 1 {
		t.Fatalf("first record = %d, want 1", got)
	}
	if got := d.recordLocalRecvTimeout("devbox"); got != 2 {
		t.Fatalf("second record = %d, want 2", got)
	}
	// Independent per server.
	if got := d.recordLocalRecvTimeout("other"); got != 1 {
		t.Fatalf("other server record = %d, want 1", got)
	}
	d.resetLocalRecvTimeout("devbox")
	if got := d.recordLocalRecvTimeout("devbox"); got != 1 {
		t.Fatalf("post-reset record = %d, want 1", got)
	}
}

// TestTransportFailure_LocalRecvTimeoutBreaker verifies that the first N-1
// consecutive local recv timeouts keep the subprocess alive, and the Nth trips
// a full transport teardown (runningServers entry removed).
func TestTransportFailure_LocalRecvTimeoutBreaker(t *testing.T) {
	t.Setenv("LOOM_LOCAL_RECV_TIMEOUT_BREAKER", "3")

	d := newCallPipelineTestDaemon()
	d.pool = newTestPool(t)
	d.runningServers.Store("devbox", true)

	timeoutErr := daemonRPCPhaseError("tools/call", "recv", time.Second, context.DeadlineExceeded)

	mkPipeline := func() *callPipeline {
		return &callPipeline{
			daemon:     d,
			ctx:        context.Background(),
			msg:        &mcp.Message{ID: "x"},
			serverName: "devbox",
			toolName:   "devbox_exec",
			method:     "tools/call",
			conn: &pool.Conn{
				ServerName: "devbox",
				Transport:  &fakeTransport{},
				Healthy:    true,
			},
			target:    router.TargetLocal,
			targetStr: router.TargetLocal.String(),
		}
	}

	// Streaks 1 and 2 keep the subprocess alive.
	for i := 1; i <= 2; i++ {
		mkPipeline().transportFailure("recv", timeoutErr, time.Now())
		if _, ok := d.runningServers.Load("devbox"); !ok {
			t.Fatalf("recv timeout #%d tore down the server too early", i)
		}
	}

	// Streak 3 trips the breaker → full teardown.
	mkPipeline().transportFailure("recv", timeoutErr, time.Now())
	if _, ok := d.runningServers.Load("devbox"); ok {
		t.Fatal("3rd consecutive recv timeout should have torn down the stalled transport")
	}

	// Streak resets after teardown.
	if got := d.recordLocalRecvTimeout("devbox"); got != 1 {
		t.Fatalf("streak after teardown = %d, want reset to 1", got)
	}
}

// TestRecordSuccessMetrics_ResetsLocalRecvStreak verifies a successful local
// recv clears an accumulated streak so transient slow calls don't trip the
// breaker later.
func TestRecordSuccessMetrics_ResetsLocalRecvStreak(t *testing.T) {
	d := newCallPipelineTestDaemon()
	d.recordLocalRecvTimeout("devbox")
	d.recordLocalRecvTimeout("devbox")

	p := &callPipeline{
		daemon:     d,
		serverName: "devbox",
		method:     "tools/call",
		target:     router.TargetLocal,
		targetStr:  router.TargetLocal.String(),
	}
	p.recordSuccessMetrics(5 * time.Millisecond)

	if got := d.recordLocalRecvTimeout("devbox"); got != 1 {
		t.Fatalf("streak after success = %d, want reset to 1", got)
	}
}
