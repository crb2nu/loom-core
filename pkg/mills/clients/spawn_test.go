package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// hudFakeTransport routes HUD HTTP calls to per-method handlers so
// tests can drive POST/GET independently. The transport records every
// request so tests assert on body + auth header.
type hudFakeTransport struct {
	mu       sync.Mutex
	requests []hudRecorded
	post     func(*http.Request) (int, any)
	get      func(*http.Request) (int, any)
}

type hudRecorded struct {
	Method      string
	Path        string
	EscapedPath string
	Auth        string
	Body        string
}

func (t *hudFakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	body := ""
	if req.Body != nil {
		buf, _ := io.ReadAll(req.Body)
		body = string(buf)
	}
	t.requests = append(t.requests, hudRecorded{
		Method: req.Method, Path: req.URL.Path, EscapedPath: req.URL.EscapedPath(),
		Auth: req.Header.Get("Authorization"), Body: body,
	})
	t.mu.Unlock()

	var status int
	var payload any
	switch {
	case req.Method == http.MethodPost && t.post != nil:
		status, payload = t.post(req)
	case req.Method == http.MethodGet && t.get != nil:
		status, payload = t.get(req)
	default:
		status, payload = 404, map[string]string{"err": "no handler"}
	}
	buf, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(buf)),
		Header:     make(http.Header),
	}, nil
}

func (t *hudFakeTransport) recordedRequests() []hudRecorded {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]hudRecorded, len(t.requests))
	copy(out, t.requests)
	return out
}

func newHUDStub(t *testing.T, ft *hudFakeTransport) *HUDSpawnClient {
	t.Helper()
	c, err := NewHUDSpawnClient(HUDSpawnConfig{
		BaseURL:        "http://hud.example",
		Token:          "tok-abc",
		PollInterval:   5 * time.Millisecond,
		PollDeadline:   500 * time.Millisecond,
		MaxRetries:     1,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	c.SetTransport(ft)
	return c
}

func sampleSpawnReq() pipeline.SpawnRequest {
	return pipeline.SpawnRequest{
		Prompt:          "plan slices for BL-X",
		Model:           "claude",
		BudgetUSD:       2.0,
		BudgetTurns:     50,
		BudgetMinutes:   30,
		ParentSessionID: "session-op-1",
		StageID:         "plan_slice",
		BacklogID:       "BL-X",
		Project:         "loom-core",
		Branch:          "mills/BL-X/plan_slice",
		BaseBranch:      "main",
		Namespace:       "loom-mills",
		Env:             map[string]string{"LOOM_MILLS_RUN_ID": "PIPE-X-1"},
	}
}

// ----- Config validation -----

func TestNewHUDSpawnClient_RequiresFields(t *testing.T) {
	if _, err := NewHUDSpawnClient(HUDSpawnConfig{}); err == nil {
		t.Error("expected error for empty BaseURL")
	}
	if _, err := NewHUDSpawnClient(HUDSpawnConfig{BaseURL: "x"}); err == nil {
		t.Error("expected error for empty Token")
	}
}

// ----- POST + auth -----

func TestRun_PostsCorrectRequestAndAuth(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-99", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-99", Status: "completed",
				Telemetry: &hudSpawnTelemetry{
					TotalCostUSD: 1.23,
					FileChanges: []hudFileChange{
						{Path: "a.go", Kind: "modify", LinesAdded: 5, LinesRemoved: 2},
					},
					StopReason:  "task_complete",
					LastMessage: "done",
				},
			}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.SpawnID != "spawn-99" {
		t.Errorf("SpawnID = %q", resp.SpawnID)
	}
	if resp.CostUSD != 1.23 {
		t.Errorf("CostUSD = %v", resp.CostUSD)
	}
	if len(resp.FilesChanged) != 1 || resp.FilesChanged[0] != "a.go" {
		t.Errorf("FilesChanged = %v", resp.FilesChanged)
	}
	if resp.LinesAdded != 5 || resp.LinesRemoved != 2 {
		t.Errorf("lines wrong: +%d -%d", resp.LinesAdded, resp.LinesRemoved)
	}
	if !strings.Contains(resp.LogTail, "task_complete") {
		t.Errorf("LogTail missing stop_reason: %q", resp.LogTail)
	}

	requests := ft.recordedRequests()
	if len(requests) < 2 {
		t.Fatalf("expected POST + at least one GET, got %d requests", len(requests))
	}
	post := requests[0]
	if post.Method != http.MethodPost {
		t.Errorf("first call method = %q", post.Method)
	}
	if post.Auth != "Bearer tok-abc" {
		t.Errorf("auth header = %q", post.Auth)
	}
	if !strings.Contains(post.Path, "/api/mobile/v1/agent/spawn") {
		t.Errorf("post path = %q", post.Path)
	}
	var body hudSpawnRequestBody
	if err := json.Unmarshal([]byte(post.Body), &body); err != nil {
		t.Fatalf("decode post body: %v", err)
	}
	if body.AgentType != "claude-code" {
		t.Errorf("agent_type = %q (claude → claude-code)", body.AgentType)
	}
	if body.Project != "loom-core" {
		t.Errorf("project = %q", body.Project)
	}
	if body.Branch != "mills/BL-X/plan_slice" {
		t.Errorf("branch = %q", body.Branch)
	}
	if body.MaxCostUSD != 2.0 {
		t.Errorf("max_cost_usd = %v", body.MaxCostUSD)
	}
	if body.ParentSessionID != "session-op-1" {
		t.Errorf("parent_session_id = %q", body.ParentSessionID)
	}
	if body.Metadata["loom_mills_stage"] != "plan_slice" {
		t.Errorf("metadata.loom_mills_stage missing: %v", body.Metadata)
	}
	// Default sampleSpawnReq has empty Substrate → omitempty must drop
	// the field from the POST body so Slice 2c stays a no-op for callers
	// that have not opted into stage-substrate routing.
	if body.Substrate != "" {
		t.Errorf("substrate must be omitted when SpawnRequest.Substrate empty; got %q", body.Substrate)
	}
	if body.CompletionHoldSeconds != 0 {
		t.Errorf("completion hold must default to zero; got %d", body.CompletionHoldSeconds)
	}
	if strings.Contains(post.Body, `"completion_hold_seconds"`) {
		t.Errorf("zero completion hold must be omitted from POST body: %s", post.Body)
	}
}

// TestRun_PropagatesSubstrate covers the Slice 2c hop:
// pipeline.SpawnRequest.Substrate must land in the POST body as the
// JSON "substrate" field so the HUD spawn server can promote it to
// DEVBOX_BACKEND on the pod env. Empty-substrate omission is covered
// by TestRun_PostsCorrectRequestAndAuth above.
func TestRun_PropagatesSubstrate(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-sub", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-sub", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq()
	req.Substrate = "harvester-vm"
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}
	requests := ft.recordedRequests()
	if len(requests) < 1 {
		t.Fatalf("expected at least one request")
	}
	var body hudSpawnRequestBody
	if err := json.Unmarshal([]byte(requests[0].Body), &body); err != nil {
		t.Fatalf("decode post body: %v", err)
	}
	if body.Substrate != "harvester-vm" {
		t.Errorf("substrate = %q; want %q", body.Substrate, "harvester-vm")
	}
	// Raw JSON must include the substrate key (defensive against future
	// struct-field renames that would silently break the wire contract).
	if !strings.Contains(requests[0].Body, `"substrate":"harvester-vm"`) {
		t.Errorf("raw POST body missing substrate key: %s", requests[0].Body)
	}
}

