package hud

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Printed reset zones must work in minimal HUD images.

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

type AuthOutcome string

const (
	authUnknown       AuthOutcome = "unknown"
	authOAuthRejected AuthOutcome = "oauth_rejected"
	authOAuthQuota    AuthOutcome = "oauth_quota"
	authAPIBilling    AuthOutcome = "api_billing"
	authAPIAuth       AuthOutcome = "api_auth"
)

type claudeParseResult struct {
	sawResult     bool
	isError       bool
	result        string
	lastRetryNote string
}

func (p *ClaudeJSONLParser) authSnapshot() *claudeParseResult {
	r := p.authResult
	r.lastRetryNote = p.lastRetryNote
	return &r
}

// A terminal success supersedes retries. A terminal error is authoritative,
// even when unrecognized; stale stderr must not turn it into a billing retry.
func classifyClaudeAuth(parsed *claudeParseResult, exit int, stderrHead []string) AuthOutcome {
	if parsed != nil {
		if parsed.sawResult {
			if !parsed.isError {
				return authUnknown
			}
			return classifyClaudeAuthText(parsed.result)
		}
		if parsed.lastRetryNote != "" {
			return classifyClaudeAuthText(parsed.lastRetryNote)
		}
	}
	if exit == 0 {
		return authUnknown
	}
	return classifyClaudeAuthText(strings.Join(stderrHead, "\n"))
}

func classifyClaudeAuthText(message string) AuthOutcome {
	s := strings.ToLower(message)
	switch {
	case strings.Contains(s, "credit balance"), strings.Contains(s, "billing_error"), strings.Contains(s, "insufficient credits"):
		return authAPIBilling
	case strings.Contains(s, "usage limit"), strings.Contains(s, "usage-limit"), strings.Contains(s, "you've hit your limit"), strings.Contains(s, "you have hit your limit"):
		return authOAuthQuota
	case strings.Contains(s, "please run /login"), strings.Contains(s, "oauth token"), strings.Contains(s, "oauth authentication"):
		return authOAuthRejected
	case strings.Contains(s, "401"), strings.Contains(s, "invalid api key"), strings.Contains(s, "authentication_error"), strings.Contains(s, "invalid x-api-key"):
		return authAPIAuth
	default:
		return authUnknown
	}
}

