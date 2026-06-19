package daemon

import (
	"sync/atomic"

	"github.com/crb2nu/loom/pkg/env"
)

// defaultLocalRecvTimeoutThreshold is the number of consecutive recv timeouts
// on a local server's shared stdio transport that trips a full transport
// teardown.
//
// A single recv timeout from a still-alive, busy subprocess is intentionally
// kept alive (see transportFailure) so one slow tool call does not drop every
// concurrent caller. But N consecutive recv timeouts indicate the shared
// transport itself has desynced or stalled (e.g. the subprocess reader is
// wedged, or loomd holds a stale transport to a replaced process). In that
// state every future request hangs for the full timeout and never recovers,
// which is what makes a server like devbox appear permanently "unavailable".
// Tearing the transport down and respawning lets the next request get a fresh,
// healthy channel. Override with LOOM_LOCAL_RECV_TIMEOUT_BREAKER.
const defaultLocalRecvTimeoutThreshold = 3

// localRecvTimeoutBreakerThreshold returns the consecutive-recv-timeout count
// that trips a transport teardown, honoring the env override.
func (d *Daemon) localRecvTimeoutBreakerThreshold() int64 {
	n := env.Int("LOOM_LOCAL_RECV_TIMEOUT_BREAKER", defaultLocalRecvTimeoutThreshold)
	if n < 1 {
		return defaultLocalRecvTimeoutThreshold
	}
	return int64(n)
}

// recordLocalRecvTimeout increments and returns the consecutive recv-timeout
// streak for serverName.
func (d *Daemon) recordLocalRecvTimeout(serverName string) int64 {
	v, _ := d.localRecvTimeoutStreak.LoadOrStore(serverName, new(atomic.Int64))
	return v.(*atomic.Int64).Add(1)
}

// resetLocalRecvTimeout clears the recv-timeout streak after a successful local
// recv (or after a teardown), so transient slow calls don't accumulate toward
// the breaker.
func (d *Daemon) resetLocalRecvTimeout(serverName string) {
	if v, ok := d.localRecvTimeoutStreak.Load(serverName); ok {
		v.(*atomic.Int64).Store(0)
	}
}