// TestRun_PropagatesAgentModel covers the stage_models hop:
// pipeline.SpawnRequest.AgentModel must land in the POST body as the JSON
// "model" field so the HUD spawn server can pin it on `codex exec --model`.
// Empty-model omission (omitempty) keeps legacy requests byte-identical.
func TestRun_PropagatesAgentModel(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-model", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-model", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq()
	req.AgentModel = "gpt-5.6-terra"
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}
	requests := ft.recordedRequests()
	if len(requests) < 1 {
		t.Fatalf("expected at least one request")
	}
	var body hudSpawnRequestBody
	if err := json.Unmarshal([]byte(requests[0].Body), &body); err != nil {
		t.Fatalf("decode post body: %v", err)
	}
	if body.Model != "gpt-5.6-terra" {
		t.Errorf("model = %q; want %q", body.Model, "gpt-5.6-terra")
	}
	if !strings.Contains(requests[0].Body, `"model":"gpt-5.6-terra"`) {
		t.Errorf("raw POST body missing model key: %s", requests[0].Body)
	}
}

// TestRun_OmitsEmptyAgentModel guards the omitempty wire contract: an empty
// AgentModel must not add a "model" key so the spawn server keeps its vendor
// default and legacy callers stay byte-identical on the wire.
func TestRun_OmitsEmptyAgentModel(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-nomodel", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-nomodel", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq() // AgentModel unset
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}
	requests := ft.recordedRequests()
	if len(requests) < 1 {
		t.Fatalf("expected at least one request")
	}
	if strings.Contains(requests[0].Body, `"model"`) {
		t.Errorf("empty AgentModel must omit the model key: %s", requests[0].Body)
	}
}

func TestRun_PropagatesCompletionHold(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-hold", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-hold", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq()
	req.CompletionHoldSeconds = 90
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}
	requests := ft.recordedRequests()
	if len(requests) < 1 {
		t.Fatal("expected at least one request")
	}
	var body hudSpawnRequestBody
	if err := json.Unmarshal([]byte(requests[0].Body), &body); err != nil {
		t.Fatalf("decode post body: %v", err)
	}
	if body.CompletionHoldSeconds != 90 {
		t.Errorf("completion_hold_seconds = %d; want 90", body.CompletionHoldSeconds)
	}
	if !strings.Contains(requests[0].Body, `"completion_hold_seconds":90`) {
		t.Errorf("raw POST body missing completion hold: %s", requests[0].Body)
	}
}

func TestRun_RetriesTransientPostFailure(t *testing.T) {
	var postCount int32
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			if atomic.AddInt32(&postCount, 1) == 1 {
				return http.StatusServiceUnavailable, map[string]string{"error": "hud rolling"}
			}
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-after-rollout", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-after-rollout", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.SpawnID != "spawn-after-rollout" {
		t.Fatalf("spawn_id = %q", resp.SpawnID)
	}
	if got := atomic.LoadInt32(&postCount); got != 2 {
		t.Fatalf("POST attempts = %d, want 2", got)
	}
}

func TestRun_RecordsAcceptedSpawnBeforePolling(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-record", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-record", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq()
	var recorded string
	req.OnAccepted = func(spawnID string) error {
		recorded = spawnID
		if len(ft.recordedRequests()) != 1 {
			t.Fatalf("OnAccepted should run immediately after POST, before polling")
		}
		return nil
	}
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if recorded != "spawn-record" || resp.SpawnID != "spawn-record" {
		t.Fatalf("recorded=%q response=%q, want spawn-record", recorded, resp.SpawnID)
	}
}

// TestRun_SendsIdempotencyKeyWhenSet proves the OPT-IN path: when
// SpawnRequest.IdempotencyKey is non-empty, the POST body carries
// idempotency_key so the HUD controller can derive a deterministic id and
// dedupe a duplicate create into an AlreadyExists re-attach.
func TestRun_SendsIdempotencyKeyWhenSet(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-idem", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-idem", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq()
	req.IdempotencyKey = "mills/run-7/stage-implement"

	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}

	reqs := ft.recordedRequests()
	if len(reqs) == 0 || reqs[0].Method != http.MethodPost {
		t.Fatalf("expected a POST request, got %+v", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("decode POST body: %v", err)
	}
	if got, _ := body["idempotency_key"].(string); got != req.IdempotencyKey {
		t.Errorf("idempotency_key in body = %q, want %q (body=%s)", got, req.IdempotencyKey, reqs[0].Body)
	}
}

// TestRun_OmitsIdempotencyKeyWhenEmpty proves the legacy path is wire-
// identical: with no key, the field is omitted (omitempty) entirely so the
// server mints the id exactly as before.
func TestRun_OmitsIdempotencyKeyWhenEmpty(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-legacy", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-legacy", Status: "completed"}
		},
	}
	c := newHUDStub(t, ft)
	req := sampleSpawnReq() // IdempotencyKey left empty

	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}

	reqs := ft.recordedRequests()
	if len(reqs) == 0 {
		t.Fatal("expected at least one request")
	}
	if strings.Contains(reqs[0].Body, "idempotency_key") {
		t.Errorf("legacy request must omit idempotency_key, body=%s", reqs[0].Body)
	}
}

func TestRun_AcceptsMobileEnvelopeResponses(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, map[string]any{
				"ok":   true,
				"data": hudSpawnAcceptResponse{SpawnID: "spawn-envelope", Status: "creating"},
			}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, map[string]any{
				"ok": true,
				"data": hudSpawnState{
					SpawnID: "spawn-envelope",
					Status:  "completed",
					Telemetry: &hudSpawnTelemetry{
						TotalCostUSD: 0.25,
						TurnCount:    2,
					},
				},
			}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.SpawnID != "spawn-envelope" {
		t.Errorf("SpawnID = %q", resp.SpawnID)
	}
	if resp.CostUSD != 0.25 {
		t.Errorf("CostUSD = %v", resp.CostUSD)
	}
	if v, ok := resp.Artifacts["turn_count"].(int); !ok || v != 2 {
		t.Errorf("turn_count artifact = %v", resp.Artifacts["turn_count"])
	}
}

func TestDecodeHUDResponse_ReturnsEnvelopeError(t *testing.T) {
	var out hudSpawnAcceptResponse
	err := decodeHUDResponse([]byte(`{"ok":false,"error":{"code":"spawn_error","message":"boom"}}`), &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "spawn_error: boom") {
		t.Fatalf("error = %q", err.Error())
	}
}

// ----- Polling -----

func TestRun_PollsUntilTerminal(t *testing.T) {
	var pollCount int32
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-poll"}
		},
		get: func(_ *http.Request) (int, any) {
			n := atomic.AddInt32(&pollCount, 1)
			status := "running"
			if n >= 3 {
				status = "completed"
			}
			return 200, hudSpawnState{
				SpawnID: "spawn-poll", Status: status,
				Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.42, TurnCount: 12},
			}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.SpawnID != "spawn-poll" {
		t.Errorf("SpawnID = %q", resp.SpawnID)
	}
	if atomic.LoadInt32(&pollCount) < 3 {
		t.Errorf("expected at least 3 polls, got %d", pollCount)
	}
	if v, ok := resp.Artifacts["turn_count"].(int); !ok || v != 12 {
		t.Errorf("turn_count artifact = %v", resp.Artifacts["turn_count"])
	}
}

