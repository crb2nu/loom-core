package daemon

import (
	"errors"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/router"
	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

// decompHintTokenThreshold is the estimated token count above which a
// decomposition hint is emitted, suggesting the agent use the
// recursive-context workflow for large responses.
const decompHintTokenThreshold = 8000

func (p *callPipeline) transportFailure(stage string, err error, start time.Time) *mcp.Message {
	if p.conn != nil {
		p.conn.Healthy = false
	}

	// A muxed caller owns only its request registration and generation lease.
	// If its parent context ended, either by cancellation or deadline, the mux
	// discards that request's late response without corrupting the shared wire.
	// Do not count a caller-local outcome as upstream health failure or retire
	// the generation serving other agents.
	if p.localMuxCallerContextDone() {
		p.daemon.metrics.RecordRequest(p.serverName, p.method, "error", p.targetStr, time.Since(start))
		p.emitErrorAudit(p.targetStr, err.Error())
		p.recordTransportSpanEvent("daemon.server.mux_call_context_done_no_restart",
			attribute.String("server.name", p.serverName),
			attribute.String("failure.stage", stage),
			attribute.String("target", p.targetStr),
			attribute.String("caller.context_error", p.ctx.Err().Error()),
		)
		return p.internalError(err)
	}

	p.daemon.router.RecordFailure(p.serverName, p.target, err)
	p.daemon.metrics.RecordServerFailure(p.serverName, p.targetStr, stage)
	p.daemon.metrics.RecordRequest(p.serverName, p.method, "error", p.targetStr, time.Since(start))
	p.emitErrorAudit(p.targetStr, err.Error())

	// Per-call timeouts (context.DeadlineExceeded) from a still-alive
	// upstream subprocess must not tear down the subprocess. Doing so
	// drops every concurrent caller's pending Recv with `transport
	// closed`, which on the cluster's embedded HUD cascades into
	// /api/agent/heartbeat returning 5xx → Cloudflare 502.
	//
	// The conn was marked unhealthy above so the pool will discard it
	// on next Get(); the shared muxstdio.Transport survives and other
	// pending callers continue to receive their responses. The next
	// caller that needs a conn dials a fresh one over the same alive
	// subprocess.
	//
	// Only applies when the failure is a recv-side timeout (a send-side
	// failure or a real EOF/broken-pipe still means the subprocess
	// channel is dead and must be respawned).
	if p.target == router.TargetLocal && p.daemon.muxStdio && stage != "send" && isRPCTimeout(err) {
		// A recv timeout on a call that opted into an extended budget (caller
		// _timeout / timeout_seconds, or a per-server config override) means the
		// tool is legitimately slow, not that the shared transport is wedged.
		// Keep the subprocess alive and do NOT count it toward the teardown
		// breaker — a genuinely wedged transport is still caught by ordinary
		// default-budget calls, which do count. This stops a few legitimately
		// slow long-poll calls (e.g. gitlab poll_pipeline) from tearing down the
		// whole server process and dropping every concurrent caller.
		if p.callTimeoutExtended {
			p.daemon.logger.Warn("local server recv timed out on extended-budget call; subprocess kept alive",
				"server", p.serverName, "tool", p.toolName, "error", err)
			p.recordTransportSpanEvent("daemon.server.recv_timeout_extended_budget_no_restart",
				attribute.String("server.name", p.serverName),
				attribute.String("failure.stage", stage),
				attribute.String("failure.error", err.Error()),
				attribute.String("target", p.targetStr),
			)
			return p.internalError(err)
		}
		streak := p.daemon.recordLocalRecvTimeout(p.serverName)
		threshold := p.daemon.localRecvTimeoutBreakerThreshold()
		if streak < threshold {
			p.daemon.logger.Warn("local server recv timed out; subprocess kept alive",
				"server", p.serverName, "tool", p.toolName, "streak", streak, "threshold", threshold, "error", err)
			p.recordTransportSpanEvent("daemon.server.recv_timeout_no_restart",
				attribute.String("server.name", p.serverName),
				attribute.String("failure.stage", stage),
				attribute.String("failure.error", err.Error()),
				attribute.String("target", p.targetStr),
				attribute.Int64("recv_timeout.streak", streak),
			)
			return p.internalError(err)
		}
		// Circuit breaker tripped: N consecutive recv timeouts mean the shared
		// transport is desynced/stalled, not merely busy. Reset the streak and
		// fall through to the full teardown below so the next request dials a
		// fresh subprocess instead of hanging on a dead channel forever.
		p.daemon.resetLocalRecvTimeout(p.serverName)
		p.daemon.logger.Warn("local server recv timed out repeatedly; tearing down stalled transport",
			"server", p.serverName, "tool", p.toolName, "streak", streak, "threshold", threshold, "error", err)
		p.recordTransportSpanEvent("daemon.server.recv_timeout_breaker_tripped",
			attribute.String("server.name", p.serverName),
			attribute.String("failure.stage", stage),
			attribute.String("failure.error", err.Error()),
			attribute.String("target", p.targetStr),
			attribute.Int64("recv_timeout.streak", streak),
		)
	}

	// muxstdio.ErrDuplicateID means another concurrent caller registered
	// the same JSON-RPC id on the shared mux before we did. The subprocess
	// is alive — the failure is purely muxer-side state. Killing the
	// subprocess in response amplifies a transient collision into the
	// restart cascade seen on 2026-05-25. perConnTransport rewrites ids
	// to unique internal values so this normally cannot happen, but the
	// classifier remains as defense-in-depth for any caller that bypasses
	// the rewriter.
	if p.target == router.TargetLocal && errors.Is(err, muxstdio.ErrDuplicateID) {
		p.daemon.logger.Warn("local server muxer rejected duplicate id; subprocess kept alive",
			"server", p.serverName, "tool", p.toolName, "error", err)
		p.recordTransportSpanEvent("daemon.server.muxer_duplicate_id_no_restart",
			attribute.String("server.name", p.serverName),
			attribute.String("failure.stage", stage),
			attribute.String("failure.error", err.Error()),
			attribute.String("target", p.targetStr),
		)
		return p.internalError(err)
	}

	if p.target == router.TargetLocal {
		reason := "transport_recv_error"
		if stage == "send" {
			reason = "transport_send_error"
		}
		retired := p.retireObservedLocalGeneration(reason, err)
		if retired {
			p.daemon.logger.Warn("local server transport failed; generation retired",
				"server", p.serverName, "generation", p.localGeneration, "tool", p.toolName, "stage", stage, "error", err)
			p.recordTransportSpanEvent("daemon.server.restart_triggered",
				attribute.String("server.name", p.serverName),
				attribute.Int64("server.generation", int64(p.localGeneration)),
				attribute.String("failure.stage", stage),
				attribute.String("failure.error", err.Error()),
				attribute.String("target", p.targetStr),
			)
			if p.daemon.eventBus != nil {
				p.daemon.eventBus.Publish(EventProcessStop, map[string]any{
					"server":     p.serverName,
					"reason":     reason,
					"generation": p.localGeneration,
				})
			}
			// Upstream restarted; debounce a tool cache refresh so clients learn
			// about any new/removed tools once the pool repopulates.
			p.daemon.scheduleToolRefresh()
		} else {
			p.daemon.logger.Debug("stale local transport failure ignored by generation fence",
				"server", p.serverName, "generation", p.localGeneration, "tool", p.toolName, "stage", stage)
			p.recordTransportSpanEvent("daemon.server.stale_failure_ignored",
				attribute.String("server.name", p.serverName),
				attribute.Int64("server.generation", int64(p.localGeneration)),
				attribute.String("failure.stage", stage),
			)
		}
	} else if p.target == router.TargetHub && p.daemon.hubPool != nil {
		p.daemon.logger.Warn("hub transport failure; retiring owning connection",
			"server", p.serverName, "tool", p.toolName, "stage", stage, "error", err)
		p.recordTransportSpanEvent("daemon.proxy.hub_transport_retired",
			attribute.String("server.name", p.serverName),
			attribute.String("failure.stage", stage),
			attribute.String("failure.error", err.Error()),
			attribute.String("target", p.targetStr),
		)
		// The failed pool entry is already marked unhealthy and releaseConnection
		// closes that one independently owned WebSocket. Clearing by server name
		// here would discard unrelated healthy physical connections.
		p.daemon.scheduleToolRefresh()
	}

	return p.internalError(err)
}

func (p *callPipeline) localMuxCallerContextDone() bool {
	return p != nil && p.daemon != nil && p.target == router.TargetLocal &&
		p.daemon.muxStdio && p.ctx != nil && p.ctx.Err() != nil
}

func (p *callPipeline) recordTransportSpanEvent(name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(p.ctx)
	if !span.SpanContext().IsValid() {
		return
	}
	span.AddEvent(name, trace.WithAttributes(attrs...))
}

func (p *callPipeline) invalidParamsError(message string) *mcp.Message {
	return newErrorResponse(p.msg.ID, mcp.InvalidParams, message,
		newInvalidInputPipelineError(p.serverName, p.toolName, p.stage, message))
}

func (p *callPipeline) internalError(err error) *mcp.Message {
	return newErrorResponse(p.msg.ID, mcp.InternalError, err.Error(),
		newInternalPipelineError(p.serverName, p.toolName, p.stage, err))
}

func (p *callPipeline) internalErrorWithAudit(target string, err error) *mcp.Message {
	p.emitErrorAudit(target, err.Error())
	return newErrorResponse(p.msg.ID, mcp.InternalError, err.Error(),
		newInternalPipelineError(p.serverName, p.toolName, p.stage, err))
}

func (p *callPipeline) rbacDeniedError(decision AccessDecision) *mcp.Message {
	reason := fmt.Sprintf("access denied: agent %q with role %q cannot call %s__%s (%s)",
		decision.AgentID, decision.Role, decision.Server, decision.Tool, decision.Reason)
	return newErrorResponse(p.msg.ID, mcp.InvalidRequest, reason,
		newRBACDeniedPipelineError(p.serverName, p.toolName, decision))
}

func (p *callPipeline) policyDeniedError(decision GatewayPolicyDecision) *mcp.Message {
	return newErrorResponse(p.msg.ID, mcp.InvalidRequest,
		fmt.Sprintf("policy denied: %s (%s)", decision.ReasonCode, decision.Reason),
		newPolicyDeniedPipelineError(p.serverName, p.toolName, decision))
}

func (p *callPipeline) recordSuccessMetrics(duration time.Duration) {
	latencyMs := float64(duration.Milliseconds())
	p.daemon.router.RecordSuccess(p.serverName, p.target, latencyMs)
	p.daemon.metrics.RecordServerSuccess(p.serverName, p.targetStr)
	p.daemon.metrics.RecordRequest(p.serverName, p.method, "success", p.targetStr, duration)
	// A successful hub call means the hub recovered: reset the exponential
	// prefer-hub backoff so the next failure starts from the base again.
	if p.target == router.TargetHub {
		p.daemon.clearPreferHubBackoff(p.serverName)
	}
	// A successful local recv means the shared transport is healthy: clear any
	// accumulated recv-timeout streak so transient slow calls don't trip the
	// teardown breaker later.
	if p.target == router.TargetLocal {
		p.daemon.resetLocalRecvTimeout(p.serverName)
	}
}

func (p *callPipeline) markLocalActivity() {
	if p.target != router.TargetLocal || p.daemon.serverSupervisor != nil || p.daemon.procMgr == nil {
		return
	}
	p.daemon.procMgr.MarkActivity(p.serverName)
}

func (p *callPipeline) cacheSuccessResponse(resp *mcp.Message) {
	if p.cacheKey == "" || resp == nil || resp.Error != nil || resp.Result == nil || p.daemon.respCache == nil {
		return
	}

	p.daemon.respCache.Set(p.cacheKey, resp.Result, p.serverName, p.toolName)
	stats := p.daemon.respCache.Stats()
	p.daemon.metrics.UpdateResponseCacheStats(stats.Entries, stats.SizeBytes)
	p.daemon.logger.Debug("response cached", "server", p.serverName, "tool", p.toolName)
}

func (p *callPipeline) emitResponseAudit(resp *mcp.Message) {
	status := "success"
	errMsg := ""
	if resp != nil && resp.Error != nil {
		status = "error"
		errMsg = resp.Error.Message
	}
	p.daemon.emitAudit(p.params, p.serverName, p.toolName, p.targetStr, p.auditStart, status, errMsg, false, nil, p.stage, p.reqBytes, p.resBytes, p.auditTimings())
}

func (p *callPipeline) emitErrorAudit(target, errMsg string) {
	p.daemon.emitAudit(p.params, p.serverName, p.toolName, target, p.auditStart, "error", errMsg, false, nil, p.stage, 0, 0, p.auditTimings())
}

func (p *callPipeline) emitDecompHintIfLarge(resp *mcp.Message) {
	if resp == nil || resp.Result == nil || p.daemon.eventBus == nil {
		return
	}
	// Estimate tokens: ~4 bytes per token heuristic.
	estimatedTokens := (len(resp.Result) + 3) / 4
	if estimatedTokens < decompHintTokenThreshold {
		return
	}
	p.daemon.eventBus.Publish(EventDecompHint, map[string]any{
		"server":           p.serverName,
		"tool":             p.toolName,
		"response_bytes":   len(resp.Result),
		"estimated_tokens": estimatedTokens,
		"suggestion":       "Response exceeds 8K tokens. Consider using the recursive-context workflow for decomposed analysis.",
		"workflow":         "recursive-context",
	})
}
