package killtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow"
	"github.com/crb2nu/loom/pkg/poll"
)

type CrashLease struct {
	Token      string                    `json:"token"`
	RequestID  string                    `json:"request_id"`
	RunID      string                    `json:"run_id"`
	SpawnID    string                    `json:"spawn_id"`
	ExpiresAt  time.Time                 `json:"expires_at"`
	ObservedAt time.Time                 `json:"-"`
	Authority  OperatorResponseAuthority `json:"-"`
}

// String deliberately omits the bearer token so diagnostic formatting of a
// lease value cannot disclose destructive authority.
func (lease CrashLease) String() string {
	return fmt.Sprintf("{token:<redacted> token_present:%t request_id:%q run_id:%q spawn_id:%q expires_at:%s observed_at:%s}",
		lease.Token != "", lease.RequestID, lease.RunID, lease.SpawnID,
		lease.ExpiresAt.UTC().Format(time.RFC3339Nano), lease.ObservedAt.UTC().Format(time.RFC3339Nano))
}

func redactSecretText(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, encoded := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret)} {
			if encoded != "" {
				value = strings.ReplaceAll(value, encoded, "<redacted>")
			}
		}
	}
	return value
}

type secretRedactedError struct {
	message string
	cause   error
}

func (err *secretRedactedError) Error() string    { return err.message }
func (err *secretRedactedError) Unwrap() error    { return err.cause }
func (err *secretRedactedError) GoString() string { return err.message }
func (err *secretRedactedError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(err.message))
}

func redactedSecretError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return &secretRedactedError{message: redactSecretText(err.Error(), secrets...), cause: err}
}

// Evidence returns the non-secret part of a server-authored lease response.
func (lease CrashLease) Evidence() CrashLeaseEvidence {
	return CrashLeaseEvidence{
		RequestID:         lease.RequestID,
		RunID:             lease.RunID,
		SpawnID:           lease.SpawnID,
		ObservedAt:        lease.ObservedAt,
		ExpiresAt:         lease.ExpiresAt,
		OperatorAuthority: lease.Authority,
	}
}

func randomRequestID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func NewCanaryRunID() (string, error) {
	return randomRequestID("wf-canary-")
}

const (
	// GateBindingContract versions the durable cross-file evidence chain.
	GateBindingContract        = "mills-s1c-gate-binding"
	GateBindingContractVersion = 1
	// S1cGateRequiredRuns is part of the durable gate identity contract.
	S1cGateRequiredRuns = 3
	// MergingGateRequiredRuns is the S6-full merging-canary contract: exactly
	// one run, because each merging run merges its canary MR into main and
	// moves the expected-revision identity baseline out from under any
	// subsequent run in the same gate.
	MergingGateRequiredRuns = 1
)

// NewCanaryGateID allocates the compact random identity shared by all runs in
// one S1c gate. The run IDs themselves are deterministic children of it.
func NewCanaryGateID() (string, error) {
	return randomRequestID("")
}

// CanaryRunIDForGate returns the canonical server-bound run ID for one gate
// position. LaunchCanary subsequently proves the API persisted this exact ID.
func CanaryRunIDForGate(gateID string, runIndex int) (string, error) {
	if err := validateCanaryGateID(gateID); err != nil {
		return "", err
	}
	if runIndex < 1 || runIndex > S1cGateRequiredRuns {
		return "", fmt.Errorf("S1c gate run index %d is outside 1..%d", runIndex, S1cGateRequiredRuns)
	}
	return fmt.Sprintf("wf-canary-%s-%02d", gateID, runIndex), nil
}

func validateCanaryGateID(gateID string) error {
	if len(gateID) != 32 || strings.ToLower(gateID) != gateID {
		return fmt.Errorf("S1c gate id %q is not 32 lowercase hexadecimal characters", gateID)
	}
	if _, err := hex.DecodeString(gateID); err != nil {
		return fmt.Errorf("S1c gate id %q is not hexadecimal: %w", gateID, err)
	}
	return nil
}