func TestResumePollsExistingSpawnWithoutPost(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			t.Fatal("resume must not POST a new spawn")
			return 500, nil
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID:   "spawn-existing",
				Status:    "completed",
				Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.11},
			}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Resume(context.Background(), "spawn-existing")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resp.SpawnID != "spawn-existing" || resp.CostUSD != 0.11 {
		t.Fatalf("resp = %+v", resp)
	}
	requests := ft.recordedRequests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v, want one GET", requests)
	}
}

func TestRun_FailedTerminalReturnsError(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-fail"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-fail", Status: "failed",
				Error:     "max_turns exceeded",
				Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.5},
			}
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err == nil {
		t.Error("expected error for failed terminal status")
	}
	// The terminal-failure sentinel lets attach-by-key callers (the workflow
	// runtime) tell "spawn can never progress" apart from a transient poll
	// error; without it a resume loop re-attaches to the dead spawn forever.
	if !errors.Is(err, pipeline.ErrSpawnTerminalFailure) {
		t.Errorf("error should wrap pipeline.ErrSpawnTerminalFailure, got: %v", err)
	}
	if resp.SpawnID != "spawn-fail" {
		t.Errorf("SpawnID still in resp: %q", resp.SpawnID)
	}
	if resp.CostUSD != 0.5 {
		t.Errorf("cost should be propagated even on failure: %v", resp.CostUSD)
	}
	if resp.Artifacts["status"] != "failed" {
		t.Errorf("terminal status artifact = %v, want failed", resp.Artifacts["status"])
	}
}

func TestMapTelemetryToResponse_PreservesTerminalStatusWithoutTelemetry(t *testing.T) {
	resp := mapTelemetryToResponse(&hudSpawnState{
		SpawnID: "spawn-fail",
		AgentID: "agent-1",
		Status:  "failed",
		Error:   "agent pod failed before telemetry",
	})
	if resp.SpawnID != "spawn-fail" {
		t.Fatalf("spawn id = %q", resp.SpawnID)
	}
	if resp.Artifacts["status"] != "failed" {
		t.Fatalf("status artifact = %v, want failed", resp.Artifacts["status"])
	}
	if resp.LogTail != "agent pod failed before telemetry" {
		t.Fatalf("log tail = %q", resp.LogTail)
	}
}

func TestRun_PollDeadlineExceeded(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-stuck"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-stuck", Status: "running"}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.PollDeadline = 30 * time.Millisecond
	c.cfg.PollInterval = 5 * time.Millisecond
	_, err := c.Run(context.Background(), sampleSpawnReq())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// The error MUST wrap pipeline.ErrSpawnPollTimeout: the Mills runner
	// relies on errors.Is to tell a stalled-but-alive spawn apart from a
	// transient poll interruption and convert a recurring stall into a
	// failed attempt instead of looping the stage pending forever.
	if !errors.Is(err, pipeline.ErrSpawnPollTimeout) {
		t.Errorf("err = %v; want wrapped pipeline.ErrSpawnPollTimeout", err)
	}
}

// ----- Required-field validation -----

func TestRun_RequiresProjectBranchPrompt(t *testing.T) {
	ft := &hudFakeTransport{}
	c := newHUDStub(t, ft)
	cases := []struct {
		name  string
		mut   func(*pipeline.SpawnRequest)
		errOn string
	}{
		{"no project", func(r *pipeline.SpawnRequest) { r.Project = "" }, "Project"},
		{"absolute project", func(r *pipeline.SpawnRequest) { r.Project = "/workspace/services/loom-core" }, "Project"},
		{"traversing project", func(r *pipeline.SpawnRequest) { r.Project = "services/../loom-core" }, "Project"},
		{"no branch", func(r *pipeline.SpawnRequest) { r.Branch = "" }, "Branch"},
		{"no prompt", func(r *pipeline.SpawnRequest) { r.Prompt = "" }, "Prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := sampleSpawnReq()
			tc.mut(&req)
			if _, err := c.Run(context.Background(), req); err == nil {
				t.Errorf("expected error mentioning %s", tc.errOn)
			}
		})
	}
}

// ----- HTTP error paths -----

func TestRun_PostFailureSurfacesStatus(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 401, map[string]string{"error": "unauthorized"}
		},
	}
	c := newHUDStub(t, ft)
	if _, err := c.Run(context.Background(), sampleSpawnReq()); err == nil {
		t.Error("expected error on 401")
	}
}

func TestRun_GetFailureSurfacesStatus(t *testing.T) {
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-getfail"}
		},
		get: func(_ *http.Request) (int, any) {
			return 500, map[string]string{"error": "boom"}
		},
	}
	c := newHUDStub(t, ft)
	if _, err := c.Run(context.Background(), sampleSpawnReq()); err == nil {
		t.Error("expected error on 500 GET")
	}
}

// ----- AgentType mapping -----

