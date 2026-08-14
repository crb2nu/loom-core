package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/worker"
)

// HUDSpawnConfig captures the connection settings for the loom HUD's
// mobile API. The operator runs in-cluster; the HUD usually runs on the
// developer's laptop OR cluster-deployed depending on the topology. In
// either case the operator reaches it via HTTP with a bearer token
// configured as HUD_MOBILE_OPERATOR_TOKEN on the HUD side.
type HUDSpawnConfig struct {
	// BaseURL is the HUD's HTTP base, e.g.
	// "http://hud.loom-system.svc.cluster.local:8090". Trailing slash
	// is tolerated.
	BaseURL string
	// Token is the mobile bearer token. Sent as "Authorization: Bearer <token>".
	Token string
	// PollInterval is how often the spawn detail endpoint is polled
	// for terminal status. Defaults to 5s.
	PollInterval time.Duration
	// PollDeadline caps total wait. Defaults to 30 minutes.
	PollDeadline time.Duration
	// Timeout caps any individual HTTP call. Default 30s.
	Timeout time.Duration
	// MaxRetries controls transient HTTP retries for the HUD spawn API.
	// Defaults to 6 so short Kubernetes rollouts do not burn Mills stage
	// attempts before a replacement HUD pod becomes ready.
	MaxRetries int
	// RetryBaseDelay is the initial retry delay. Defaults to 1s.
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps exponential retry delay. Defaults to 5s.
	RetryMaxDelay time.Duration
	// GitRunner runs `git` commands in an operator-readable checkout
	// (SpawnRequest.WorkingDir: the run's worktree when allocated, else
	// the operator-local clone SpawnWorker falls back to) to capture the
	// cumulative unified diff + commit messages once the spawn reaches a
	// terminal state. The spawn pod's own filesystem is never visible to
	// the operator — the capture fetches the pod's pushed commits from
	// origin instead. Defaults to execCommandRunner{} (real os/exec).
	// Tests inject a fake.
	GitRunner CommandRunner
	// MaxDiffBytes caps how much of the working-tree diff we serialize
	// into SpawnResponse.DiffPatch. The rubric prompt has its own 8 KiB
	// cap; we cap higher here so secret-scan and other downstream gates
	// can see more context. Defaults to 32 KiB.
	MaxDiffBytes int
	// MaxCommitMessagesBytes caps the joined byte length of
	// SpawnResponse.CommitMessages. Defaults to 8 KiB.
	MaxCommitMessagesBytes int
	// Logger receives the structured cumulative-git-capture event emitted
	// after every terminal spawn. A skipped or failed capture is logged at
	// WARN with a machine-readable reason; a successful one at DEBUG.
	// Defaults to slog.Default().
	//
	// Before this existed the capture had eight silent `return` paths and
	// no telemetry, so a production operator could not tell "the branch is
	// genuinely empty" from "the capture never ran" — both reached the
	// gates as zero files and an empty diff (issue #224).
	Logger *slog.Logger
}

// HUDSpawnClient implements pipeline.SpawnClient against the HUD mobile
// API at /api/mobile/v1/agent/spawn. The flow is:
//
//	POST /spawn with the spawn.Request body → returns spawn_id immediately
//	GET /spawn/{id} repeatedly until status is terminal (completed/failed/stopped)
//	Map the final state.Telemetry into pipeline.SpawnResponse
type HUDSpawnClient struct {
	cfg  HUDSpawnConfig
	http *httpclient.Client
}

// Default caps on captured diff/commit-message content. Chosen to match
// gateInputFor + composePrompt expectations: the rubric template
// re-truncates the diff at 8 KiB before sending to the judge; we keep
// 32 KiB here so secret-scan and other gates that read the raw
// SpawnResponse.DiffPatch see more context.
const (
	defaultMaxDiffBytes           = 32 * 1024
	defaultMaxCommitMessagesBytes = 8 * 1024
)

