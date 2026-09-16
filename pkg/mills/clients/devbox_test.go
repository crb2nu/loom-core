package clients

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// devboxStubResult builds a CallTool envelope whose first content block
// is the JSON of a qualityGateResult.
func devboxStubResult(t *testing.T, body devboxQualityGateResult) []byte {
	t.Helper()
	bodyJSON, _ := json.Marshal(body)
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: string(bodyJSON)}},
	}
	out, _ := json.Marshal(res)
	return out
}

func TestDevboxClient_HappyPathPropagatesPassedAndChecks(t *testing.T) {
	body := devboxQualityGateResult{
		Language:        "go",
		Passed:          true,
		TotalDurationMs: 5000,
		Checks: []devboxQualityCheckRow{
			{Name: "fmt", Passed: true, DurationMs: 200},
			{Name: "lint", Passed: true, DurationMs: 1500},
			{Name: "test", Passed: true, DurationMs: 3300},
		},
	}
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
			"tools/call": devboxStubResult(t, body),
		},
	}
	hub := newTestHubClient(t, ft)
	dc := NewDevboxClient(hub)
	resp, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{
		Project: "loom-core",
		AgentID: "claude-code",
	})
	if err != nil {
		t.Fatalf("quality gate: %v", err)
	}
	if !resp.Passed {
		t.Errorf("expected Passed=true")
	}
	if resp.Language != "go" {
		t.Errorf("language = %q", resp.Language)
	}
	if len(resp.Checks) != 3 {
		t.Errorf("checks len = %d, want 3", len(resp.Checks))
	}
	if resp.Checks[1].Name != "lint" {
		t.Errorf("check 1 name = %q", resp.Checks[1].Name)
	}
	if resp.Checks[2].Duration < 3.0 {
		t.Errorf("check 2 duration should be in seconds: %v", resp.Checks[2].Duration)
	}
}

func TestDevboxClient_PassesProjectAndAgentIDArgs(t *testing.T) {
	body := devboxQualityGateResult{Language: "go", Passed: true}
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": devboxStubResult(t, body),
		},
	}
	hub := newTestHubClient(t, ft)
	dc := NewDevboxClient(hub)
	if _, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{
		Project: "platform/gitops",
		AgentID: "loom-mills",
	}); err != nil {
		t.Fatalf("call: %v", err)
	}
	sent := ft.sentMessages()
	var callMsg mcp.Message
	for _, m := range sent {
		if m.Method == "tools/call" {
			callMsg = m
		}
	}
	var params mcp.CallToolParams
	if err := json.Unmarshal(callMsg.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.Name != "devbox_quality_gate" {
		t.Errorf("tool name = %q", params.Name)
	}
	if params.Arguments["project"] != "platform/gitops" {
		t.Errorf("project arg = %v", params.Arguments["project"])
	}
	if params.Arguments["agent_id"] != "loom-mills" {
		t.Errorf("agent_id arg = %v", params.Arguments["agent_id"])
	}
}