func TestAgentTypeMapping(t *testing.T) {
	cases := map[string]string{
		"":              "claude-code",
		"claude":        "claude-code",
		"claude-code":   "claude-code",
		"claude-sonnet": "claude-code",
		"codex":         "codex",
		"openai-codex":  "codex",
		"gemini":        "gemini",
		"qwen3-8b":      "qwen3-8b",
	}
	for in, want := range cases {
		if got := agentTypeOrDefault(in); got != want {
			t.Errorf("agentTypeOrDefault(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTerminalSpawnStatus(t *testing.T) {
	for _, s := range []string{"completed", "failed", "stopped"} {
		if !isTerminalSpawnStatus(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []string{"creating", "building", "running", "unknown", ""} {
		if isTerminalSpawnStatus(s) {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}

// TestRun_DecodesCostEstimatedFromWire proves the operator decodes the
// HUD spawn API's existing `cost_estimated` field (Codex's estimated-cost
// marker) into SpawnResponse.CostEstimated. The detail endpoint already
// serialises this on the wire; the operator subset historically dropped
// it. We feed the raw JSON (not the Go struct) so the json tag is
// exercised end-to-end.
func TestRun_DecodesCostEstimatedFromWire(t *testing.T) {
	cases := []struct {
		name          string
		costJSON      string // value for total_cost_usd + cost_estimated
		wantCost      float64
		wantEstimated bool
	}{
		{
			name:          "claude real cost",
			costJSON:      `"total_cost_usd": 1.23`,
			wantCost:      1.23,
			wantEstimated: false,
		},
		{
			name:          "codex estimated cost",
			costJSON:      `"total_cost_usd": 0.42, "cost_estimated": true`,
			wantCost:      0.42,
			wantEstimated: true,
		},
		{
			name:          "gemini unavailable cost",
			costJSON:      `"total_cost_usd": 0`,
			wantCost:      0,
			wantEstimated: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := json.RawMessage(`{"spawn_id":"sp-1","status":"completed","telemetry":{` + c.costJSON + `}}`)
			ft := &hudFakeTransport{
				post: func(_ *http.Request) (int, any) {
					return 202, hudSpawnAcceptResponse{SpawnID: "sp-1", Status: "creating"}
				},
				get: func(_ *http.Request) (int, any) {
					return 200, raw
				},
			}
			c2 := newHUDStub(t, ft)
			resp, err := c2.Run(context.Background(), sampleSpawnReq())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if resp.CostUSD != c.wantCost {
				t.Errorf("CostUSD = %v, want %v", resp.CostUSD, c.wantCost)
			}
			if resp.CostEstimated != c.wantEstimated {
				t.Errorf("CostEstimated = %v, want %v", resp.CostEstimated, c.wantEstimated)
			}
		})
	}
}

// ----- DiffPatch + CommitMessages capture -----

// spawnTelGitRunner records every invocation and returns canned stdout per
// invocation key (joined args after "git"). Unmatched keys fall back to
// exit code 128 with the literal string "unknown" — that's the same
// shape git produces for missing-ref errors so capture stays best-effort.
type spawnTelGitRunner struct {
	mu       sync.Mutex
	calls    []spawnTelGitCall
	stdouts  map[string]string
	stderrs  map[string]string
	exits    map[string]int
	errs     map[string]error
	dirSeen  string
	fallback spawnTelGitResult
}

type spawnTelGitCall struct {
	Dir  string
	Args []string
}

type spawnTelGitResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

func (r *spawnTelGitRunner) Run(_ context.Context, dir, name string, args ...string) (string, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirSeen = dir
	r.calls = append(r.calls, spawnTelGitCall{Dir: dir, Args: append([]string{name}, args...)})
	key := strings.Join(args, " ")
	if r.errs != nil {
		if err, ok := r.errs[key]; ok {
			return r.stdouts[key], r.stderrs[key], r.exits[key], err
		}
	}
	if r.stdouts != nil {
		if out, ok := r.stdouts[key]; ok {
			return out, r.stderrs[key], r.exits[key], nil
		}
	}
	return r.fallback.Stdout, r.fallback.Stderr, r.fallback.ExitCode, r.fallback.Err
}

func (r *spawnTelGitRunner) callArgs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c.Args...)
	}
	return out
}

// TestRun_PopulatesDiffPatchAndCommitMessages drives a complete
// Run() against a stub HUD + a fake git runner and asserts both
// SpawnResponse fields are populated for downstream gate input.
func TestRun_PopulatesDiffPatchAndCommitMessages(t *testing.T) {
	diffOut := "diff --git a/testdata/mills-canary/heartbeat.md b/testdata/mills-canary/heartbeat.md\n" +
		"--- a/testdata/mills-canary/heartbeat.md\n" +
		"+++ b/testdata/mills-canary/heartbeat.md\n" +
		"@@\n-old line\n+new line\n"
	logOut := "feat(canary): bump heartbeat\x00fix(spawn): retry logic\x00"

	// Branch from sampleSpawnReq → "mills/BL-X/plan_slice"; the head ref
	// the operator now diffs against is origin/<branch>.
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      diffOut,
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": logOut,
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-diff-1", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-diff-1", Status: "completed",
				Telemetry: &hudSpawnTelemetry{
					TotalCostUSD: 0.5,
					FileChanges: []hudFileChange{
						{Path: "testdata/mills-canary/heartbeat.md", Kind: "modify", LinesAdded: 1, LinesRemoved: 1},
					},
				},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "/work/spawn/heartbeat"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(resp.DiffPatch), "testdata/mills-canary/heartbeat.md") {
		t.Errorf("DiffPatch missing file path; got %q", string(resp.DiffPatch))
	}
	if len(resp.CommitMessages) != 2 {
		t.Fatalf("CommitMessages len = %d, want 2: %#v", len(resp.CommitMessages), resp.CommitMessages)
	}
	if resp.CommitMessages[0] != "feat(canary): bump heartbeat" {
		t.Errorf("CommitMessages[0] = %q", resp.CommitMessages[0])
	}
	if resp.CommitMessages[1] != "fix(spawn): retry logic" {
		t.Errorf("CommitMessages[1] = %q", resp.CommitMessages[1])
	}
	// Capture should be rooted at WorkingDir, not the operator's CWD.
	if gr.dirSeen != "/work/spawn/heartbeat" {
		t.Errorf("git ran in dir %q, want /work/spawn/heartbeat", gr.dirSeen)
	}
	// Both fetches must run before the diff so origin/<branch> AND
	// origin/main are fresh locally — the operator clone's local main is
	// only fetched at boot and goes stale between rolls. The refspec is
	// explicit and deepened: the operator clone is single-branch (main)
	// and shallow, so a bare `git fetch origin <branch>` never created
	// refs/remotes/origin/<branch> and depth-1 held no merge-base for
	// the triple-dot diff (issue #224, 2026-07-08 kill-test).
	calls := gr.callArgs()
	fetchIdx, fetchBaseIdx, diffIdx := -1, -1, -1
	for i, c := range calls {
		key := strings.Join(c, " ")
		switch key {
		case "git fetch --depth=100 origin +refs/heads/mills/BL-X/plan_slice:refs/remotes/origin/mills/BL-X/plan_slice":
			fetchIdx = i
		case "git fetch --depth=100 origin +refs/heads/main:refs/remotes/origin/main":
			fetchBaseIdx = i
		case "git diff origin/main...origin/mills/BL-X/plan_slice":
			diffIdx = i
		}
	}
	if fetchIdx < 0 {
		t.Errorf("missing git fetch call for branch; saw %v", calls)
	}
	if fetchBaseIdx < 0 {
		t.Errorf("missing git fetch call for base; saw %v", calls)
	}
	if diffIdx < 0 {
		t.Errorf("missing git diff call; saw %v", calls)
	}
	if fetchIdx >= 0 && diffIdx >= 0 && fetchIdx >= diffIdx {
		t.Errorf("fetch must run before diff; fetchIdx=%d diffIdx=%d", fetchIdx, diffIdx)
	}
	if fetchBaseIdx >= 0 && diffIdx >= 0 && fetchBaseIdx >= diffIdx {
		t.Errorf("base fetch must run before diff; fetchBaseIdx=%d diffIdx=%d", fetchBaseIdx, diffIdx)
	}
	// And the log call must use the same origin refs.
	wantLog := "git log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice"
	var sawLog bool
	for _, c := range calls {
		if strings.Join(c, " ") == wantLog {
			sawLog = true
			break
		}
	}
	if !sawLog {
		t.Errorf("missing log call %q; saw %v", wantLog, calls)
	}
}

// TestRun_FillsFilesChangedFromCumulativeDiff pins the retry-after-push
// shape (issue #224, 2026-07-08 kill-test): the session's telemetry
// reports NO file changes because the work already sits on origin/<branch>
// from a prior run's attempts. The capture must fill FilesChanged from
// `git diff --name-only` so scope/path_policy judge the cumulative work
// instead of passing trivially on an empty list.
func TestRun_FillsFilesChangedFromCumulativeDiff(t *testing.T) {
	diffOut := "diff --git a/pkg/mills/council/ci_incident_classifier.go b/pkg/mills/council/ci_incident_classifier.go\n@@\n+real work\n"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      diffOut,
			"diff --name-only origin/main...origin/mills/BL-X/plan_slice":          "pkg/mills/council/ci_incident_classifier.go\nCHANGELOG.md\n",
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": "feat(mills): classify dependency-update CI incidents\x00",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-cum-1", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			// Telemetry carries zero FileChanges — the agent session found
			// the branch already up to date and committed nothing new.
			return 200, hudSpawnState{SpawnID: "spawn-cum-1", Status: "completed", Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.1}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "/work/operator/clone"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"pkg/mills/council/ci_incident_classifier.go", "CHANGELOG.md"}
	if len(resp.FilesChanged) != len(want) {
		t.Fatalf("FilesChanged = %v, want %v", resp.FilesChanged, want)
	}
	for i := range want {
		if resp.FilesChanged[i] != want[i] {
			t.Errorf("FilesChanged[%d] = %q, want %q", i, resp.FilesChanged[i], want[i])
		}
	}
}

// TestRun_SupplementsPartialTelemetryFromCumulativeDiff pins the partial
// telemetry shape from Mills escalation #371: the spawn reported the code file
// it edited but omitted the changelog fragment it also committed. Gates must
// judge the complete branch diff, otherwise docs_guardrail rejects a valid
// branch because FilesChanged contains code but no documentation update.
func TestRun_SupplementsPartialTelemetryFromCumulativeDiff(t *testing.T) {
	diffOut := "diff --git a/pkg/mills/council/autonomy_policy.go b/pkg/mills/council/autonomy_policy.go\n@@\n+real work\n"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":             diffOut,
			"diff --name-only origin/main...origin/mills/BL-X/plan_slice": "pkg/mills/council/autonomy_policy.go\nchangelog.d/autonomy-policy.changed.md\n",
			"diff --numstat origin/main...origin/mills/BL-X/plan_slice":   "144\t0\tpkg/mills/council/autonomy_policy.go\n1000\t0\tchangelog.d/autonomy-policy.changed.md\n",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-partial-1", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-partial-1",
				Status:  "completed",
				Telemetry: &hudSpawnTelemetry{FileChanges: []hudFileChange{
					{Path: "pkg/mills/council/autonomy_policy.go", LinesAdded: 144},
				}},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "/work/operator/clone"

	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{
		"pkg/mills/council/autonomy_policy.go",
		"changelog.d/autonomy-policy.changed.md",
	}
	if !reflect.DeepEqual(resp.FilesChanged, want) {
		t.Fatalf("FilesChanged = %v, want cumulative paths %v", resp.FilesChanged, want)
	}
	if resp.LinesAdded != 1144 || resp.LinesRemoved != 0 {
		t.Fatalf("line totals = +%d/-%d, want cumulative +1144/-0", resp.LinesAdded, resp.LinesRemoved)
	}
	outcome, err := (&gates.DocsGuardrail{}).Evaluate(context.Background(), gates.StageInput{
		FilesChanged: resp.FilesChanged,
	})
	if err != nil {
		t.Fatalf("docs_guardrail: %v", err)
	}
	if !outcome.Pass {
		t.Fatalf("docs_guardrail rejected committed changelog fragment: %v", outcome.Reasons)
	}
	sizeOutcome, err := (&gates.DiffSize{MaxLines: 800}).Evaluate(context.Background(), gates.StageInput{
		LinesAdded:   resp.LinesAdded,
		LinesRemoved: resp.LinesRemoved,
		DiffPatch:    resp.DiffPatch,
	})
	if err != nil {
		t.Fatalf("diff_size: %v", err)
	}
	if sizeOutcome.Pass {
		t.Fatal("diff_size passed partial +144 telemetry despite cumulative +1144 branch diff")
	}
}

func TestMergeChangedFiles_NormalizesDistinctSpawnAndOperatorRoots(t *testing.T) {
	spawnRoot := "/workspace/services/loom-core"
	operatorRoot := "/var/lib/loom-mills/loom-core"
	telemetry := []string{
		spawnRoot + "/pkg/mills/council/autonomy_policy.go",
		operatorRoot + "/scratch/uncommitted.txt",
	}
	cumulative := []string{
		"./pkg/mills/council/autonomy_policy.go",
		"changelog.d/autonomy-policy.changed.md",
	}
	want := []string{
		"pkg/mills/council/autonomy_policy.go",
		"scratch/uncommitted.txt",
		"changelog.d/autonomy-policy.changed.md",
	}
	if got := mergeChangedFiles(telemetry, cumulative, spawnRoot, operatorRoot); !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeChangedFiles() = %v, want normalized and deduplicated %v", got, want)
	}
}