// NewHUDSpawnClient validates config and returns a ready client.
func NewHUDSpawnClient(cfg HUDSpawnConfig) (*HUDSpawnClient, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("hud spawn: BaseURL required")
	}
	if cfg.Token == "" {
		return nil, errors.New("hud spawn: Token required")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.PollDeadline == 0 {
		cfg.PollDeadline = 30 * time.Minute
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 6
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = time.Second
	}
	if cfg.RetryMaxDelay == 0 {
		cfg.RetryMaxDelay = 5 * time.Second
	}
	if cfg.GitRunner == nil {
		cfg.GitRunner = execCommandRunner{}
	}
	if cfg.MaxDiffBytes <= 0 {
		cfg.MaxDiffBytes = defaultMaxDiffBytes
	}
	if cfg.MaxCommitMessagesBytes <= 0 {
		cfg.MaxCommitMessagesBytes = defaultMaxCommitMessagesBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	hcfg := httpclient.DefaultConfig()
	hcfg.Timeout = cfg.Timeout
	hcfg.MaxRetries = cfg.MaxRetries
	hcfg.RetryBaseDelay = cfg.RetryBaseDelay
	hcfg.RetryMaxDelay = cfg.RetryMaxDelay
	c := httpclient.New(hcfg)
	return &HUDSpawnClient{cfg: cfg, http: c}, nil
}

// SetTransport is for tests.
func (c *HUDSpawnClient) SetTransport(rt http.RoundTripper) {
	c.http.HTTP().Transport = rt
}

// hudSpawnRequestBody mirrors the subset of internal/spawn.Request the
// operator needs to populate. We keep it as a local typed struct rather
// than importing internal/spawn (to avoid pulling the HUD's internal
// package into the operator's dependency tree).
type hudSpawnRequestBody struct {
	AgentType string `json:"agent_type"`
	// Model is the vendor-native LLM model id (e.g. "gpt-5.6-terra") the spawn
	// pod's agent CLI should run. Populated from pipeline.SpawnRequest.AgentModel
	// (policy stage_models / LOOM_MILLS_SPAWN_MODEL). Omitted when empty so the
	// HUD spawn server applies its own vendor default (SPAWN_CODEX_MODEL /
	// resolveCodexModel for codex) — legacy requests are byte-identical on the
	// wire. Decoded server-side into spawn.Request.Model.
	Model           string            `json:"model,omitempty"`
	Namespace       string            `json:"namespace"`
	Branch          string            `json:"branch"`
	BaseBranch      string            `json:"base_branch,omitempty"`
	TaskDescription string            `json:"task_description"`
	Project         string            `json:"project"`
	TimeoutMinutes  int               `json:"timeout_minutes,omitempty"`
	MaxCostUSD      float64           `json:"max_cost_usd,omitempty"`
	MaxTurns        int               `json:"max_turns,omitempty"`
	ParentSessionID string            `json:"parent_session_id,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	// Substrate carries the per-stage devbox backend selection from
	// policy.SubstrateForStage. The HUD spawn server translates it to
	// DEVBOX_BACKEND on the spawn pod's env so the pod's in-pod
	// mcp-devbox routes devbox_* calls onto the named substrate.
	// Slice 2c — see .loom/121-iteration-plan-…-slice2c-2026-05-27.md.
	Substrate string `json:"substrate,omitempty"`
	// CompletionHoldSeconds keeps a successful agent command attached to the
	// durable spawn with a driver-owned foreground sleep. It is omitted for all
	// normal callers and used only by bounded lifecycle fault tests.
	CompletionHoldSeconds int `json:"completion_hold_seconds,omitempty"`
	// IdempotencyKey is an OPT-IN deterministic replay key (Slice 2b).
	// When non-empty, the HUD spawn controller derives a stable spawn id
	// from it and dedupes a duplicate create into an AlreadyExists
	// re-attach (no second pod). Omitted (omitempty) when empty, so legacy
	// requests are byte-identical on the wire to pre-2b behavior and the
	// server mints the id via crypto/rand.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// hudSpawnAcceptResponse is what POST /spawn returns on success.
type hudSpawnAcceptResponse struct {
	SpawnID string `json:"spawn_id"`
	AgentID string `json:"agent_id"`
	Status  string `json:"status"`
}

// hudFileChange mirrors bridge.FileChangeEntry for the operator side.
type hudFileChange struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesRemoved int    `json:"lines_removed,omitempty"`
}

// hudSpawnTelemetry mirrors bridge.SpawnTelemetry — only the fields
// the operator actually consumes.
type hudSpawnTelemetry struct {
	TurnCount    int             `json:"turn_count"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	FileChanges  []hudFileChange `json:"file_changes,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	LastMessage  string          `json:"last_message,omitempty"`
	// CostEstimated mirrors bridge.SpawnTelemetry.CostEstimated. The HUD
	// spawn API already serialises this (cost_estimated) on the spawn
	// detail wire; the operator's subset historically dropped it, losing
	// the Codex estimated-cost marker. Decoding it here lets the Layer-1
	// Worker contract surface CostSource without any change to what the
	// HUD sends or to TotalCostUSD's value.
	CostEstimated bool `json:"cost_estimated,omitempty"`
	// TokenUsage mirrors bridge.SpawnTelemetry.TokenUsage. The HUD has
	// accumulated it per turn all along (the Claude, Codex, and Gemini
	// stdout parsers all feed SpawnTelemetryAccumulator.AddTokens) and
	// already serialises it as `token_usage` on the spawn detail wire —
	// the operator's subset simply never decoded it, which is why a
	// spawn-dispatched stage could only ever report cost USD.
	//
	// Producer coverage as of this change, by agent harness:
	//   - claude: input/output/cache_creation/cache_read, all four real
	//     (spawn_claude_parser.go reads the SDK's usage block verbatim).
	//   - codex:  input/output/cache_read. Reports NO cache-creation count,
	//     and its native `cached_input_tokens` is a subset of `input_tokens`
	//     that spawn_codex_parser.go subtracts out before accumulating.
	//   - gemini: input/output/cache_read; no cache-creation count.
	// A harness added later with no usage in its stdout contract decodes
	// as absent here and is filtered out downstream rather than logged as
	// a measured zero.
	//
	// Pointer so "the HUD sent no token_usage" (an older HUD, or a state
	// row written before the field existed) stays distinguishable from
	// "the harness measured zero of everything".
	TokenUsage *hudSpawnTokenUsage `json:"token_usage,omitempty"`
}

// hudSpawnTokenUsage mirrors bridge.SpawnTokenUsage. The four counts are
// additive, not overlapping — see pipeline.SpawnTokenUsage.
type hudSpawnTokenUsage struct {
	InputTokens         int `json:"input_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
}

// hudSpawnState mirrors spawn.State — only what the operator reads.
type hudSpawnState struct {
	SpawnID   string             `json:"spawn_id"`
	AgentID   string             `json:"agent_id"`
	Status    string             `json:"status"`
	Error     string             `json:"error,omitempty"`
	Telemetry *hudSpawnTelemetry `json:"telemetry,omitempty"`
}

// Run implements pipeline.SpawnClient.
func (c *HUDSpawnClient) Run(ctx context.Context, req pipeline.SpawnRequest) (pipeline.SpawnResponse, error) {
	if c == nil || c.http == nil {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: client not configured")
	}
	project := strings.TrimSpace(req.Project)
	if project == "" {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: SpawnRequest.Project required")
	}
	if _, ok := spawnCheckoutRoots(project); !ok {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: SpawnRequest.Project must be a relative, non-traversing project path")
	}
	if req.Branch == "" {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: SpawnRequest.Branch required")
	}
	if req.Prompt == "" {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: SpawnRequest.Prompt required")
	}

	body := hudSpawnRequestBody{
		AgentType:             agentTypeOrDefault(req.Model),
		Model:                 strings.TrimSpace(req.AgentModel),
		Namespace:             req.Namespace,
		Branch:                req.Branch,
		BaseBranch:            req.BaseBranch,
		TaskDescription:       req.Prompt,
		Project:               project,
		TimeoutMinutes:        req.BudgetMinutes,
		MaxCostUSD:            req.BudgetUSD,
		MaxTurns:              req.BudgetTurns,
		ParentSessionID:       req.ParentSessionID,
		Metadata:              buildSpawnMetadata(req),
		Substrate:             req.Substrate,
		CompletionHoldSeconds: req.CompletionHoldSeconds,
		IdempotencyKey:        req.IdempotencyKey,
	}

	spawnID, err := c.startSpawn(ctx, body)
	if err != nil {
		return pipeline.SpawnResponse{}, err
	}
	if req.OnAccepted != nil {
		if err := req.OnAccepted(spawnID); err != nil {
			return pipeline.SpawnResponse{SpawnID: spawnID}, fmt.Errorf("hud spawn: record accepted spawn: %w", err)
		}
	}

	return c.pollSpawn(ctx, spawnID, pipeline.SpawnResumeContext{
		Project:    project,
		WorkingDir: req.WorkingDir,
		BaseBranch: req.BaseBranch,
		Branch:     req.Branch,
	}, false)
}

// Resume implements pipeline.SpawnResumeClient by polling an already
// accepted HUD spawn id. This lets the Mills operator re-attach after a
// rollout instead of starting duplicate stage attempts.
//
// Callers that still hold the stage's checkout/branch coordinates should
// use ResumeWithContext: this bare form has nothing to diff, so the
// post-terminal git capture records
// GitCaptureStatusResumeNoContext and gates fall back to per-attempt
// spawn telemetry.
func (c *HUDSpawnClient) Resume(ctx context.Context, spawnID string) (pipeline.SpawnResponse, error) {
	return c.ResumeWithContext(ctx, spawnID, pipeline.SpawnResumeContext{})
}

// ResumeWithContext implements pipeline.SpawnContextResumeClient: it
// re-attaches to an accepted spawn AND still runs the post-terminal
// cumulative branch-vs-base capture, because the caller supplies the
// checkout/branch coordinates a resume would otherwise have lost.
//
// This is the production resume path. The operator re-attaches on every
// pod rollout, so before this existed the capture was dead for every
// stage that finished across a roll (issue #224).
func (c *HUDSpawnClient) ResumeWithContext(ctx context.Context, spawnID string, rc pipeline.SpawnResumeContext) (pipeline.SpawnResponse, error) {
	if c == nil || c.http == nil {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: client not configured")
	}
	if spawnID == "" {
		return pipeline.SpawnResponse{}, errors.New("hud spawn: resume spawn id required")
	}
	return c.pollSpawn(ctx, spawnID, rc, true)
}

// Stop terminates an accepted HUD spawn. A stopped or already-gone spawn is
// intentionally treated as success by the HUD endpoint, making pause retries
// safe after an operator rollout.
func (c *HUDSpawnClient) Stop(ctx context.Context, spawnID string) error {
	if c == nil || c.http == nil {
		return errors.New("hud spawn: client not configured")
	}
	if strings.TrimSpace(spawnID) == "" {
		return errors.New("hud spawn: stop spawn id required")
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/mobile/v1/agent/spawn/" + url.PathEscape(spawnID) + "/stop"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("hud spawn: build stop request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hud spawn: stop %s: %w", spawnID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("hud spawn: stop %s status %d: %s", spawnID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *HUDSpawnClient) pollSpawn(ctx context.Context, spawnID string, rc pipeline.SpawnResumeContext, resumed bool) (pipeline.SpawnResponse, error) {
	// Poll until terminal.
	pollCtx, cancel := context.WithTimeout(ctx, c.cfg.PollDeadline)
	defer cancel()
	for {
		if err := pollCtx.Err(); err != nil {
			return pipeline.SpawnResponse{
				SpawnID: spawnID,
				LogTail: fmt.Sprintf("hud spawn poll deadline (%s) exceeded", c.cfg.PollDeadline),
			}, fmt.Errorf("hud spawn: poll timeout after %s: %w", c.cfg.PollDeadline, pipeline.ErrSpawnPollTimeout)
		}
		state, err := c.getSpawnState(pollCtx, spawnID)
		if err != nil {
			if pollCtx.Err() != nil {
				return pipeline.SpawnResponse{SpawnID: spawnID}, fmt.Errorf("hud spawn: poll cancelled after %s: %w: %w", c.cfg.PollDeadline, pipeline.ErrSpawnPollTimeout, err)
			}
			return pipeline.SpawnResponse{SpawnID: spawnID}, fmt.Errorf("hud spawn %s: %w", spawnID, err)
		}
		if isTerminalSpawnStatus(state.Status) {
			resp := mapTelemetryToResponse(state)
			c.attachGitContext(ctx, &resp, rc, resumed)
			if state.Status != "completed" {
				return resp, fmt.Errorf("hud spawn %s status=%s: %s: %w", spawnID, state.Status, state.Error, pipeline.ErrSpawnTerminalFailure)
			}
			return resp, nil
		}
		select {
		case <-pollCtx.Done():
			return pipeline.SpawnResponse{SpawnID: spawnID}, fmt.Errorf("hud spawn: poll timeout after %s: %w", c.cfg.PollDeadline, pipeline.ErrSpawnPollTimeout)
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// startSpawn POSTs the spawn request and returns the new spawn id.
func (c *HUDSpawnClient) startSpawn(ctx context.Context, body hudSpawnRequestBody) (string, error) {
	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("hud spawn: marshal: %w", err)
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/mobile/v1/agent/spawn"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("hud spawn: POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("hud spawn: POST status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("hud spawn: read accept: %w", err)
	}
	var accept hudSpawnAcceptResponse
	if err := decodeHUDResponse(buf, &accept); err != nil {
		return "", fmt.Errorf("hud spawn: decode accept: %w", err)
	}
	if accept.SpawnID == "" {
		return "", errors.New("hud spawn: server returned empty spawn_id")
	}
	return accept.SpawnID, nil
}

// getSpawnState polls the detail endpoint once.
func (c *HUDSpawnClient) getSpawnState(ctx context.Context, spawnID string) (*hudSpawnState, error) {
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/mobile/v1/agent/spawn/" + spawnID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hud spawn: GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("hud spawn: GET status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("hud spawn: read state: %w", err)
	}
	var state hudSpawnState
	if err := decodeHUDResponse(buf, &state); err != nil {
		return nil, fmt.Errorf("hud spawn: decode state: %w", err)
	}
	return &state, nil
}

func decodeHUDResponse(data []byte, out any) error {
	var envelope struct {
		OK    *bool           `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.OK != nil {
		if !*envelope.OK {
			msg := envelope.Error.Message
			if msg == "" {
				msg = "mobile API returned ok=false"
			}
			if envelope.Error.Code != "" {
				return fmt.Errorf("%s: %s", envelope.Error.Code, msg)
			}
			return errors.New(msg)
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return errors.New("mobile envelope missing data")
		}
		return json.Unmarshal(envelope.Data, out)
	}
	return json.Unmarshal(data, out)
}

// agentTypeOrDefault returns a valid spawn AgentType. The pipeline
// model field allows any FlexInfer / frontier id; we map common
// shorthands to the loom spawn AgentType vocabulary.
//
// Behavior preserved from pre-Layer-1: a recognised harness (canonical
// token or known shorthand) normalises to the canonical AgentType; empty
// falls through to claude-code (the most common pipeline path); any other
// non-empty value is passed through unchanged (operators can pin a
// FlexInfer id this way). The only change is that recognition now routes
// through worker.ValidateAgentType so the shorthand table lives in one
// place.
func agentTypeOrDefault(model string) string {
	if canon, err := worker.ValidateAgentType(model); err == nil {
		return canon
	}
	// Unknown / empty falls through to claude-code — the most common
	// pipeline path. Operators can override per-stage by setting
	// SpawnWorker.Model.
	if model == "" {
		return "claude-code"
	}
	return model
}

// buildSpawnMetadata stuffs the LOOM_MILLS_* env-vars + stage id into
// the spawn metadata map so the spawn pod's env carries them through
// to the agent process.
func buildSpawnMetadata(req pipeline.SpawnRequest) map[string]string {
	out := map[string]string{}
	for k, v := range req.Env {
		out[k] = v
	}
	if req.StageID != "" {
		out["loom_mills_stage"] = req.StageID
	}
	if req.BacklogID != "" {
		out["loom_mills_backlog_id"] = req.BacklogID
	}
	return out
}

// isTerminalSpawnStatus mirrors spawn.IsTerminal (we don't import the
// internal package; the string set is part of the persisted contract).
func isTerminalSpawnStatus(s string) bool {
	switch s {
	case "completed", "failed", "stopped":
		return true
	default:
		return false
	}
}

// mapTelemetryToResponse turns a HUD spawn state into the runner's
// SpawnResponse, computing FilesChanged + LinesAdded/Removed totals.
func mapTelemetryToResponse(state *hudSpawnState) pipeline.SpawnResponse {
	resp := pipeline.SpawnResponse{
		SpawnID: state.SpawnID,
		Artifacts: map[string]any{
			"agent_id": state.AgentID,
			"status":   state.Status,
		},
	}
	if state.Telemetry == nil {
		resp.LogTail = state.Error
		return resp
	}
	tel := state.Telemetry
	resp.CostUSD = tel.TotalCostUSD
	resp.CostEstimated = tel.CostEstimated
	if u := tel.TokenUsage; u != nil {
		resp.TokenUsage = pipeline.SpawnTokenUsage{
			InputTokens:         u.InputTokens,
			OutputTokens:        u.OutputTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			CacheReadTokens:     u.CacheReadTokens,
		}
	}
	for _, fc := range tel.FileChanges {
		resp.FilesChanged = append(resp.FilesChanged, fc.Path)
		resp.LinesAdded += fc.LinesAdded
		resp.LinesRemoved += fc.LinesRemoved
	}
	logParts := []string{}
	if tel.StopReason != "" {
		logParts = append(logParts, "stop_reason="+tel.StopReason)
	}
	if tel.LastMessage != "" {
		logParts = append(logParts, "last_message="+tel.LastMessage)
	}
	if state.Error != "" {
		logParts = append(logParts, "error="+state.Error)
	}
	resp.LogTail = strings.Join(logParts, "\n")
	resp.Artifacts["turn_count"] = tel.TurnCount
	return resp
}

// attachGitContext fills cumulative diff, commit, path, and line context by
// shelling out to git in an operator-readable checkout:
// the run's worktree when one exists, otherwise the operator-local
// clone SpawnWorker passes as WorkingDir (its RepoRoot fallback).
//
// The spawn pod runs in a separate, pod-local clone (see
// internal/devbox/backend/k8s_workspace.go). The pod pushes its commits
// to origin/<branch>; the operator never sees the pod's filesystem. So
// before we diff, we `git fetch origin <branch>` into the operator's
// checkout to bring the pod's commits into a ref we can read, then
// compare origin/<baseBranch>...origin/<branch>. Pre-fix the diff was
// taken against the operator's untouched HEAD and always came back
// empty — the root cause documented in .loom/119. The base is fetched
// and read via origin/ too: the operator-local clone is fetched only at
// boot (ensureRepoRoot), so its local base ref can be days stale, and a
// stale base would fold every main commit since the last roll into the
// captured diff.
//
// The capture is cumulative by construction — origin/<branch> holds
// everything ever pushed there, not just this attempt's work — which is
// what post_implement gates must judge. This covers the
// attempt-1-errored-after-push retry shape (issue #224) that
// runner.carryForwardDiff cannot: an errored attempt records no
// successful StageOutput to carry forward.
//
// Failure modes are best-effort without trusting stale state. If either ref
// refresh fails, spawn telemetry is preserved unchanged and all cumulative
// enrichment is skipped. After successful refreshes, an individual diff, log,
// name, or numstat error degrades only that captured field; telemetry remains
// the fallback for paths and line totals. The M2.5 unparseable-retry path is
// the safety net for the legitimate nothing-changed case (the canary's no-op
// edit).
func (c *HUDSpawnClient) attachGitContext(ctx context.Context, resp *pipeline.SpawnResponse, rc pipeline.SpawnResumeContext, resumed bool) {
	if c == nil || resp == nil {
		return
	}
	project, workingDir := rc.Project, rc.WorkingDir
	baseBranch, branch := rc.BaseBranch, rc.Branch
	if workingDir == "" || baseBranch == "" || branch == "" {
		// No worktree path, base ref, or branch → the caller never told
		// us where to look. Leave Diff/Commits at zero values; gate
		// fallback path will retry via M2.5's unparseable-handler. The
		// skip is RECORDED so triage can tell an unwired capture from a
		// genuinely empty branch.
		status := gitCaptureStatusNoContext
		if resumed {
			status = gitCaptureStatusResumeNoContext
		}
		c.recordGitCapture(resp, gitCaptureOutcome{
			Status:     status,
			Reason:     missingCaptureContextReason(workingDir, baseBranch, branch),
			WorkingDir: workingDir,
			Resumed:    resumed,
		})
		return
	}
	if c.cfg.GitRunner == nil {
		c.recordGitCapture(resp, gitCaptureOutcome{
			Status:     gitCaptureStatusNoRunner,
			Reason:     "spawn client has no GitRunner configured",
			WorkingDir: workingDir,
			Resumed:    resumed,
		})
		return
	}

	// Pull the pod's pushed commits into refs the operator can read. Both
	// fetches must succeed before any cumulative context is trusted: the
	// operator checkout can retain origin refs from an earlier attempt, and
	// reading those after a failed refresh would replace current telemetry
	// with a stale branch view. The base is a separate call so a missing branch
	// ref cannot prevent the base refresh from being attempted.
	//
	// The refspec is EXPLICIT and the fetch deepened because the
	// operator-local clone is minted by ensureRepoRoot as
	// `--depth=1 --branch main`: its remote.origin.fetch covers only
	// main, so a bare `git fetch origin <branch>` dropped the commits
	// into FETCH_HEAD without ever creating refs/remotes/origin/<branch>,
	// and the depth-1 history held no merge-base for the triple-dot diff
	// — the capture came back empty on every run (issue #224; the
	// 2026-07-08 kill-test escalated finished branch work on
	// nonempty_diff exactly this way). --depth=100 bounds the deepening
	// while reaching any same-era fork point; a branch forked >100
	// commits behind both tips still misses its merge-base and degrades
	// to the old empty-capture behavior.
	//
	// A fetch failure is BEST-EFFORT for the stage (telemetry is left
	// untouched, the stage never fails on it) but never silent: the git
	// exit code and stderr are recorded and logged, because "git fetch
	// failed" and "the branch has no commits" were previously the same
	// observable state.
	baseRef := "origin/" + baseBranch
	headRef := "origin/" + branch
	if reason, ok := c.refreshCaptureRefs(ctx, workingDir, baseBranch, branch); !ok {
		c.recordGitCapture(resp, gitCaptureOutcome{
			Status:     gitCaptureStatusFetchFailed,
			Reason:     reason,
			WorkingDir: workingDir,
			BaseRef:    baseRef,
			HeadRef:    headRef,
			Resumed:    resumed,
		})
		return
	}

	diff := captureGitDiff(ctx, c.cfg.GitRunner, workingDir, baseRef, headRef, c.cfg.MaxDiffBytes)
	commits := captureGitCommitMessages(ctx, c.cfg.GitRunner, workingDir, baseRef, headRef, c.cfg.MaxCommitMessagesBytes)
	cumulative := captureGitChangedFiles(ctx, c.cfg.GitRunner, workingDir, baseRef, headRef)

	resp.DiffPatch = diff
	resp.CommitMessages = commits
	// FilesChanged and line totals must describe the complete branch diff, not
	// only the subset surfaced by the agent session's telemetry. A session can
	// report one code file while omitting another committed path (for example a
	// changelog fragment); trusting that partial view makes scope, docs, and
	// diff-size gates judge a branch state that never existed. When the
	// cumulative name capture answers with paths, it alone defines the merge
	// envelope: telemetry paths describe what the pod session TOUCHED, which
	// includes files edited and then reverted back to base (a gate-fail retry
	// removing an out-of-scope change touches that file to restore it) and
	// uncommitted pod residue — neither is on the branch, and unioning them in
	// made the scope gate false-fail a healthy retry and made FilesChanged
	// disagree with the patch's file headers, tripping fabricated_slice's
	// truncation heuristic (run PIPE-bl-daemon-hub-ws-liveness-backoff-
	// 20260810, attempt 2). Telemetry remains the fallback when the capture
	// yields no paths. Line totals likewise prefer git's cumulative numstat.
	// Name and numstat captures are intentionally independent of the unified
	// patch: a patch can fail or exceed a renderer-specific limit while git
	// can still provide the complete path and size metadata required by
	// deterministic gates.
	if len(cumulative) > 0 {
		resp.FilesChanged = mergeChangedFiles(nil, cumulative)
	} else {
		spawnRoots, _ := spawnCheckoutRoots(project)
		repoRoots := append(spawnRoots, workingDir)
		resp.FilesChanged = mergeChangedFiles(resp.FilesChanged, cumulative, repoRoots...)
	}
	if added, removed, ok := captureGitLineTotals(ctx, c.cfg.GitRunner, workingDir, baseRef, headRef); ok {
		resp.LinesAdded = added
		resp.LinesRemoved = removed
	}

	outcome := gitCaptureOutcome{
		Status:     gitCaptureStatusCaptured,
		WorkingDir: workingDir,
		BaseRef:    baseRef,
		HeadRef:    headRef,
		Files:      len(cumulative),
		DiffBytes:  len(diff),
		Commits:    len(commits),
		Resumed:    resumed,
	}
	if len(cumulative) == 0 && len(diff) == 0 {
		// Refs refreshed, git answered, and the answer was "nothing".
		// Either the branch genuinely carries no work, or an individual
		// diff/name capture errored (both degrade to empty by contract).
		// Distinct status so nonempty_diff escalations can be attributed.
		outcome.Status = gitCaptureStatusEmpty
		outcome.Reason = "git reported no changes between " + baseRef + " and " + headRef
	}
	c.recordGitCapture(resp, outcome)
}

// Git capture status values recorded under
// pipeline.GitCaptureArtifactKey on every terminal spawn response. They
// are the answer to "did the cumulative branch-vs-base capture actually
// run, and if not, why not" — the question issue #224 could not be
// answered from production because every failure path returned silently.
const (
	gitCaptureStatusCaptured        = "captured"
	gitCaptureStatusEmpty           = "captured_empty"
	gitCaptureStatusNoContext       = "skipped_no_capture_context"
	gitCaptureStatusResumeNoContext = "skipped_resume_without_capture_context"
	gitCaptureStatusNoRunner        = "skipped_no_git_runner"
	gitCaptureStatusFetchFailed     = "fetch_failed"
)

// gitCaptureOutcome is the structured record of one capture attempt.
type gitCaptureOutcome struct {
	Status     string
	Reason     string
	WorkingDir string
	BaseRef    string
	HeadRef    string
	Files      int
	DiffBytes  int
	Commits    int
	Resumed    bool
}

// refreshCaptureRefs fetches the branch and base refs into
// refs/remotes/origin/* so the triple-dot diff has both sides. It returns
// a human-readable reason (git exit code + stderr, credential-redacted)
// when either fetch fails, so the caller can record WHY the capture was
// abandoned instead of returning an indistinguishable empty result.
func (c *HUDSpawnClient) refreshCaptureRefs(ctx context.Context, workingDir, baseBranch, branch string) (string, bool) {
	_, branchStderr, branchCode, branchErr := c.cfg.GitRunner.Run(ctx, workingDir, "git", "fetch", "--depth=100", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	_, baseStderr, baseCode, baseErr := c.cfg.GitRunner.Run(ctx, workingDir, "git", "fetch", "--depth=100", "origin",
		"+refs/heads/"+baseBranch+":refs/remotes/origin/"+baseBranch)
	reasons := make([]string, 0, 2)
	if branchErr != nil || branchCode != 0 {
		reasons = append(reasons, gitFetchFailureReason("branch", branch, branchCode, branchStderr, branchErr))
	}
	if baseErr != nil || baseCode != 0 {
		reasons = append(reasons, gitFetchFailureReason("base", baseBranch, baseCode, baseStderr, baseErr))
	}
	if len(reasons) == 0 {
		return "", true
	}
	return strings.Join(reasons, "; "), false
}

func gitFetchFailureReason(kind, ref string, code int, stderr string, err error) string {
	msg := fmt.Sprintf("%s ref %q fetch failed (exit %d)", kind, ref, code)
	if err != nil {
		msg += ": " + err.Error()
	}
	if detail := redactGitOutput(stderr); detail != "" {
		msg += ": " + detail
	}
	return msg
}

// maxGitCaptureReasonBytes bounds a recorded reason so a runaway git
// stderr cannot bloat the persisted stage_results artifact blob.
const maxGitCaptureReasonBytes = 512

// redactGitOutput trims git stderr for recording. Any userinfo in a URL
// ("https://oauth2:<token>@host") is stripped: the operator's clone
// authenticates via a credential-store helper, and a helper misfire is
// exactly the failure this reason is meant to surface.
func redactGitOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for {
		at := strings.IndexByte(s, '@')
		if at < 0 {
			break
		}
		scheme := strings.LastIndex(s[:at], "://")
		if scheme < 0 {
			break
		}
		userinfo := s[scheme+3 : at]
		if strings.ContainsAny(userinfo, " \t\n/") {
			// Not a URL userinfo run — keep the text up to '@' verbatim.
			b.WriteString(s[:at+1])
			s = s[at+1:]
			continue
		}
		b.WriteString(s[:scheme+3])
		b.WriteString("***@")
		s = s[at+1:]
	}
	b.WriteString(s)
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > maxGitCaptureReasonBytes {
		out = out[:maxGitCaptureReasonBytes] + truncationMarker(len(out)-maxGitCaptureReasonBytes)
	}
	return out
}

func missingCaptureContextReason(workingDir, baseBranch, branch string) string {
	missing := make([]string, 0, 3)
	if workingDir == "" {
		missing = append(missing, "working_dir")
	}
	if baseBranch == "" {
		missing = append(missing, "base_branch")
	}
	if branch == "" {
		missing = append(missing, "branch")
	}
	return "capture coordinates unset: " + strings.Join(missing, ",")
}

// recordGitCapture pins the outcome onto the spawn response artifacts —
// which the dispatcher persists onto stage_results and the HUD renders —
// and emits the matching structured log event. A skipped or failed
// capture is a WARN because it silently downgrades every downstream gate
// to per-attempt spawn telemetry; it is never an error, because the
// capture is best-effort and must never fail the stage.
func (c *HUDSpawnClient) recordGitCapture(resp *pipeline.SpawnResponse, out gitCaptureOutcome) {
	if resp == nil {
		return
	}
	art := map[string]any{"status": out.Status, "resumed": out.Resumed}
	if out.Reason != "" {
		art["reason"] = out.Reason
	}
	if out.WorkingDir != "" {
		art["working_dir"] = out.WorkingDir
	}
	if out.BaseRef != "" {
		art["base_ref"] = out.BaseRef
	}
	if out.HeadRef != "" {
		art["head_ref"] = out.HeadRef
	}
	if out.Status == gitCaptureStatusCaptured || out.Status == gitCaptureStatusEmpty {
		art["files"] = out.Files
		art["diff_bytes"] = out.DiffBytes
		art["commits"] = out.Commits
	}
	if resp.Artifacts == nil {
		resp.Artifacts = map[string]any{}
	}
	resp.Artifacts[pipeline.GitCaptureArtifactKey] = art

	logger := c.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		"spawn_id", resp.SpawnID,
		"status", out.Status,
		"resumed", out.Resumed,
		"working_dir", out.WorkingDir,
		"base_ref", out.BaseRef,
		"head_ref", out.HeadRef,
	}
	if out.Reason != "" {
		attrs = append(attrs, "reason", out.Reason)
	}
	if out.Status == gitCaptureStatusCaptured {
		logger.Debug("mills spawn: cumulative git capture attached",
			append(attrs, "files", out.Files, "diff_bytes", out.DiffBytes)...)
		return
	}
	logger.Warn("mills spawn: cumulative git capture unavailable; gates fall back to per-attempt spawn telemetry (issue #224)", attrs...)
}

func mergeChangedFiles(telemetry, cumulative []string, repoRoots ...string) []string {
	if len(telemetry) == 0 && len(cumulative) == 0 {
		return telemetry
	}
	out := make([]string, 0, len(telemetry)+len(cumulative))
	seen := make(map[string]struct{}, len(telemetry)+len(cumulative))
	for _, raw := range telemetry {
		path := telemetryPathRelativeToRoots(raw, repoRoots...)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	for _, raw := range cumulative {
		path := normalizeChangedPath(raw)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

var spawnProjectBuckets = []string{"services", "libs", "platform", "private", "labs"}

// spawnCheckoutRoots returns every deterministic checkout root the HUD may use
// for a project. A bucket-qualified project has exactly one root. A bare name
// follows HUD resolution order: workspace root first, then each canonical
// workspace bucket. This boundary is intentionally stricter about lexical
// traversal: absolute paths, backslashes, and any parent-traversal segment are
// rejected so project metadata cannot escape /workspace.
func spawnCheckoutRoots(project string) ([]string, bool) {
	project = strings.TrimSpace(project)
	if project == "" || strings.Contains(project, "\\") || strings.HasPrefix(project, "/") {
		return nil, false
	}
	for _, segment := range strings.Split(project, "/") {
		if segment == ".." {
			return nil, false
		}
	}
	cleaned := normalizeChangedPath(project)
	if cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(filepath.FromSlash(cleaned)) {
		return nil, false
	}
	if strings.Contains(cleaned, "/") {
		return []string{"/workspace/" + cleaned}, true
	}
	roots := make([]string, 0, 1+len(spawnProjectBuckets))
	roots = append(roots, "/workspace/"+cleaned)
	for _, bucket := range spawnProjectBuckets {
		roots = append(roots, "/workspace/"+bucket+"/"+cleaned)
	}
	return roots, true
}

func normalizeChangedPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return ""
	}
	return filepath.ToSlash(cleaned)
}

// telemetryPathRelativeToRoots converts an absolute telemetry path to a stable
// repo-relative path only when lexical containment under a known checkout root
// is proven. Callers provide the deterministic spawn root and operator checkout
// because production uses different filesystems; when roots overlap, the most
// specific containing root wins. An arbitrary absolute path that merely ends
// with the same suffix as a Git path is retained: it may identify a distinct
// nested checkout or another workspace.
func telemetryPathRelativeToRoots(path string, repoRoots ...string) string {
	path = normalizeChangedPath(path)
	if path == "" || !filepath.IsAbs(filepath.FromSlash(path)) {
		return path
	}
	bestRootLen := -1
	bestRel := ""
	for _, rawRoot := range repoRoots {
		root := normalizeChangedPath(rawRoot)
		if root == "" || !filepath.IsAbs(filepath.FromSlash(root)) {
			continue
		}
		rel, err := filepath.Rel(filepath.FromSlash(root), filepath.FromSlash(path))
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if len(root) > bestRootLen {
			bestRootLen = len(root)
			bestRel = rel
		}
	}
	if bestRootLen >= 0 {
		return normalizeChangedPath(bestRel)
	}
	return path
}

// captureGitChangedFiles returns the file paths changed on headRef since
// fork from baseRef (`git diff --name-only base...head`), or nil on any
// git error (best-effort, same contract as captureGitDiff).
func captureGitChangedFiles(ctx context.Context, runner CommandRunner, workingDir, baseRef, headRef string) []string {
	if runner == nil || workingDir == "" || baseRef == "" || headRef == "" {
		return nil
	}
	stdout, _, code, err := runner.Run(ctx, workingDir, "git", "diff", "--name-only", baseRef+"..."+headRef)
	if err != nil || code != 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// captureGitLineTotals returns cumulative added/removed line counts from
// `git diff --numstat base...head`. The bool is false when git fails or emits
// malformed output so callers can retain telemetry as a best-effort fallback.
// Binary entries use "-" counts and contribute zero lines while still being
// represented in FilesChanged by captureGitChangedFiles.
func captureGitLineTotals(ctx context.Context, runner CommandRunner, workingDir, baseRef, headRef string) (added, removed int, ok bool) {
	if runner == nil || workingDir == "" || baseRef == "" || headRef == "" {
		return 0, 0, false
	}
	stdout, _, code, err := runner.Run(ctx, workingDir, "git", "diff", "--numstat", baseRef+"..."+headRef)
	if err != nil || code != 0 || strings.TrimSpace(stdout) == "" {
		return 0, 0, false
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		fields := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 3)
		if len(fields) != 3 {
			return 0, 0, false
		}
		lineAdded, valid := parseGitNumstatCount(fields[0])
		if !valid {
			return 0, 0, false
		}
		lineRemoved, valid := parseGitNumstatCount(fields[1])
		if !valid {
			return 0, 0, false
		}
		added += lineAdded
		removed += lineRemoved
	}
	return added, removed, true
}

func parseGitNumstatCount(raw string) (int, bool) {
	if raw == "-" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// captureGitDiff runs `git diff <baseRef>...<headRef>` in workingDir
// and returns the unified diff capped at maxBytes. The triple-dot form
// produces the symmetric-difference diff between base and head — i.e.
// "what changed on this branch since fork from base", which is the
// view the rubric judge needs to score code review questions.
//
// Both refs are typically origin-qualified ("origin/main",
// "origin/<branch>") after attachGitContext fetches them — the operator
// checkout's local refs are never touched by the spawn pod and go stale
// between operator rolls.
//
// On any git error (worktree missing, base ref unknown, etc.) we
// return an empty slice — never nil — so callers can distinguish
// "ran git, no changes" from "git capture was skipped entirely".
func captureGitDiff(ctx context.Context, runner CommandRunner, workingDir, baseRef, headRef string, maxBytes int) []byte {
	if runner == nil || workingDir == "" || baseRef == "" || headRef == "" {
		return nil
	}
	stdout, _, code, err := runner.Run(ctx, workingDir, "git", "diff", baseRef+"..."+headRef)
	if err != nil || code != 0 {
		// best-effort: return empty slice (not nil) so the response
		// shape is consistent.
		return []byte{}
	}
	return truncateBytes([]byte(stdout), maxBytes)
}

// captureGitCommitMessages returns the per-commit message bodies on
// headRef since fork from baseRef. Uses a NUL delimiter so
// multi-paragraph commit messages don't get mangled by newline
// splitting. Both refs are origin-qualified for HUD-spawn flows where
// the pod's commits live on origin.
func captureGitCommitMessages(ctx context.Context, runner CommandRunner, workingDir, baseRef, headRef string, maxBytes int) []string {
	if runner == nil || workingDir == "" || baseRef == "" || headRef == "" {
		return nil
	}
	stdout, _, code, err := runner.Run(ctx, workingDir, "git", "log", "--pretty=format:%B%x00", baseRef+".."+headRef)
	if err != nil || code != 0 {
		return nil
	}
	parts := strings.Split(strings.TrimRight(stdout, "\x00\n"), "\x00")
	out := make([]string, 0, len(parts))
	total := 0
	for _, p := range parts {
		msg := strings.TrimSpace(p)
		if msg == "" {
			continue
		}
		// Reserve room for joining commas in the byte budget so a single
		// runaway commit message still hits the cap.
		if maxBytes > 0 && total+len(msg) > maxBytes {
			remaining := maxBytes - total
			if remaining > 0 {
				marker := truncationMarker(len(msg) - remaining)
				out = append(out, msg[:remaining]+marker)
			}
			break
		}
		out = append(out, msg)
		total += len(msg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateBytes returns the input capped at maxBytes, appending a
// truncation marker that tells the reader exactly how much was dropped.
// maxBytes <= 0 disables truncation.
func truncateBytes(b []byte, maxBytes int) []byte {
	if maxBytes <= 0 || len(b) <= maxBytes {
		return b
	}
	dropped := len(b) - maxBytes
	marker := truncationMarker(dropped)
	out := make([]byte, 0, maxBytes+len(marker))
	out = append(out, b[:maxBytes]...)
	out = append(out, marker...)
	return out
}

func truncationMarker(dropped int) string {
	return fmt.Sprintf("\n... [truncated %d bytes]\n", dropped)
}

// Compile-time interface assertion.
var _ pipeline.SpawnClient = (*HUDSpawnClient)(nil)