func TestDevboxClient_ForwardsChecksTestsAndEnv(t *testing.T) {
	body := devboxQualityGateResult{Language: "go", Passed: true}
	ft := &fakeTransport{responses: map[string][]byte{
		"initialize": []byte(`{}`), "tools/call": devboxStubResult(t, body),
	}}
	hub := newTestHubClient(t, ft)
	_, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{
		Project: "loom-core", Checks: []string{"fmt"},
		TestCommands: []string{"go test ./cmd/loom -run Mills"},
		Env:          map[string]string{"GIT_TOKEN": "token", "CUSTOM": "value"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var callMsg mcp.Message
	for _, message := range ft.sentMessages() {
		if message.Method == "tools/call" {
			callMsg = message
		}
	}
	var params mcp.CallToolParams
	if err := json.Unmarshal(callMsg.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	commands, ok := params.Arguments["extra_test_commands"].([]any)
	if !ok || len(commands) != 1 || commands[0] != "go test ./cmd/loom -run Mills" {
		t.Fatalf("extra_test_commands = %#v", params.Arguments["extra_test_commands"])
	}
	env, ok := params.Arguments["env"].(map[string]any)
	if !ok || env["GIT_TOKEN"] != "token" || env["CUSTOM"] != "value" {
		t.Fatalf("env = %#v", params.Arguments["env"])
	}
}

func TestDevboxClient_FailedGateBuildsLogTail(t *testing.T) {
	body := devboxQualityGateResult{
		Language: "python",
		Passed:   false,
		Checks: []devboxQualityCheckRow{
			{Name: "fmt", Passed: true, DurationMs: 100},
			{Name: "lint", Passed: false, DurationMs: 500, OutputTail: "ruff: E501 line too long\n"},
			{Name: "test", Passed: false, DurationMs: 800, ExitCode: 1, OutputTail: "1 failed, 5 passed\n"},
		},
	}
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": devboxStubResult(t, body),
		},
	}
	hub := newTestHubClient(t, ft)
	dc := NewDevboxClient(hub)
	resp, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err != nil {
		t.Fatalf("call should succeed even when gate fails: %v", err)
	}
	if resp.Passed {
		t.Errorf("Passed should be false")
	}
	if !strings.Contains(resp.LogTail, "FAIL lint") {
		t.Errorf("log tail missing FAIL marker: %q", resp.LogTail)
	}
	if !strings.Contains(resp.LogTail, "ruff: E501") {
		t.Errorf("log tail missing failed-check output: %q", resp.LogTail)
	}
	if !strings.Contains(resp.LogTail, "PASS fmt") {
		t.Errorf("log tail missing PASS marker for passing check: %q", resp.LogTail)
	}
}

func TestDevboxClient_DecodesTOONGateResult(t *testing.T) {
	toonBody := "language: unknown\npassed: false\nchecks[1]{name,passed,exit_code,duration_ms}:\n  fmt,false,1,42\ntotal_duration_ms: 42"
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: toonBody}},
	}
	resBytes, _ := json.Marshal(res)
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": resBytes,
		},
	}
	hub := newTestHubClient(t, ft)
	resp, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err != nil {
		t.Fatalf("TOON quality gate should decode: %v", err)
	}
	if resp.Passed {
		t.Fatal("expected failed gate")
	}
	if resp.Language != "unknown" {
		t.Fatalf("language = %q", resp.Language)
	}
	if len(resp.Checks) != 1 || resp.Checks[0].Name != "fmt" || resp.Checks[0].ExitCode != 1 {
		t.Fatalf("checks = %#v", resp.Checks)
	}
	if !strings.Contains(resp.LogTail, "FAIL fmt") {
		t.Fatalf("log tail = %q", resp.LogTail)
	}
}

func TestDevboxClient_PrefersStructuredVerdictOverLossyText(t *testing.T) {
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: `{"language":"unknown","passed":false,"checks":[]}`}},
		StructuredContent: map[string]any{
			"language": "go", "passed": false, "tested_sha": "deadbeef", "total_duration_ms": 42,
			"checks": []any{map[string]any{
				"name": "test", "passed": false, "exit_code": 1, "duration_ms": 42,
				"output_tail": "structured failure",
			}},
		},
	}
	resBytes, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	ft := &fakeTransport{responses: map[string][]byte{
		"initialize": []byte(`{}`),
		"tools/call": resBytes,
	}}

	resp, err := NewDevboxClient(newTestHubClient(t, ft)).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err != nil {
		t.Fatalf("QualityGate: %v", err)
	}
	if resp.Language != "go" || resp.TestedSHA != "deadbeef" || len(resp.Checks) != 1 {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Checks[0].ExitCode != 1 || resp.Checks[0].Output != "structured failure" {
		t.Fatalf("check = %#v", resp.Checks[0])
	}
}

func TestDevboxClient_MixedRowsSurviveTOONRoundTrip(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "toon")
	body := devboxQualityGateResult{
		Language: "go",
		Passed:   false,
		Checks: []devboxQualityCheckRow{
			{Name: "fmt", Passed: true, DurationMs: 12},
			{Name: "test:0", Passed: false, ExitCode: 1, DurationMs: 34, OutputTail: "go test failed"},
		},
		TotalDurationMs: 46,
	}
	result, err := mcp.JSONResult(body)
	if err != nil {
		t.Fatalf("encode TOON gate result: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("TOON gate result has no content")
	}

	decoded, err := parseDevboxQualityGateResult(result.Content[0].Text)
	if err != nil {
		t.Fatalf("decode TOON gate result: %v", err)
	}
	if len(decoded.Checks) != 2 {
		t.Fatalf("checks = %#v", decoded.Checks)
	}
	failed := decoded.Checks[1]
	if failed.ExitCode != 1 || failed.OutputTail != "go test failed" {
		t.Fatalf("failed check lost TOON columns: %#v", failed)
	}
}