func TestMergeChangedFiles_DoesNotCollapseSuffixCollisions(t *testing.T) {
	spawnRoot := "/workspace/services/loom-core"
	operatorRoot := "/var/lib/loom-mills/loom-core"
	telemetry := []string{
		spawnRoot + "/nested/pkg/mills/council/autonomy_policy.go",
		"/srv/another-checkout/pkg/mills/council/autonomy_policy.go",
	}
	cumulative := []string{"pkg/mills/council/autonomy_policy.go"}
	want := []string{
		"nested/pkg/mills/council/autonomy_policy.go",
		"/srv/another-checkout/pkg/mills/council/autonomy_policy.go",
		"pkg/mills/council/autonomy_policy.go",
	}
	if got := mergeChangedFiles(telemetry, cumulative, spawnRoot, operatorRoot); !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeChangedFiles() = %v, want distinct suffix paths %v", got, want)
	}
}

func TestMergeChangedFiles_PrefersNestedOperatorRoot(t *testing.T) {
	spawnRoot := "/workspace/services/loom-core"
	operatorRoot := spawnRoot + "/.worktrees/fix-cumulative"
	telemetry := []string{operatorRoot + "/pkg/mills/clients/spawn.go"}
	cumulative := []string{"pkg/mills/clients/spawn.go"}
	want := []string{"pkg/mills/clients/spawn.go"}
	if got := mergeChangedFiles(telemetry, cumulative, spawnRoot, operatorRoot); !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeChangedFiles() = %v, want most-specific-root mapping %v", got, want)
	}
}