// ValidateGateBinding proves a non-empty evidence binding has the current
// three-run contract, canonical run ID, and normalized predecessor link. A
// zero binding is reserved for recovery-only attached runs.
func ValidateGateBinding(evidence Evidence) error {
	binding := evidence.GateBinding
	if binding == (GateBinding{}) {
		return nil
	}
	if binding.Contract != GateBindingContract || binding.ContractVersion != GateBindingContractVersion {
		return fmt.Errorf("unsupported S1c gate binding contract %q-v%d (want %q-v%d)",
			binding.Contract, binding.ContractVersion, GateBindingContract, GateBindingContractVersion)
	}
	if err := validateCanaryGateID(binding.GateID); err != nil {
		return err
	}
	requiredRuns := S1cGateRequiredRuns
	if evidence.MergingCanary {
		requiredRuns = MergingGateRequiredRuns
	}
	if binding.RequiredRuns != requiredRuns {
		return fmt.Errorf("S1c gate requires %d runs, binding declares %d",
			requiredRuns, binding.RequiredRuns)
	}
	wantRunID, err := CanaryRunIDForGate(binding.GateID, binding.RunIndex)
	if err != nil {
		return err
	}
	if evidence.RunID != wantRunID {
		return fmt.Errorf("S1c gate run %d id %q differs from canonical %q",
			binding.RunIndex, evidence.RunID, wantRunID)
	}
	if binding.GateStartedAt.IsZero() {
		return errors.New("S1c gate binding has no start timestamp")
	}
	runStartedAt := evidence.InitialPreflight.FluxSourcesStart.ObservedAt
	if !runStartedAt.IsZero() && runStartedAt.Before(binding.GateStartedAt) {
		return fmt.Errorf("S1c gate run %d predates gate start: run=%s gate=%s",
			binding.RunIndex, runStartedAt, binding.GateStartedAt)
	}
	if binding.RunIndex == 1 {
		if binding.PreviousEvidenceSHA256 != "" {
			return errors.New("S1c gate run 1 unexpectedly has a predecessor evidence hash")
		}
	} else if !isNormalizedSHA256(binding.PreviousEvidenceSHA256) {
		return fmt.Errorf("S1c gate run %d has invalid predecessor evidence SHA-256 %q",
			binding.RunIndex, binding.PreviousEvidenceSHA256)
	}
	return nil
}

func (h *Harness) AcquireCrashLease(ctx context.Context, runID, spawnID string) (CrashLease, error) {
	requestID, err := randomRequestID("s1c-lease-")
	if err != nil {
		return CrashLease{}, err
	}
	payload, err := json.Marshal(map[string]string{
		"request_id": requestID, "run_id": runID, "spawn_id": spawnID,
	})
	if err != nil {
		return CrashLease{}, err
	}
	endpoint := strings.TrimRight(h.cfg.OperatorURL, "/") + "/api/mills/safety/crash-lease"
	var lastErr error
	// A transport failure may occur after the server committed the lease. Retry
	// once with the same request_id; the endpoint returns the same token.
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return CrashLease{}, redactedSecretError(err, h.cfg.AdminToken)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
		resp, authority, err := h.doOperatorRequest(req)
		if err != nil {
			if ctx.Err() != nil {
				lastErr = fmt.Errorf("acquire crash lease: %w", ctx.Err())
			} else {
				lastErr = fmt.Errorf("acquire crash lease: %w", redactedSecretError(err, h.cfg.AdminToken))
			}
			if errors.Is(err, ErrOperatorAuthority) {
				return CrashLease{}, lastErr
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		body, readErr := readSafetyEndpointBody("acquire crash lease", resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = redactedSecretError(readErr, h.cfg.AdminToken)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return CrashLease{}, fmt.Errorf("acquire crash lease: unexpected HTTP status %d", resp.StatusCode)
		}
		var lease CrashLease
		if err := json.Unmarshal(body, &lease); err != nil {
			return CrashLease{}, fmt.Errorf("decode crash lease: %w", err)
		}
		observedAt := time.Now().UTC()
		if lease.Token == "" || lease.RequestID != requestID || lease.RunID != runID || lease.SpawnID != spawnID ||
			!lease.ExpiresAt.After(observedAt) {
			return CrashLease{}, fmt.Errorf(
				"invalid crash lease response: token_present=%t request_id_match=%t run_id_match=%t spawn_id_match=%t expiry_valid=%t",
				lease.Token != "", lease.RequestID == requestID, lease.RunID == runID,
				lease.SpawnID == spawnID, lease.ExpiresAt.After(observedAt))
		}
		lease.ObservedAt = observedAt
		lease.Authority = authority
		return lease, nil
	}
	return CrashLease{}, lastErr
}

const (
	crashLeaseReleaseTimeout      = 30 * time.Second
	crashLeaseReleaseInitialDelay = 100 * time.Millisecond
	crashLeaseReleaseMaxDelay     = 2 * time.Second
)

func (h *Harness) releaseCrashLeaseOnce(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		strings.TrimRight(h.cfg.OperatorURL, "/")+"/api/mills/safety/crash-lease/"+url.PathEscape(token), nil)
	if err != nil {
		return false, redactedSecretError(err, token, h.cfg.AdminToken)
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	resp, _, err := h.doOperatorRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("release crash lease: %w", ctx.Err())
		}
		return true, fmt.Errorf("release crash lease: %w", redactedSecretError(err, token, h.cfg.AdminToken))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	retryable := resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode <= 599
	body, readErr := readSafetyEndpointBody("release crash lease", resp.Body)
	if readErr != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("release crash lease: %w", ctx.Err())
		}
		return retryable, fmt.Errorf("release crash lease: %d: read response: %w", resp.StatusCode,
			redactedSecretError(readErr, token, h.cfg.AdminToken))
	}
	_ = body
	err = fmt.Errorf("release crash lease: unexpected HTTP status %d", resp.StatusCode)
	return retryable, err
}