// TestDevboxClient_StderrFallbackOutput verifies that when a check
// reports an empty stdout (typical of `make fmt` errors which write
// to stderr only), the client surfaces stderr_tail through Output so
// the runner's escalation reason is not a blank string.
func TestDevboxClient_StderrFallbackOutput(t *testing.T) {
	const stderrMsg = "make: *** No rule to make target 'fmt'.  Stop."
	body := devboxQualityGateResult{
		Language: "unknown",
		Passed:   false,
		Checks: []devboxQualityCheckRow{
			{Name: "fmt", Passed: false, ExitCode: 1, DurationMs: 8, OutputTail: "", StderrTail: stderrMsg},
		},
	}
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": devboxStubResult(t, body),
		},
	}
	hub := newTestHubClient(t, ft)
	resp, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err != nil {
		t.Fatalf("QualityGate should not error on a non-passing gate: %v", err)
	}
	if len(resp.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(resp.Checks))
	}
	if resp.Checks[0].Output != stderrMsg {
		t.Fatalf("Output = %q, want stderr fallback %q", resp.Checks[0].Output, stderrMsg)
	}
}

func TestDevboxClient_RequiresProject(t *testing.T) {
	hub := newTestHubClient(t, &fakeTransport{})
	if _, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{}); err == nil {
		t.Error("expected error when Project empty")
	}
}

func TestDevboxClient_NilHubErrors(t *testing.T) {
	dc := &DevboxClient{}
	if _, err := dc.QualityGate(context.Background(), pipeline.DevboxRequest{Project: "x"}); err == nil {
		t.Error("expected error with nil hub")
	}
}

// TestDevboxClient_InfraErrorBodyDoesNotFabricateVerdict is the
// regression test for escalation #322 ("devbox quality gate failed
// (0/0 checks marked failed; gate reported not passed)" ×3): a tool
// ERROR whose plain-text body contains a colon TOON-decoded into an
// unrelated object, unmarshaled into a zero-value gate result
// (passed=false, checks=[]), and QualityGate dropped the CallTool
// error — fabricating a not-passed verdict that the tests stage
// burned as a code-class failure. The client must instead surface the
// real error text, which Classify maps to the infra class.
func TestDevboxClient_InfraErrorBodyDoesNotFabricateVerdict(t *testing.T) {
	const infraMsg = "ensure sandbox: sandbox image still building after 8m0s: build in progress"
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: infraMsg}},
		IsError: true,
	}
	resBytes, _ := json.Marshal(res)
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": resBytes,
		},
	}
	hub := newTestHubClient(t, ft)
	_, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err == nil {
		t.Fatal("expected an error, not a fabricated not-passed/zero-checks verdict")
	}
	if !strings.Contains(err.Error(), "sandbox image still building") {
		t.Errorf("error should carry the real infra failure text: %v", err)
	}
	// Still-building is a FREE transient since the 2026-09-02 retry-honesty
	// change (the build keeps running; the next gate call re-attaches), so it
	// neither burns attempts as code nor as infra.
	if got := pipeline.Classify(err); got != pipeline.ClassTransient {
		t.Errorf("Classify = %s, want %s (so the tests stage doesn't burn attempts as code or infra)", got, pipeline.ClassTransient)
	}
}

// TestDevboxClient_RejectsJSONWithoutPassedField verifies the strict
// parse: a syntactically valid JSON object that is not a gate result
// (no "passed" field) must not decode into a zero-value verdict.
func TestDevboxClient_RejectsJSONWithoutPassedField(t *testing.T) {
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: `{"error":"backend unavailable"}`}},
	}
	resBytes, _ := json.Marshal(res)
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": resBytes,
		},
	}
	hub := newTestHubClient(t, ft)
	_, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"})
	if err == nil {
		t.Fatal("expected decode error for a non-gate JSON object")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("error should expose raw body: %v", err)
	}
}