func TestSpawnCheckoutRoots(t *testing.T) {
	tests := []struct {
		project string
		want    []string
		ok      bool
	}{
		{project: "services/loom-core", want: []string{"/workspace/services/loom-core"}, ok: true},
		{project: "loom-core", want: []string{
			"/workspace/loom-core",
			"/workspace/services/loom-core",
			"/workspace/libs/loom-core",
			"/workspace/platform/loom-core",
			"/workspace/private/loom-core",
			"/workspace/labs/loom-core",
		}, ok: true},
		{project: "/services/loom-core"},
		{project: "services/../loom-core"},
		{project: "../loom-core"},
		{project: `services\\loom-core`},
	}
	for _, tc := range tests {
		t.Run(tc.project, func(t *testing.T) {
			got, ok := spawnCheckoutRoots(tc.project)
			if !reflect.DeepEqual(got, tc.want) || ok != tc.ok {
				t.Fatalf("spawnCheckoutRoots(%q) = (%v, %v), want (%v, %v)", tc.project, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMergeChangedFiles_BareProjectRootCandidates(t *testing.T) {
	spawnRoots, ok := spawnCheckoutRoots("loom-core")
	if !ok {
		t.Fatal("bare project roots rejected")
	}
	tests := []struct {
		name      string
		telemetry string
	}{
		{name: "flat root", telemetry: "/workspace/loom-core/pkg/mills/clients/spawn.go"},
		{name: "alternate bucket", telemetry: "/workspace/libs/loom-core/pkg/mills/clients/spawn.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cumulative := []string{"pkg/mills/clients/spawn.go"}
			got := mergeChangedFiles([]string{tc.telemetry}, cumulative, spawnRoots...)
			if !reflect.DeepEqual(got, cumulative) {
				t.Fatalf("mergeChangedFiles() = %v, want bare-project root normalized to %v", got, cumulative)
			}
		})
	}
}

func TestRun_PatchFailureStillCapturesNamesAndNumstat(t *testing.T) {
	const diffKey = "diff origin/main...origin/mills/BL-X/plan_slice"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			diffKey: "stale patch must be ignored",
			"diff --name-only origin/main...origin/mills/BL-X/plan_slice": "pkg/mills/council/autonomy_policy.go\nchangelog.d/autonomy-policy.changed.md\n",
			"diff --numstat origin/main...origin/mills/BL-X/plan_slice":   "10\t2\tpkg/mills/council/autonomy_policy.go\n1\t0\tchangelog.d/autonomy-policy.changed.md\n",
		},
		exits: map[string]int{diffKey: 128},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-patch-fail", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-patch-fail",
				Status:  "completed",
				Telemetry: &hudSpawnTelemetry{FileChanges: []hudFileChange{
					{Path: "/workspace/services/loom-core/pkg/mills/council/autonomy_policy.go", LinesAdded: 3},
				}},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.Project = "services/loom-core"
	req.WorkingDir = "/var/lib/loom-mills/loom-core"

	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.DiffPatch == nil || len(resp.DiffPatch) != 0 {
		t.Fatalf("DiffPatch = %q, want non-nil empty capture after patch failure", resp.DiffPatch)
	}
	wantFiles := []string{"pkg/mills/council/autonomy_policy.go", "changelog.d/autonomy-policy.changed.md"}
	if !reflect.DeepEqual(resp.FilesChanged, wantFiles) {
		t.Fatalf("FilesChanged = %v, want independent name capture %v", resp.FilesChanged, wantFiles)
	}
	if resp.LinesAdded != 11 || resp.LinesRemoved != 2 {
		t.Fatalf("line totals = +%d/-%d, want independent numstat +11/-2", resp.LinesAdded, resp.LinesRemoved)
	}
}

func TestRun_FailedFetchRetainsTelemetryInsteadOfStaleRefs(t *testing.T) {
	branchFetch := "fetch --depth=100 origin +refs/heads/mills/BL-X/plan_slice:refs/remotes/origin/mills/BL-X/plan_slice"
	baseFetch := "fetch --depth=100 origin +refs/heads/main:refs/remotes/origin/main"
	tests := []struct {
		name        string
		failedFetch string
	}{
		{name: "branch refresh", failedFetch: branchFetch},
		{name: "base refresh", failedFetch: baseFetch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gr := &spawnTelGitRunner{
				stdouts: map[string]string{
					tc.failedFetch: "",
					"diff origin/main...origin/mills/BL-X/plan_slice":             "diff --git a/stale.go b/stale.go\n+stale\n",
					"diff --name-only origin/main...origin/mills/BL-X/plan_slice": "stale.go\n",
					"diff --numstat origin/main...origin/mills/BL-X/plan_slice":   "999\t0\tstale.go\n",
				},
				exits: map[string]int{tc.failedFetch: 1},
			}
			ft := &hudFakeTransport{
				post: func(_ *http.Request) (int, any) {
					return 202, hudSpawnAcceptResponse{SpawnID: "spawn-fetch-fail", Status: "creating"}
				},
				get: func(_ *http.Request) (int, any) {
					return 200, hudSpawnState{
						SpawnID: "spawn-fetch-fail",
						Status:  "completed",
						Telemetry: &hudSpawnTelemetry{FileChanges: []hudFileChange{
							{Path: "current.go", LinesAdded: 3, LinesRemoved: 1},
						}},
					}
				},
			}
			c := newHUDStub(t, ft)
			c.cfg.GitRunner = gr
			req := sampleSpawnReq()
			req.WorkingDir = "/work/operator/clone"

			resp, err := c.Run(context.Background(), req)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if want := []string{"current.go"}; !reflect.DeepEqual(resp.FilesChanged, want) {
				t.Fatalf("FilesChanged = %v, want current telemetry %v", resp.FilesChanged, want)
			}
			if resp.LinesAdded != 3 || resp.LinesRemoved != 1 {
				t.Fatalf("line totals = +%d/-%d, want telemetry +3/-1", resp.LinesAdded, resp.LinesRemoved)
			}
			if resp.DiffPatch != nil || resp.CommitMessages != nil {
				t.Fatalf("stale cumulative context used after failed fetch: diff=%q commits=%v", resp.DiffPatch, resp.CommitMessages)
			}
			for _, call := range gr.callArgs() {
				if len(call) > 1 && (call[1] == "diff" || call[1] == "log") {
					t.Fatalf("stale ref read after failed fetch: %v", call)
				}
			}
		})
	}
}

func TestCaptureGitLineTotals(t *testing.T) {
	const key = "diff --numstat origin/main...origin/branch"
	tests := []struct {
		name        string
		stdout      string
		exitCode    int
		runErr      error
		wantAdded   int
		wantRemoved int
		wantOK      bool
	}{
		{name: "binary entries count as zero", stdout: "-\t-\tassets/logo.png\n2\t3\tpkg/x.go\n", wantAdded: 2, wantRemoved: 3, wantOK: true},
		{name: "malformed output falls back", stdout: "not-a-count\t1\tpkg/x.go\n"},
		{name: "nonzero exit falls back", stdout: "9\t1\tstale.go\n", exitCode: 1},
		{name: "runner error falls back", stdout: "9\t1\tstale.go\n", runErr: errors.New("git unavailable")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gr := &spawnTelGitRunner{
				stdouts: map[string]string{key: tc.stdout},
				exits:   map[string]int{key: tc.exitCode},
			}
			if tc.runErr != nil {
				gr.errs = map[string]error{key: tc.runErr}
			}
			added, removed, ok := captureGitLineTotals(context.Background(), gr, "/repo", "origin/main", "origin/branch")
			if added != tc.wantAdded || removed != tc.wantRemoved || ok != tc.wantOK {
				t.Fatalf("captureGitLineTotals() = (%d, %d, %v), want (%d, %d, %v)", added, removed, ok, tc.wantAdded, tc.wantRemoved, tc.wantOK)
			}
		})
	}
}

// TestRun_DiffPatchTruncatedAtCap synthesizes an oversized diff and
// confirms the byte cap + marker land on SpawnResponse.DiffPatch. The
// rubric prompt has its own 8 KiB cap; this test guards the 32 KiB
// spawn-client cap so the marker is visible in stage_results artifacts
// even when the prompt re-truncates.
func TestRun_DiffPatchTruncatedAtCap(t *testing.T) {
	bigDiff := strings.Repeat("x", 64*1024)
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      bigDiff,
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": "",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-big", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-big", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.MaxDiffBytes = 4 * 1024
	req := sampleSpawnReq()
	req.WorkingDir = "/work/spawn/big"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(resp.DiffPatch), "[truncated") {
		t.Errorf("DiffPatch missing truncation marker; len=%d", len(resp.DiffPatch))
	}
	// Allow marker overhead on top of the byte cap.
	if len(resp.DiffPatch) > 4*1024+128 {
		t.Errorf("DiffPatch len = %d, want <= %d", len(resp.DiffPatch), 4*1024+128)
	}
}

// TestRun_CommitMessagesTruncatedAtCap drives the per-message byte
// budget: a runaway commit body gets truncated, but earlier commits
// that fit are preserved intact.
func TestRun_CommitMessagesTruncatedAtCap(t *testing.T) {
	big := strings.Repeat("y", 12*1024)
	logOut := "feat: tiny\x00" + big + "\x00"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      "",
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": logOut,
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-big-msg"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-big-msg", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.MaxCommitMessagesBytes = 4 * 1024
	req := sampleSpawnReq()
	req.WorkingDir = "/work/spawn/msgs"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(resp.CommitMessages) != 2 {
		t.Fatalf("CommitMessages len = %d, want 2", len(resp.CommitMessages))
	}
	if resp.CommitMessages[0] != "feat: tiny" {
		t.Errorf("first message = %q, want %q (small msg should be preserved)", resp.CommitMessages[0], "feat: tiny")
	}
	if !strings.Contains(resp.CommitMessages[1], "[truncated") {
		t.Errorf("second message missing truncation marker; got prefix %q", resp.CommitMessages[1][:64])
	}
}

// TestRun_EmptyWorktreeYieldsEmptyDiff covers the canary's no-op edit
// case: the spawn ran but the working tree carries no changes vs base.
// The post_review_gate's M2.5 retry-on-unparseable path is the safety
// net for the "judge has nothing to grade" outcome; this test just
// guarantees DiffPatch is the empty slice (not nil, not absent) so
// downstream code can distinguish "ran git, nothing changed" from
// "didn't run git at all".
func TestRun_EmptyWorktreeYieldsEmptyDiff(t *testing.T) {
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      "",
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": "",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-empty"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-empty", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "/work/spawn/empty"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.DiffPatch == nil {
		t.Error("DiffPatch is nil; want non-nil empty slice so downstream sees 'ran git, nothing changed'")
	}
	if len(resp.DiffPatch) != 0 {
		t.Errorf("DiffPatch should be empty for unchanged worktree; got %q", string(resp.DiffPatch))
	}
	if resp.CommitMessages != nil {
		t.Errorf("CommitMessages should be nil when no commits exist; got %v", resp.CommitMessages)
	}
}

