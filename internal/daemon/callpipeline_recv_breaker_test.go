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
	d.muxStdio = true
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

// TestTransportFailure_ExtendedBudgetSkipsBreaker verifies that recv timeouts
// on calls that opted into an extended timeout budget (callTimeoutExtended) are
// kept alive AND never counted toward the teardown breaker — so a few
// legitimately slow long-poll calls do not tear down the whole server process.
func TestTransportFailure_ExtendedBudgetSkipsBreaker(t *testing.T) {
	t.Setenv("LOOM_LOCAL_RECV_TIMEOUT_BREAKER", "3")

	d := newCallPipelineTestDaemon()
	d.muxStdio = true
	d.pool = newTestPool(t)
	d.runningServers.Store("gitlab", true)

	timeoutErr := daemonRPCPhaseError("tools/call", "recv", time.Second, context.DeadlineExceeded)

	mkPipeline := func() *callPipeline {
		return &callPipeline{
			daemon:              d,
			ctx:                 context.Background(),
			msg:                 &mcp.Message{ID: "x"},
			serverName:          "gitlab",
			toolName:            "poll_pipeline",
			method:              "tools/call",
			callTimeoutExtended: true, // caller opted into a long budget
			conn: &pool.Conn{
				ServerName: "gitlab",
				Transport:  &fakeTransport{},
				Healthy:    true,
			},
			target:    router.TargetLocal,
			targetStr: router.TargetLocal.String(),
		}
	}

	// Far more than the breaker threshold — none should tear down the server.
	for i := 1; i <= 5; i++ {
		mkPipeline().transportFailure("recv", timeoutErr, time.Now())
		if _, ok := d.runningServers.Load("gitlab"); !ok {
			t.Fatalf("extended-budget recv timeout #%d tore down the server; breaker must skip long-budget calls", i)
		}
	}

	// The streak must be untouched: the first real record returns 1.
	if got := d.recordLocalRecvTimeout("gitlab"); got != 1 {
		t.Fatalf("extended-budget timeouts must not increment the streak; got %d, want 1", got)
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