func TestDevboxClient_DecodeFailureSurfacesRawBody(t *testing.T) {
	res := mcp.CallToolResult{
		Content: []mcp.Content{{Type: "text", Text: "not-json"}},
	}
	resBytes, _ := json.Marshal(res)
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": resBytes,
		},
	}
	hub := newTestHubClient(t, ft)
	_, err := NewDevboxClient(hub).QualityGate(context.Background(), pipeline.DevboxRequest{Project: "x"})
	if err == nil {
		t.Error("expected decode error")
	}
	if !strings.Contains(err.Error(), "not-json") {
		t.Errorf("error should expose raw body: %v", err)
	}
}

func TestDevboxClient_EffectiveGateTimeout(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", time.Hour}, {"90m", 90 * time.Minute}, {"0s", 500 * time.Millisecond}, {"-1m", 500 * time.Millisecond},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LOOM_MILLS_DEVBOX_GATE_TIMEOUT", tc.env)
			client := NewDevboxClient(newTestHubClient(t, &fakeTransport{}))
			d := pipeline.NewDispatcher(map[string]pipeline.Worker{"tests": &pipeline.DevboxWorker{Client: client}}, nil)
			if got := d.SynchronousCallTimeout("tests"); got != tc.want {
				t.Fatalf("timeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// Capture the tools/call deadline to verify watchdog metadata matches the
// actual transport bound, even when the hub has a different default.
type gateDeadlineTransport struct {
	*fakeTransport
	remaining time.Duration
}

func (f *gateDeadlineTransport) Send(ctx context.Context, msg *mcp.Message) error {
	if msg.Method == "tools/call" {
		deadline, _ := ctx.Deadline()
		f.remaining = time.Until(deadline)
	}
	return f.fakeTransport.Send(ctx, msg)
}

func TestDevboxClient_GateTimeoutMatchesCallDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Minute, 90 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			ft := &gateDeadlineTransport{fakeTransport: &fakeTransport{responses: map[string][]byte{
				"initialize": []byte(`{}`),
				"tools/call": devboxStubResult(t, devboxQualityGateResult{Passed: true}),
			}}}
			hub := newMCPHubClientWithDefaults(MCPHubConfig{CallTimeout: time.Second}, func(context.Context, string) (mcp.Transport, error) { return ft, nil })
			client := &DevboxClient{Hub: hub, GateTimeout: timeout}
			if _, err := client.QualityGate(context.Background(), pipeline.DevboxRequest{Project: "loom-core"}); err != nil {
				t.Fatal(err)
			}
			want := client.EffectiveGateTimeout()
			if ft.remaining > want || ft.remaining < want-time.Second {
				t.Fatalf("call bound = %s, watchdog bound = %s", ft.remaining, want)
			}
		})
	}
}

func TestDevboxClient_ParityDiagnostics(t *testing.T) {
	body := devboxQualityGateResult{Checks: []devboxQualityCheckRow{
		{Name: "lint:parity", ExitCode: 1, OutputTail: "handler.go: use CommandContext (noctx)\nstderr diagnostic"},
		{Name: "lint:parity", ExitCode: 1, Degraded: true, Warning: "infra warning", FailureSignature: "lint_parity_no_output"},
	}}
	ft := &fakeTransport{responses: map[string][]byte{
		"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
		"tools/call": devboxStubResult(t, body),
	}}
	resp, err := NewDevboxClient(newTestHubClient(t, ft)).QualityGate(t.Context(), pipeline.DevboxRequest{Project: "loom-core"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Checks[0].Output, "noctx") || !strings.Contains(resp.Checks[0].Output, "stderr diagnostic") || !resp.Checks[1].Degraded || resp.Checks[1].FailureSignature != "lint_parity_no_output" || resp.Checks[1].Warning != "infra warning" {
		t.Fatalf("checks=%+v", resp.Checks)
	}
}

func TestDevboxClient_StopRequiresConfirmation(t *testing.T) {
	for _, body := range []string{`{"stopped":true}`, `{"stopped":false}`, `{}`, "stopped: true\nproject: loom-core"} {
		t.Run(body, func(t *testing.T) {
			envelope, _ := json.Marshal(mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: body}}})
			ft := &fakeTransport{responses: map[string][]byte{
				"initialize": []byte(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"x","version":"1"}}`),
				"tools/call": envelope,
			}}
			c := NewDevboxClient(newTestHubClient(t, ft))
			err := c.Stop(context.Background(), "loom-core", "run-agent")
			if (err == nil) != (body == `{"stopped":true}` || strings.HasPrefix(body, "stopped: true")) {
				t.Fatalf("Stop(%s) = %v", body, err)
			}
		})
	}
}