// ReleaseCrashLease fails closed until the operator confirms the lease is
// absent. A replacement can be Kubernetes Ready before its external route is
// ready, so transport failures and 5xx responses are retried within a hard
// deadline. Other statuses (notably authorization/token failures) are terminal.
func (h *Harness) ReleaseCrashLease(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(ctx, crashLeaseReleaseTimeout)
	defer cancel()

	delay := crashLeaseReleaseInitialDelay
	var lastErr error
	for {
		retry, err := h.releaseCrashLeaseOnce(releaseCtx, token)
		if err == nil {
			return nil
		}
		if !retry {
			return err
		}
		lastErr = err
		if releaseCtx.Err() != nil {
			return fmt.Errorf("release crash lease not confirmed after %v: %w", lastErr, releaseCtx.Err())
		}
		if err := poll.WaitWithContext(releaseCtx, delay); err != nil {
			return fmt.Errorf("release crash lease not confirmed after %v: %w", lastErr, err)
		}
		delay *= 2
		if delay > crashLeaseReleaseMaxDelay {
			delay = crashLeaseReleaseMaxDelay
		}
	}
}

const minimumCrashLeaseRemaining = 45 * time.Second

// RenewCrashLease repeats the server-side proof and requires enough remaining
// TTL for the immediately following bounded Kubernetes delete.
func (h *Harness) RenewCrashLease(ctx context.Context, token string) (CrashLease, error) {
	if strings.TrimSpace(token) == "" {
		return CrashLease{}, errors.New("crash lease token is required")
	}
	endpoint := strings.TrimRight(h.cfg.OperatorURL, "/") + "/api/mills/safety/crash-lease/" +
		url.PathEscape(token) + "/renew"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return CrashLease{}, redactedSecretError(err, token, h.cfg.AdminToken)
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	resp, authority, err := h.doOperatorRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return CrashLease{}, fmt.Errorf("renew crash lease: %w", ctx.Err())
		}
		return CrashLease{}, fmt.Errorf("renew crash lease: %w",
			redactedSecretError(err, token, h.cfg.AdminToken))
	}
	defer resp.Body.Close()
	body, err := readSafetyEndpointBody("renew crash lease", resp.Body)
	if err != nil {
		return CrashLease{}, redactedSecretError(err, token, h.cfg.AdminToken)
	}
	if resp.StatusCode != http.StatusOK {
		return CrashLease{}, fmt.Errorf("renew crash lease: unexpected HTTP status %d", resp.StatusCode)
	}
	var lease CrashLease
	if err := json.Unmarshal(body, &lease); err != nil {
		return CrashLease{}, fmt.Errorf("decode renewed crash lease: %w", err)
	}
	observedAt := time.Now().UTC()
	if lease.Token != token || lease.RequestID == "" || lease.RunID == "" || lease.SpawnID == "" {
		return CrashLease{}, fmt.Errorf(
			"invalid renewed crash lease response: token_match=%t request_id_present=%t run_id_present=%t spawn_id_present=%t",
			lease.Token == token, lease.RequestID != "", lease.RunID != "", lease.SpawnID != "")
	}
	if remaining := lease.ExpiresAt.Sub(observedAt); remaining < minimumCrashLeaseRemaining {
		return CrashLease{}, fmt.Errorf("renewed crash lease has only %s remaining, need at least %s",
			remaining.Round(time.Millisecond), minimumCrashLeaseRemaining)
	}
	lease.ObservedAt = observedAt
	lease.Authority = authority
	return lease, nil
}

