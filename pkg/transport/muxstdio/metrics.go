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
	// IncMuxDropsNoPending counts responses dropped because no caller ever
	// registered the message's id: a genuinely unsolicited response from the
	// server. Responses that arrive after the caller gave up are counted by
	// IncMuxLateResponses instead.
	IncMuxDropsNoPending()
	// IncMuxLateResponses counts responses that arrived after the Recv caller
	// abandoned the call (its context expired or the transport was closing).
	// The upstream server finished the work; the caller had already reported
	// a timeout. A high rate means the per-call budget is set just below the
	// server's real latency and every timed-out call is wasted work.
	IncMuxLateResponses()
	// IncMuxNotifications counts id-less messages routed to NotificationCh.
	IncMuxNotifications()
}

type nopMetrics struct{}

func (nopMetrics) IncMuxDispatches()     {}
func (nopMetrics) IncMuxDropsFullChan()  {}
func (nopMetrics) IncMuxDropsNoPending() {}
func (nopMetrics) IncMuxLateResponses()  {}
func (nopMetrics) IncMuxNotifications()  {}