func claudeAuthPolicy() string {
	switch p := os.Getenv("SPAWN_CLAUDE_AUTH_FALLBACK"); p {
	case "", "oauth-then-api":
		return "oauth-then-api"
	case "api-then-oauth", "none":
		return p
	default:
		return "none" // Fail closed on misspelled policy.
	}
}
func claudeAuthDailyCap() int {
	raw := os.Getenv("SPAWN_CLAUDE_API_FALLBACK_MAX_PER_DAY")
	if raw == "" {
		return 6
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var claudeResetPattern = regexp.MustCompile(`(?i)resets?\s+(?:([a-z]{3})\s+(\d{1,2}),?\s+)?(\d{1,2}(?::\d{2})?\s*[ap]m)\s*\(([^)]+)\)`)

func claudeAuthReset(message string, account string, now time.Time) time.Time {
	m := claudeResetPattern.FindStringSubmatch(message)
	if len(m) == 5 {
		loc, err := time.LoadLocation(m[4])
		if err == nil {
			value := strings.ToUpper(strings.ReplaceAll(m[3], " ", ""))
			layout := "3PM"
			if strings.Contains(value, ":") {
				layout = "3:04PM"
			}
			clock, err := time.Parse(layout, value)
			if err == nil {
				local := now.In(loc)
				if m[1] != "" {
					date, dateErr := time.ParseInLocation("Jan 2 2006", m[1]+" "+m[2]+" "+strconv.Itoa(local.Year()), loc)
					if dateErr != nil {
						return now.Add(claudeWeeklyWindow)
					}
					local = date
				}
				reset := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
				if !reset.After(now) {
					if m[1] == "" {
						reset = reset.AddDate(0, 0, 1)
					} else {
						reset = reset.AddDate(1, 0, 0)
					}
				}
				return reset
			}
		}
	}
	for _, a := range claudeAccounts() {
		if a.Key == account && a.HasReset {
			return a.windowStart(now).Add(claudeWeeklyWindow)
		}
	}
	return now.Add(claudeWeeklyWindow)
}

func claudeHealthyAccounts(accounts []claudeAccount, states []*spawn.State, now time.Time, attempted []spawn.AuthFailure) []claudeAccount {
	excluded := map[string]bool{}
	clearBefore, _ := time.Parse(time.RFC3339, os.Getenv("SPAWN_CLAUDE_AUTH_CLEAR_BEFORE"))
	for _, f := range attempted {
		excluded[f.Account] = true
	}
	for _, s := range states {
		if s == nil || s.Request.AgentType != "claude-code" {
			continue
		}
		for _, f := range s.AuthFailures {
			if (f.Outcome == string(authOAuthQuota) || f.Outcome == string(authOAuthRejected)) && now.Before(f.ResetAt) && !f.At.Before(clearBefore) {
				excluded[f.Account] = true
			}
		}
	}
	healthy := make([]claudeAccount, 0, len(accounts))
	for _, a := range accounts {
		if !excluded[a.Key] {
			healthy = append(healthy, a)
		}
	}
	return healthy
}

// handleClaudeAuthCompletion records evidence before terminalization and
// queues a retry only after its current driver has relinquished ownership.
func (o *SpawnOrchestrator) handleClaudeAuthCompletion(ctx context.Context, state *SpawnState, parsed *claudeParseResult, exit int, stderr []string) bool {
	if state.Request.AgentType != "claude-code" {
		return false
	}
	outcome := classifyClaudeAuth(parsed, exit, stderr)
	if outcome == authUnknown {
		return false
	}
	now := time.Now()
	failure := spawn.AuthFailure{Account: state.AuthAccount, Outcome: string(outcome), At: now}
	if outcome == authOAuthRejected {
		failure.ResetAt = now.Add(10 * time.Minute)
	}
	if outcome == authOAuthQuota {
		message := strings.Join(stderr, "\n")
		if parsed != nil {
			message = parsed.result + "\n" + parsed.lastRetryNote
		}
		failure.ResetAt = claudeAuthReset(message, state.AuthAccount, now)
	}
	mode, account := state.AuthMode, state.AuthAccount
	retry := false
	policy := claudeAuthPolicy()
	if state.AuthFallbackAt == nil && policy != "none" {
		attempted := append(append([]spawn.AuthFailure(nil), state.AuthFailures...), failure)
		healthy := claudeHealthyAccounts(claudeAccounts(), o.ctrl.List(), now, attempted)
		switch {
		case mode == spawn.AuthModeClusterOAuth && (outcome == authOAuthRejected || outcome == authOAuthQuota):
			if len(healthy) > 0 && len(attempted) <= len(claudeAccounts()) {
				account, _ = pickClaudeAccount(healthy, o.ctrl.List(), now)
				retry = true
			} else if policy == "oauth-then-api" {
				mode, account, retry = spawn.AuthModeClusterAPIKey, "", true
			}
		case mode == spawn.AuthModeClusterAPIKey && outcome == authAPIBilling && policy == "api-then-oauth" && len(healthy) > 0:
			mode = spawn.AuthModeClusterOAuth
			account, _ = pickClaudeAccount(healthy, o.ctrl.List(), now)
			retry = true
		}
	}
	if retry {
		if resolver, ok := o.substrateBackend(state.Request.Substrate).(backend.SecretResolver); ok {
			credentialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			refs := agentSecretEnvVarsForMode("claude-code", account, mode)
			values, resolveErr := resolver.ResolveSecretEnv(credentialCtx, refs)
			cancel()
			if resolveErr != nil || strings.TrimSpace(values[refs[0].Name]) == "" {
				retry = false
			}
		}
	}
	// Snapshot the origin mode first: the controller may hand back the very
	// pointer the caller holds (UpdateState stores it as-is), and
	// RecordAuthAttempt rewrites AuthMode through it before returning. Comparing
	// against state.AuthMode afterwards would never see the cross-mode hop.
	fromMode := state.AuthMode
	updated, reserved, err := o.ctrl.RecordAuthAttempt(ctx, state.SpawnID, failure, mode, account, retry, claudeAuthDailyCap())
	if err != nil {
		o.failSpawn(ctx, state, fmt.Sprintf("Claude auth %s: reserve fallback: %v", outcome, err))
		return true
	}
	if !reserved {
		return updated.AuthRetryPending
	}
	if mode != fromMode {
		if o.logger != nil {
			o.logger.Warn("claude spawn auth fallback", "spawn_id", state.SpawnID, "from", fromMode, "to", mode, "reason", outcome)
		}
		if o.metrics != nil && o.metrics.SpawnAuthFallbackTotal != nil {
			o.metrics.SpawnAuthFallbackTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("from", string(fromMode)), attribute.String("to", string(mode)), attribute.String("reason", string(outcome))))
		}
	}
	o.driversMu.Lock()
	owner := o.drivers[state.SpawnID]
	o.driversMu.Unlock()
	go func() {
		if owner != nil {
			<-owner.done
		}
		if current, ok := o.ctrl.Get(updated.SpawnID); ok && current.AuthRetryPending && current.StopRequestedAt == nil && !spawn.IsTerminal(current.Status) {
			o.redriveOrRun(updated.SpawnID, updated.Request)
		}
	}()
	return true
}

// Replay only result/retry records from the entire durable log. The init event
// alone can exceed the exec tail budget; reading a log head loses the failure.
func (o *SpawnOrchestrator) durableClaudeAuth(ctx context.Context, state *SpawnState) *claudeParseResult {
	if state.Request.AgentType != "claude-code" {
		return nil
	}
	be := o.substrateBackend(state.Request.Substrate)
	if be == nil {
		return nil
	}
	result, err := be.Exec(ctx, backend.ExecOpts{ContainerID: state.PodName, Command: "grep -E '\"type\"[[:space:]]*:[[:space:]]*\"result\"|\"subtype\"[[:space:]]*:[[:space:]]*\"api_retry\"' " + shellQuote(supervisorStateDir(state.SpawnID)+"/agent.log") + " | tail -n 20", TimeoutSec: 15})
	if err != nil || result == nil {
		return nil
	}
	// Use a fresh parser so replay does not double count live telemetry.
	return parseClaudeAuthLines(result.StdoutTail)
}

func parseClaudeAuthLines(output string) *claudeParseResult {
	r := &claudeParseResult{}
	for _, line := range strings.Split(output, "\n") {
		var ev struct {
			Type string `json:"type"`
			claudeResultEvent
			Attempt     int `json:"attempt"`
			ErrorStatus int `json:"error_status"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type == "result" {
			r.sawResult = true
			r.isError = ev.IsError || isClaudeErrorSubtype(ev.Subtype)
			r.result = ev.Result
		}
		if ev.Type == "system" && ev.Subtype == "api_retry" {
			r.lastRetryNote = fmt.Sprintf("API retry attempt %d, status %d", ev.Attempt, ev.ErrorStatus)
		}
	}
	return r
}
