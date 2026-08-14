package muxstdio

// Metrics is the minimal interface the demuxer uses for observability.
// Implementations must be safe for concurrent use; the demuxer calls these
// from the reader goroutine and from caller goroutines.
//
// All methods are mandatory; nil-safety is provided by [nopMetrics], which is
// installed by default. Callers that want real metrics should pass
// [WithMetrics] to [New].
type Metrics interface {
	// IncMuxDispatches counts inbound responses successfully routed to a
	// pending Recv caller.
	IncMuxDispatches()
	// IncMuxDropsFullChan counts messages dropped because the destination
	// channel (per-id or notification) was full.
	IncMuxDropsFullChan()
	// IncMuxDropsNoPending counts responses dropped because no caller had
	// a pending registration for the message's id (cancelled call, or
	// unsolicited response from the server).
	IncMuxDropsNoPending()
	// IncMuxNotifications counts id-less messages routed to NotificationCh.
	IncMuxNotifications()
}

type nopMetrics struct{}

func (nopMetrics) IncMuxDispatches()     {}
func (nopMetrics) IncMuxDropsFullChan()  {}
func (nopMetrics) IncMuxDropsNoPending() {}
func (nopMetrics) IncMuxNotifications()  {}