func validateCanaryRunDetail(runID, agentType, wantTemplateVersion string, detail RunDetail) error {
	if detail.Run.ID != runID || detail.Run.Engine != "imperative" ||
		detail.Run.Template != workflow.CanaryTemplateName ||
		detail.Run.TemplateVersion != wantTemplateVersion ||
		detail.Run.InterpreterVersion != workflow.HostInterpreterVersion ||
		detail.Run.AgentType != agentType {
		return fmt.Errorf("run %q has unexpected canary identity (want template version %q): %+v", runID, wantTemplateVersion, detail.Run)
	}
	return nil
}

// ValidateCanaryRun fetches a server-authored run summary and proves that an
// attached recovery targets the exact canary and agent identity selected by
// the caller.
func (h *Harness) ValidateCanaryRun(ctx context.Context, runID, agentType string) error {
	if err := ValidateAgentType(agentType); err != nil {
		return err
	}
	detail, err := h.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	return validateCanaryRunDetail(runID, agentType, h.expectedCanaryTemplateVersion(), detail)
}

// expectedCanaryTemplateVersion pins the launch mode: a merging gate must not
// silently accept a non-merging run or vice versa.
func (h *Harness) expectedCanaryTemplateVersion() string {
	if h.cfg.Merging {
		return workflow.CanaryMergingTemplateVersion
	}
	return workflow.CanaryTemplateVersion
}

func (h *Harness) recoverCanaryLaunch(ctx context.Context, runID, agentType string, launchErr error) (string, error) {
	if err := h.ValidateCanaryRun(ctx, runID, agentType); err == nil {
		h.cfg.Logf("canary launch response was ambiguous; recovered exact run_id=%s", runID)
		return runID, nil
	} else {
		return "", fmt.Errorf("%w; launch recovery rejected: %v", launchErr, err)
	}
}

// LaunchCanary POSTs a caller-owned stable run and agent identity, then GETs
// the committed run to prove the deployed template and interpreter versions
// before the harness permits either crash. If the response is lost after the
// server commits, that same exact GET recovers the durable canary instead of
// generating a second identity.
func (h *Harness) LaunchCanary(ctx context.Context, requestedRunID, agentType string) (string, error) {
	return h.launchCanary(ctx, requestedRunID, agentType, nil)
}

// LaunchCanaryWithRequestObserver is the full-gate launch path. observe is
// invoked with the request timestamp immediately before the HTTP transport is
// allowed to issue the POST, so serialized evidence can order the pre-launch
// namespace watch against the mutation boundary.
func (h *Harness) LaunchCanaryWithRequestObserver(
	ctx context.Context,
	requestedRunID, agentType string,
	observe func(time.Time),
) (string, error) {
	return h.launchCanary(ctx, requestedRunID, agentType, observe)
}

