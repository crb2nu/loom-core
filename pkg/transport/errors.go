package transport

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
)

var permanentStatus = regexp.MustCompile(`\b(?:401|403)\b`)
var eof = regexp.MustCompile(`\beof\b`)

// isPermanent reports application and configuration failures, which take
// precedence over any transport or deadline text quoted in the same error.
func isPermanent(lower string) bool {
	if permanentStatus.MatchString(lower) {
		return true
	}
	for _, permanent := range []string{"reported error:", "(code=", "unauthorized", "forbidden", "missing token", "missing credential", "not configured", "policy", "disabled", "kill-switch", "kill switch"} {
		if strings.Contains(lower, permanent) {
			return true
		}
	}
	return false
}

// IsError returns true for the WebSocket / TCP failure modes
// that mean the cached connection is dead and we should redial. We
// match on error text rather than wrapped types because the gorilla
// websocket library and the mcp-go transport both wrap errors as
// strings before they reach us. Conservative on purpose — JSON-RPC
// errors and tool-reported errors (IsError=true) must NOT match.
func IsError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if isPermanent(s) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, needle := range []string{
		"websocket: close",         // gorilla close-frame errors (1006, 1001, etc.)
		"unexpected eof",           // half-closed read
		"broken pipe",              // EPIPE on Send after peer closed
		"connection reset by peer", // RST mid-flight
		"use of closed network connection",
		"connection refused", "dial tcp", "dial udp", "no such host", "temporary failure in name resolution",
		"transport closed", // mcp-go / fake transport
		"i/o timeout",      // ReadDeadline expiry
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return eof.MatchString(s)
}

// IsTimeout returns true when the peer was busy or slow rather than dead or
// misconfigured: the caller's deadline expired before an answer arrived, or a
// per-server call slot never freed in time ("wait for call slot: context
// deadline exceeded" from the Mills hub client while several runs and the
// reconciler share one hub). It is deliberately NOT part of IsError — a slot
// wait that timed out says nothing about the cached connection, so it must
// never trigger a redial — but the autonomy breaker treats both as substrate.
// Like IsError it yields to permanent application/configuration failures
// quoted in the same text, so "401 unauthorized: context deadline exceeded"
// stays permanent.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if isPermanent(s) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	for _, needle := range []string{
		"deadline exceeded",  // context.DeadlineExceeded wrapped as a string
		"wait for call slot", // mcphub per-server call slot contention
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