// TestRun_GitFailureFallsBackToEmptyCapture guards the operator
// against an infrastructure-level git failure (worktree gone after pod
// terminated, base ref missing, etc). The spawn result must still be
// returned — DiffPatch becomes empty so the M2.5 retry path can decide
// what to do.
func TestRun_GitFailureFallsBackToEmptyCapture(t *testing.T) {
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      "irrelevant",
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": "ignored",
		},
		exits: map[string]int{
			"diff origin/main...origin/mills/BL-X/plan_slice":                      128,
			"log --pretty=format:%B%x00 origin/main..origin/mills/BL-X/plan_slice": 128,
		},
		stderrs: map[string]string{
			"diff origin/main...origin/mills/BL-X/plan_slice": "fatal: bad revision 'main...origin/mills/BL-X/plan_slice'",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-git-fail"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-git-fail", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "/work/spawn/broken"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Diff capture failed → empty slice (not nil), so downstream knows
	// we tried.
	if resp.DiffPatch == nil || len(resp.DiffPatch) != 0 {
		t.Errorf("DiffPatch should be empty after git failure; got %q", string(resp.DiffPatch))
	}
	if resp.CommitMessages != nil {
		t.Errorf("CommitMessages should be nil after git failure; got %v", resp.CommitMessages)
	}
}

// TestRun_NoWorktreeSkipsGitCapture exercises the legacy code path:
// stages that don't pass a WorkingDir + BaseBranch must not attempt
// git capture. The skip is now RECORDED rather than silent — see
// TestGitCapture_SkipWithoutCoordinatesIsEvented in
// spawn_git_capture_test.go.
func TestRun_NoWorktreeSkipsGitCapture(t *testing.T) {
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-no-wd"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-no-wd", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	req := sampleSpawnReq()
	req.WorkingDir = "" // operator omitted
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.DiffPatch != nil {
		t.Errorf("DiffPatch should stay nil when WorkingDir is empty; got %q", string(resp.DiffPatch))
	}
	if resp.CommitMessages != nil {
		t.Errorf("CommitMessages should stay nil when WorkingDir is empty; got %v", resp.CommitMessages)
	}
	if len(gr.callArgs()) != 0 {
		t.Errorf("git runner should not be invoked; saw %v", gr.callArgs())
	}
}

// TestResume_SkipsGitCapture mirrors the WorkingDir-empty case for the
// coordinate-less Resume() entrypoint, which is now the LEGACY form: the
// dispatcher prefers ResumeWithContext so a stage finishing across an
// operator rollout still gets the cumulative capture (issue #224). This
// test pins that the bare form stays inert rather than guessing a repo.
func TestResume_SkipsGitCapture(t *testing.T) {
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{},
	}
	ft := &hudFakeTransport{
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-resume-1", Status: "completed", Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.1}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	resp, err := c.Resume(context.Background(), "spawn-resume-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resp.DiffPatch != nil || resp.CommitMessages != nil {
		t.Errorf("Resume should leave Diff/Commits unset; got Diff=%q Commits=%v", string(resp.DiffPatch), resp.CommitMessages)
	}
	if len(gr.callArgs()) != 0 {
		t.Errorf("git runner should not be invoked on Resume; saw %v", gr.callArgs())
	}
}