func (h *Harness) launchCanary(
	ctx context.Context,
	requestedRunID, agentType string,
	observe func(time.Time),
) (string, error) {
	if strings.TrimSpace(requestedRunID) == "" {
		return "", errors.New("canary launch requires a caller-owned run id")
	}
	if err := ValidateAgentType(agentType); err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{"run_id": requestedRunID, "agent_type": agentType, "merging": h.cfg.Merging})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(h.cfg.OperatorURL, "/")+"/api/mills/workflow/canary", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	}
	if observe != nil {
		observe(time.Now().UTC())
	}
	resp, _, err := h.doOperatorRequest(req)
	if err != nil {
		if errors.Is(err, ErrOperatorAuthority) {
			return "", fmt.Errorf("canary POST: %w", err)
		}
		return h.recoverCanaryLaunch(ctx, requestedRunID, agentType, fmt.Errorf("canary POST: %w", err))
	}
	defer resp.Body.Close()
	body, readErr := readSafetyEndpointBody("canary POST", resp.Body)
	if readErr != nil {
		return h.recoverCanaryLaunch(ctx, requestedRunID, agentType, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("canary POST: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		ID        string `json:"id"`
		AgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("canary POST: decode body %q: %w", string(body), err)
	}
	if out.ID != requestedRunID || out.AgentType != agentType {
		return "", fmt.Errorf("canary POST: response identity id=%q agent_type=%q, want id=%q agent_type=%q",
			out.ID, out.AgentType, requestedRunID, agentType)
	}
	if err := h.ValidateCanaryRun(ctx, requestedRunID, agentType); err != nil {
		return "", fmt.Errorf("canary POST: validate committed run: %w", err)
	}
	h.cfg.Logf("canary launched: run_id=%s", out.ID)
	return out.ID, nil
}

// GetRun fetches the run detail from the operator journal API.
func (h *Harness) GetRun(ctx context.Context, runID string) (RunDetail, error) {
	var d RunDetail
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.freshOperatorURL("/api/mills/workflow/runs/"+url.PathEscape(runID)), nil)
	if err != nil {
		return d, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, authority, err := h.doOperatorRequest(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	body, err := readSafetyEndpointBody("GET run "+runID, resp.Body)
	if err != nil {
		return d, err
	}
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("GET run %s: %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("GET run %s: decode: %w", runID, err)
	}
	d.OperatorAuthority = authority
	return d, nil
}

// FailRun terminates a still-running/paused canary after a harness failure so
// the workflow flag can be closed without leaving durable active work. It is
// idempotent for already-terminal runs.
func (h *Harness) FailRun(ctx context.Context, runID, reason string) error {
	detail, err := h.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if detail.Run.State != "running" && detail.Run.State != "paused" {
		return nil
	}
	body := strings.NewReader(fmt.Sprintf(`{"reason":%s}`, strconv.Quote(reason)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(h.cfg.OperatorURL, "/")+"/api/mills/workflow/runs/"+url.PathEscape(runID)+"/fail", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	}
	resp, _, err := h.doOperatorRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := readSafetyEndpointBody("fail run "+runID, resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fail run %s: %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

// StopSpawn asks the restart-stable mobile-hud route to terminalize the exact
// spawn. Workflow /fail alone only fences the journal; the exec-driven agent
// and its pod are owned by mobile-hud and must be stopped separately.
func (h *Harness) StopSpawn(ctx context.Context, spawnID string) error {
	if strings.TrimSpace(h.cfg.HudURL) == "" || strings.TrimSpace(h.cfg.HudAdminToken) == "" {
		return fmt.Errorf("mobile-hud cleanup is not configured")
	}
	endpoint := strings.TrimRight(h.cfg.HudURL, "/") + "/api/agent/spawn/" + url.PathEscape(spawnID) + "/stop"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Admin-Token", h.cfg.HudAdminToken)
	req.Header.Set("Authorization", "Bearer "+h.cfg.HudAdminToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("stop spawn %s: %w", spawnID, err)
	}
	defer resp.Body.Close()
	body, err := readSafetyEndpointBody("stop spawn "+spawnID, resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stop spawn %s: %d: %s", spawnID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// CleanupRun fences the workflow, stops every exact spawn identity attributable
// to its deterministic branch, and verifies both ConfigMap and pod state are
// terminal before admission may be reopened. It keeps discovering identities
// while polling so a spawn that races an early /fail cannot escape cleanup.
func (h *Harness) CleanupRun(ctx context.Context, runID, knownSpawnID, reason string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("cleanup run id is required")
	}
	cleanup := h
	if h.cfg.RequireAuthorityBinding {
		var err error
		cleanup, err = h.cleanupMitigationHarness(ctx, runID)
		if err != nil {
			return err
		}
	}
	return cleanup.cleanupRun(ctx, runID, knownSpawnID, reason)
}

func (h *Harness) cleanupRun(ctx context.Context, runID, knownSpawnID, reason string) error {
	known := make(map[string]struct{})
	if knownSpawnID != "" {
		known[knownSpawnID] = struct{}{}
	}
	deadline := time.Now().Add(2 * time.Minute)
	lastDetail := "cleanup has not observed state"
	terminalSamples := 0
	for {
		workflowTerminal := false
		detail, runErr := h.GetRun(ctx, runID)
		if errors.Is(runErr, ErrOperatorAuthority) {
			return cleanupAuthorityError("workflow read during mitigation", runErr)
		}
		if runErr == nil {
			workflowTerminal = detail.Run.State != "running" && detail.Run.State != "paused"
			if step, ok := FindAgentStep(detail); ok {
				if identity, err := DeriveSpawnIdentity(runID, step); err == nil {
					known[identity.SpawnID] = struct{}{}
				} else {
					lastDetail = "derive cleanup spawn identity: " + err.Error()
				}
			}
			if !workflowTerminal {
				if err := h.FailRun(ctx, runID, reason); err != nil {
					if errors.Is(err, ErrOperatorAuthority) {
						return cleanupAuthorityError("workflow fail during mitigation", err)
					}
					lastDetail = "fail workflow: " + err.Error()
				}
			}
		} else {
			lastDetail = "read workflow: " + runErr.Error()
		}

		allSpawns, allErr := h.getSpawnStateSnapshot(ctx, "")
		runSpawnsOK := false
		if allErr != nil {
			lastDetail = "read spawn state: " + allErr.Error()
		} else {
			if runSpawns, err := h.getSpawnStateSnapshot(ctx, runID); err == nil {
				runSpawnsOK = true
				for _, id := range runSpawns.RecordIDs {
					known[id] = struct{}{}
				}
			} else {
				lastDetail = "read run spawn state: " + err.Error()
			}
		}

		recordsTerminal, exactPodsTerminal := allErr == nil && runSpawnsOK, true
		var missingRecords []string
		for spawnID := range known {
			status, recordExists := allSpawns.Statuses[spawnID]
			if !recordExists {
				missingRecords = append(missingRecords, spawnID)
			} else if !isTerminalSpawnStatus(status) {
				recordsTerminal = false
			}
			concurrent, _, err := h.CountSpawnPods(ctx, spawnID)
			if err != nil {
				exactPodsTerminal = false
				lastDetail = "read exact cleanup pod: " + err.Error()
				continue
			}
			if concurrent > 0 {
				exactPodsTerminal = false
			}
			if (recordExists && !isTerminalSpawnStatus(status)) || concurrent > 0 {
				if err := h.StopSpawn(ctx, spawnID); err != nil {
					lastDetail = err.Error()
				}
			}
		}
		activePods, podsErr := h.activeSpawnPodNames(ctx)
		if podsErr != nil {
			lastDetail = "read active cleanup pods: " + podsErr.Error()
		}
		quiescence, quiescenceErr := h.quiescence(ctx)
		if errors.Is(quiescenceErr, ErrOperatorAuthority) {
			return cleanupAuthorityError("quiescence read during mitigation", quiescenceErr)
		}
		fleetIdle := quiescenceErr == nil && quiescence.Quiescent &&
			quiescence.Counts.unrelatedIdle(0) && quiescence.InMemory.idle()
		if quiescenceErr != nil {
			lastDetail = "read cleanup quiescence: " + quiescenceErr.Error()
		}
		if runErr == nil && allErr == nil && runSpawnsOK && podsErr == nil && workflowTerminal &&
			recordsTerminal && exactPodsTerminal && len(activePods) == 0 && fleetIdle {
			terminalSamples++
			if terminalSamples >= 2 {
				return nil
			}
		} else {
			terminalSamples = 0
		}
		if runErr == nil && allErr == nil && podsErr == nil && quiescenceErr == nil {
			lastDetail = fmt.Sprintf("workflow=%s records_terminal=%t missing_records=%v exact_pods_terminal=%t fleet_idle=%t active_pods=%v known_spawns=%v terminal_samples=%d",
				detail.Run.State, recordsTerminal, missingRecords, exactPodsTerminal, fleetIdle, activePods, mapKeys(known), terminalSamples)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %s cleanup did not reach a verified terminal state within 2m: %s", runID, lastDetail)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("run %s cleanup interrupted: %w (%s)", runID, ctx.Err(), lastDetail)
		case <-time.After(h.cfg.PollInterval):
		}
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// AwaitPendingSpawn polls until the run's agent step exists (the
// record-before-dispatch row) and returns it. This is step 2 of §3.3. The
// row's spawn_id is usually EMPTY at this point (a healthy dispatch records
// it only on completion) — callers derive the identity via
// DeriveSpawnIdentity.
func (h *Harness) AwaitPendingSpawn(ctx context.Context, runID string) (StepView, error) {
	deadline := time.Now().Add(h.cfg.StepTimeout)
	for {
		d, err := h.GetRun(ctx, runID)
		if errors.Is(err, ErrOperatorAuthority) {
			return StepView{}, fmt.Errorf("await spawn: %w", err)
		} else if err != nil {
			h.cfg.Logf("await spawn: %v (retrying)", err)
		} else if st, ok := FindAgentStep(d); ok {
			h.cfg.Logf("agent step live: step_key=%s spawn_id=%q status=%s", st.StepKey, st.SpawnID, st.Status)
			return st, nil
		}
		if time.Now().After(deadline) {
			return StepView{}, fmt.Errorf("no spawn step within %s", h.cfg.StepTimeout)
		}
		select {
		case <-ctx.Done():
			return StepView{}, ctx.Err()
		case <-time.After(h.cfg.PollInterval):
		}
	}
}

// AwaitTerminal starts recovery-only process observation immediately when
// called. Full S1c execution uses AwaitTerminalWithProcessObserver with an
// observer that was started synchronously before CRASH A deletion.
func (h *Harness) AwaitTerminal(ctx context.Context, runID, spawnID string, ev *Evidence) error {
	if ev.CrashBAt.IsZero() {
		return h.awaitTerminal(ctx, runID, spawnID, ev, nil)
	}
	observer, err := h.StartCanaryProcessObservation(ctx, spawnID, ev.CanaryHoldInitial, ev.CrashBAt)
	if err != nil {
		if observer != nil {
			return errors.Join(err, observer.Record(ev))
		}
		appendObservationError(ev, err.Error())
		return err
	}
	return h.AwaitTerminalWithProcessObserver(ctx, runID, spawnID, ev, observer)
}

// AwaitTerminalWithProcessObserver carries the CRASH-A observer through both
// replacements, workflow terminalization, and exact spawn-pod disappearance.
func (h *Harness) AwaitTerminalWithProcessObserver(
	ctx context.Context,
	runID, spawnID string,
	ev *Evidence,
	observer *CanaryProcessObserver,
) error {
	if !ev.CrashBAt.IsZero() && observer == nil {
		err := errors.New("crash-window process observer is required")
		appendObservationError(ev, err.Error())
		return err
	}
	terminalErr := h.awaitTerminal(ctx, runID, spawnID, ev, observer)
	if observer == nil {
		return terminalErr
	}
	return errors.Join(terminalErr, observer.StopAndRecord(ev))
}

func (h *Harness) awaitTerminal(
	ctx context.Context,
	runID, spawnID string,
	ev *Evidence,
	observer *CanaryProcessObserver,
) error {
	deadline := time.Now().Add(h.cfg.TerminalTimeout)
	// CountSpawnPods is also used by the pre-crash gate. Fold those retained
	// UID observations before taking the first post-crash sample so a pod that
	// was deleted and recreated under the same name cannot disappear from the
	// final proof.
	mergeSpawnPodIncarnations(ev, h.observedSpawnPodIncarnations(spawnID))
	for {
		if observer != nil {
			if err := observer.Record(ev); err != nil {
				return fmt.Errorf("post-CRASH B process observation: %w", err)
			}
		}
		if err := h.captureSpawnPodSample(ctx, spawnID, ev); err != nil {
			appendObservationError(ev, "count spawn pods: "+err.Error())
			h.cfg.Logf("count spawn pods: %v (proof marked incomplete)", err)
		}
		if err := h.CaptureSpawnState(ctx, ev); err != nil {
			appendObservationError(ev, "capture spawn state: "+err.Error())
			h.cfg.Logf("capture spawn state: %v (proof marked incomplete)", err)
		}
		if pods, err := h.activeSpawnPodNames(ctx); err == nil {
			ev.FinalActiveSpawnPodNames = pods
		} else {
			appendObservationError(ev, "capture active spawn pods: "+err.Error())
			h.cfg.Logf("capture active spawn pods: %v (proof marked incomplete)", err)
		}
		d, err := h.GetRun(ctx, runID)
		if err == nil {
			ev.Final = d
			if d.Run.State != "running" {
				// A terminal journal read is followed by a mandatory fresh durable
				// record + exact-name/UID pod observation. Stale slices from an
				// earlier sample can never establish absence or a single incarnation.
				if err := h.CaptureSpawnState(ctx, ev); err != nil {
					appendObservationError(ev, "final spawn state: "+err.Error())
					return fmt.Errorf("final spawn-state observation: %w", err)
				}
				if err := h.captureSpawnPodSample(ctx, spawnID, ev); err != nil {
					appendObservationError(ev, "final exact spawn pod: "+err.Error())
					return fmt.Errorf("final exact spawn-pod observation: %w", err)
				}
				pods, err := h.activeSpawnPodNames(ctx)
				if err != nil {
					appendObservationError(ev, "final active spawn pods: "+err.Error())
					return fmt.Errorf("final active-pod observation: %w", err)
				}
				ev.FinalActiveSpawnPodNames = pods
				if observer != nil {
					if err := observer.Record(ev); err != nil {
						return fmt.Errorf("post-CRASH B process observation: %w", err)
					}
				}
				// A terminal workflow is not enough: wait briefly for the durable
				// spawn record and exact pod to disappear so a driver-lost zombie
				// or replacement exec cannot pass.
				settleDeadline := time.Now().Add(2 * time.Minute)
				if deadline.Before(settleDeadline) {
					settleDeadline = deadline
				}
				for (len(ev.FinalActiveSpawnRecordIDs) > 0 || len(ev.FinalActiveSpawnPodNames) > 0 ||
					hasOpenPostCrashProcessObservation(ev)) && time.Now().Before(settleDeadline) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(h.cfg.PollInterval):
					}
					if err := h.CaptureSpawnState(ctx, ev); err != nil {
						appendObservationError(ev, "settle spawn state: "+err.Error())
						return fmt.Errorf("settle spawn-state observation: %w", err)
					}
					if err := h.captureSpawnPodSample(ctx, spawnID, ev); err != nil {
						appendObservationError(ev, "settle exact spawn pod: "+err.Error())
						return fmt.Errorf("settle exact spawn-pod observation: %w", err)
					}
					if pods, err := h.activeSpawnPodNames(ctx); err == nil {
						ev.FinalActiveSpawnPodNames = pods
					} else {
						appendObservationError(ev, "settle active spawn pods: "+err.Error())
						return fmt.Errorf("settle active-pod observation: %w", err)
					}
					if observer != nil {
						if err := observer.Record(ev); err != nil {
							return fmt.Errorf("post-CRASH B process observation: %w", err)
						}
					}
				}
				h.cfg.Logf("run terminal: state=%s steps=%d active_records=%d active_pods=%d",
					d.Run.State, len(d.Steps), len(ev.FinalActiveSpawnRecordIDs), len(ev.FinalActiveSpawnPodNames))
				if len(ev.FinalActiveSpawnRecordIDs) > 0 || len(ev.FinalActiveSpawnPodNames) > 0 ||
					hasOpenPostCrashProcessObservation(ev) {
					return fmt.Errorf("run %s terminal but spawn state remains active: records=%v pods=%v process_observation_open=%t",
						runID, ev.FinalActiveSpawnRecordIDs, ev.FinalActiveSpawnPodNames,
						hasOpenPostCrashProcessObservation(ev))
				}
				if !ev.CrashBAt.IsZero() {
					if err := validatePostCrashProcessEvidence(*ev); err != nil {
						return fmt.Errorf("post-CRASH B process evidence: %w", err)
					}
				}
				return nil
			}
			h.cfg.Logf("run state=%s steps=%d (waiting)", d.Run.State, len(d.Steps))
		} else if errors.Is(err, ErrOperatorAuthority) {
			return fmt.Errorf("await terminal: %w", err)
		} else {
			h.cfg.Logf("await terminal: %v (retrying)", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %s not terminal within %s (last state %q)",
				runID, h.cfg.TerminalTimeout, ev.Final.Run.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.cfg.PollInterval):
		}
	}
}

func hasOpenPostCrashProcessObservation(ev *Evidence) bool {
	return !ev.CrashBAt.IsZero() && ev.PostCrashProcessObservedEnd.IsZero()
}

func (h *Harness) captureSpawnPodSample(ctx context.Context, spawnID string, ev *Evidence) error {
	concurrent, names, err := h.CountSpawnPods(ctx, spawnID)
	if err != nil {
		return err
	}
	if concurrent > ev.MaxConcurrentSpawnPods {
		ev.MaxConcurrentSpawnPods = concurrent
	}
	for _, name := range names {
		if !contains(ev.TotalSpawnPodNames, name) {
			ev.TotalSpawnPodNames = append(ev.TotalSpawnPodNames, name)
		}
	}
	mergeSpawnPodIncarnations(ev, h.observedSpawnPodIncarnations(spawnID))
	return nil
}

func appendObservationError(ev *Evidence, message string) {
	if !contains(ev.ObservationErrors, message) {
		ev.ObservationErrors = append(ev.ObservationErrors, message)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