// TestNewHUDSpawnClient_DefaultsGitConfig confirms the constructor
// fills in the default git runner + byte caps so callers don't have
// to wire them by hand.
func TestNewHUDSpawnClient_DefaultsGitConfig(t *testing.T) {
	c, err := NewHUDSpawnClient(HUDSpawnConfig{BaseURL: "x", Token: "y"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if c.cfg.GitRunner == nil {
		t.Error("default GitRunner not installed")
	}
	if c.cfg.MaxDiffBytes != defaultMaxDiffBytes {
		t.Errorf("MaxDiffBytes default = %d, want %d", c.cfg.MaxDiffBytes, defaultMaxDiffBytes)
	}
	if c.cfg.MaxCommitMessagesBytes != defaultMaxCommitMessagesBytes {
		t.Errorf("MaxCommitMessagesBytes default = %d, want %d", c.cfg.MaxCommitMessagesBytes, defaultMaxCommitMessagesBytes)
	}
}

// ----- Cumulative-diff regression: issue #224 -----

// TestRun_CumulativeDiffCoversErroredPushAttempt reproduces the
// attempt-1-errored-after-push escalation shape (issue #224):
//
//	attempt 1: the implement spawn pushes real commits to
//	           origin/<branch>, then dies (status=failed). The errored
//	           attempt records no successful StageOutput, so
//	           runner.carryForwardDiff has nothing to carry forward.
//	attempt 2: a fresh spawn finds the branch already complete, does
//	           nothing, and completes with EMPTY telemetry (zero file
//	           changes).
//
// Pre-fix, attempt 2's StageOutput was empty and post_implement_gate's
// nonempty_diff escalated a run whose finished work was already sitting
// on the branch. With WorkingDir (the operator-local clone) + BaseBranch
// wired by SpawnWorker, the post-terminal git capture reads the
// cumulative origin/main...origin/<branch> diff, so attempt 2's response
// carries the pushed work and the gate passes.
func TestRun_CumulativeDiffCoversErroredPushAttempt(t *testing.T) {
	branch := "mills/BL-X/implement"
	diffOut := "diff --git a/pkg/x/x.go b/pkg/x/x.go\n--- a/pkg/x/x.go\n+++ b/pkg/x/x.go\n@@\n-old\n+new\n"
	logOut := "feat(x): implement BL-X\x00"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/" + branch:                      diffOut,
			"diff --name-only origin/main...origin/" + branch:          "pkg/x/x.go\n",
			"diff --numstat origin/main...origin/" + branch:            "1\t1\tpkg/x/x.go\n",
			"log --pretty=format:%B%x00 origin/main..origin/" + branch: logOut,
		},
	}
	var posts atomic.Int32
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			id := fmt.Sprintf("spawn-attempt-%d", posts.Add(1))
			return 202, hudSpawnAcceptResponse{SpawnID: id, Status: "creating"}
		},
		get: func(r *http.Request) (int, any) {
			if strings.HasSuffix(r.URL.Path, "spawn-attempt-1") {
				// Attempt 1 died after pushing; no telemetry survives.
				return 200, hudSpawnState{SpawnID: "spawn-attempt-1", Status: "failed", Error: "agent crashed after push"}
			}
			// Attempt 2 completes but did no new work of its own.
			return 200, hudSpawnState{SpawnID: "spawn-attempt-2", Status: "completed", Telemetry: &hudSpawnTelemetry{TurnCount: 1}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr

	req := sampleSpawnReq()
	req.StageID = "implement"
	req.Branch = branch
	req.WorkingDir = "/var/lib/loom-mills/loom-core" // operator-local clone

	// Attempt 1: errored spawn. The error surfaces, but the response
	// still carries the cumulative capture — the commits are on origin.
	resp1, err := c.Run(context.Background(), req)
	if err == nil {
		t.Fatal("attempt 1: want error for failed spawn")
	}
	if !strings.Contains(string(resp1.DiffPatch), "pkg/x/x.go") {
		t.Errorf("attempt 1: DiffPatch should carry pushed work even on failure; got %q", string(resp1.DiffPatch))
	}

	// Attempt 2: clean completion with empty telemetry. The cumulative git
	// capture must still populate the pushed path and authoritative line totals.
	resp2, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if want := []string{"pkg/x/x.go"}; !reflect.DeepEqual(resp2.FilesChanged, want) {
		t.Fatalf("attempt 2: FilesChanged = %v, want cumulative %v", resp2.FilesChanged, want)
	}
	if resp2.LinesAdded != 1 || resp2.LinesRemoved != 1 {
		t.Fatalf("attempt 2: line totals = +%d/-%d, want cumulative +1/-1", resp2.LinesAdded, resp2.LinesRemoved)
	}
	if !strings.Contains(string(resp2.DiffPatch), "pkg/x/x.go") {
		t.Errorf("attempt 2: DiffPatch missing cumulative branch work; got %q", string(resp2.DiffPatch))
	}
	if len(resp2.CommitMessages) != 1 || resp2.CommitMessages[0] != "feat(x): implement BL-X" {
		t.Errorf("attempt 2: CommitMessages = %#v", resp2.CommitMessages)
	}

	// post_implement_gate's first check must now pass on the retry's
	// output instead of escalating the finished branch.
	outcome, gerr := (&gates.NonEmptyDiff{}).Evaluate(context.Background(), gates.StageInput{
		FilesChanged: resp2.FilesChanged,
		DiffPatch:    resp2.DiffPatch,
	})
	if gerr != nil {
		t.Fatalf("nonempty_diff: %v", gerr)
	}
	if !outcome.Pass {
		t.Errorf("nonempty_diff failed on cumulative diff: %v", outcome.Reasons)
	}
}

// TestRun_CumulativeCaptureExcludesRevertedTelemetryPath reproduces the
// gate-fail-retry shape that wrongly escalated run
// PIPE-bl-daemon-hub-ws-liveness-backoff-20260810 (2026-08-11): attempt 1
// committed an out-of-scope file, the scope gate failed, and the retry
// edited that file back to its base state before force-pushing a clean
// branch. Session telemetry lists the reverted path as touched, but the
// branch-vs-base capture does not contain it. FilesChanged must describe
// the merge envelope (the capture), not the telemetry union — the union
// re-failed the scope gate on work no longer on the branch, and the
// FilesChanged-vs-patch-header disagreement tripped fabricated_slice's
// truncated-patch heuristic.
func TestRun_CumulativeCaptureExcludesRevertedTelemetryPath(t *testing.T) {
	branch := "mills/BL-X/implement"
	diffOut := "diff --git a/pkg/x/x.go b/pkg/x/x.go\n--- a/pkg/x/x.go\n+++ b/pkg/x/x.go\n@@\n-old\n+new\n"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff origin/main...origin/" + branch:                      diffOut,
			"diff --name-only origin/main...origin/" + branch:          "pkg/x/x.go\n",
			"diff --numstat origin/main...origin/" + branch:            "1\t1\tpkg/x/x.go\n",
			"log --pretty=format:%B%x00 origin/main..origin/" + branch: "fix(x): drop out-of-scope edit\x00",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-revert", Status: "creating"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-revert",
				Status:  "completed",
				Telemetry: &hudSpawnTelemetry{TurnCount: 1, FileChanges: []hudFileChange{
					{Path: "pkg/x/x.go", LinesAdded: 1, LinesRemoved: 1},
					// Touched only to restore base content; absent from
					// the branch diff.
					{Path: "pkg/agentcontext/svc_compaction_wrappers.go", LinesAdded: 4, LinesRemoved: 4},
				}},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr

	req := sampleSpawnReq()
	req.StageID = "implement"
	req.Branch = branch
	req.WorkingDir = "/var/lib/loom-mills/loom-core"

	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := []string{"pkg/x/x.go"}; !reflect.DeepEqual(resp.FilesChanged, want) {
		t.Fatalf("FilesChanged = %v, want capture-only %v (reverted telemetry path must not survive)", resp.FilesChanged, want)
	}
	if resp.LinesAdded != 1 || resp.LinesRemoved != 1 {
		t.Fatalf("line totals = +%d/-%d, want cumulative +1/-1", resp.LinesAdded, resp.LinesRemoved)
	}
}

func TestHUDSpawnClientStop_UsesAuthenticatedEscapedEndpoint(t *testing.T) {
	ft := &hudFakeTransport{post: func(_ *http.Request) (int, any) {
		return http.StatusOK, map[string]bool{"stopped": true}
	}}
	c := newHUDStub(t, ft)
	if err := c.Stop(context.Background(), "spawn/a b"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	reqs := ft.recordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].Path != "/api/mobile/v1/agent/spawn/spawn/a b/stop" {
		t.Errorf("path = %q", reqs[0].Path)
	}
	if reqs[0].EscapedPath != "/api/mobile/v1/agent/spawn/spawn%2Fa%20b/stop" {
		t.Errorf("escaped path = %q", reqs[0].EscapedPath)
	}
	if reqs[0].Auth != "Bearer tok-abc" {
		t.Errorf("authorization = %q", reqs[0].Auth)
	}
}

func TestHUDSpawnClientStop_ReturnsStatusError(t *testing.T) {
	ft := &hudFakeTransport{post: func(_ *http.Request) (int, any) {
		return http.StatusInternalServerError, map[string]string{"error": "boom"}
	}}
	c := newHUDStub(t, ft)
	err := c.Stop(context.Background(), "spawn-err")
	if err == nil {
		t.Fatal("stop error = nil, want status error")
	}
	if !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stop error = %v, want status and body", err)
	}
}
