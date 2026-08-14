package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMRStatusHUD returns a test server that serves the branch-status endpoint
// with the given payload, plus a cleanup. The returned base URL is set as
// LOOM_HUD_URL so the CLI transport resolves to it.
func fakeMRStatusHUD(t *testing.T, payload mrStatusResponse) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.NotFound(w, r)
			return
		}
		// Echo the requested branch back so tests can assert wiring.
		resp := payload
		if b := r.URL.Query().Get("branch"); b != "" {
			resp.Branch = b
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func runMRStatus(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newAgentMRStatusCmd()
	cmd.Flags().String("port", "", "") // provided by the agent parent group in prod
	cmd.SetArgs(args)
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	return out, runErr
}

func TestMRStatus_BriefPrintsPerMR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{
			{Repo: "services/loom-core", IID: 42, State: "ci_failed_flaky", Reason: "golangci schema fetch", WebURL: "https://gl/mr/42"},
		},
		Count: 1,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/x", "--brief")
	if err != nil {
		t.Fatalf("mr-status --brief error: %v", err)
	}
	want := "!42 ci_failed_flaky golangci schema fetch https://gl/mr/42\n"
	if out != want {
		t.Fatalf("brief output = %q, want %q", out, want)
	}
}

func TestMRStatus_JSONPassthrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{{Repo: "r", IID: 7, State: "ok"}},
		Count:         1,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/x", "--json")
	if err != nil {
		t.Fatalf("mr-status --json error: %v", err)
	}
	var got mrStatusResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output not valid: %v (%q)", err, out)
	}
	if got.Count != 1 || len(got.MergeRequests) != 1 || got.MergeRequests[0].IID != 7 {
		t.Fatalf("json output = %+v, want IID 7 count 1", got)
	}
}

func TestMRStatus_UnreachableHUDSilentExitZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Point at a closed port so the request fails.
	t.Setenv("LOOM_HUD_URL", "http://127.0.0.1:1")

	out, err := runMRStatus(t, "--branch", "feat/x", "--brief")
	if err != nil {
		t.Fatalf("expected silent exit 0 on unreachable HUD, got err: %v", err)
	}
	if out != "" {
		t.Fatalf("expected no output on unreachable HUD, got %q", out)
	}
}

func TestMRStatus_UnreachableHUDJSONErrorExitOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOOM_HUD_URL", "http://127.0.0.1:1")

	out, err := runMRStatus(t, "--branch", "feat/x", "--json")
	if err == nil {
		t.Fatal("expected non-nil error (exit 1) on unreachable HUD with --json")
	}
	var obj map[string]any
	if jerr := json.Unmarshal([]byte(out), &obj); jerr != nil {
		t.Fatalf("expected JSON error object, got %q (%v)", out, jerr)
	}
	if avail, _ := obj["available"].(bool); avail {
		t.Fatalf("expected available=false in error object, got %v", obj)
	}
}

// TestMRStatus_DeltaCacheBehavior exercises the delta-gated hook path:
// first call prints, an unchanged second call is silent, a changed third
// call prints again.
func TestMRStatus_DeltaCacheBehavior(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	state := "ci_running"
	reason := ""
	// A mutable handler so we can flip the classification mid-test.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.NotFound(w, r)
			return
		}
		resp := mrStatusResponse{
			Branch: r.URL.Query().Get("branch"),
			MergeRequests: []mrStatusMR{
				{Repo: "services/loom-core", IID: 99, State: state, Reason: reason, WebURL: "https://gl/mr/99"},
			},
			Count: 1,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	// First call: no cache → prints.
	out1, err := runMRStatus(t, "--branch", "feat/delta", "--brief", "--delta")
	if err != nil {
		t.Fatalf("first delta call error: %v", err)
	}
	if !strings.Contains(out1, "!99 ci_running") {
		t.Fatalf("first delta call should print, got %q", out1)
	}

	// Cache file should now exist under the sanitized branch name.
	statePath := filepath.Join(home, ".loom", "mrwatch", "feat-delta.state")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected state cache at %s: %v", statePath, err)
	}

	// Second call, unchanged state → silent.
	out2, err := runMRStatus(t, "--branch", "feat/delta", "--brief", "--delta")
	if err != nil {
		t.Fatalf("second delta call error: %v", err)
	}
	if out2 != "" {
		t.Fatalf("second (unchanged) delta call should be silent, got %q", out2)
	}

	// Flip classification → changed → prints again.
	state = "ci_failed_deterministic"
	reason = "compile error"
	out3, err := runMRStatus(t, "--branch", "feat/delta", "--brief", "--delta")
	if err != nil {
		t.Fatalf("third delta call error: %v", err)
	}
	if !strings.Contains(out3, "!99 ci_failed_deterministic compile error") {
		t.Fatalf("third (changed) delta call should print new state, got %q", out3)
	}
}

// hookEnvelope is the JSON shape both Claude (UserPromptSubmit) and Gemini
// (BeforeAgent) parse to inject prompt context.
type hookEnvelope struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func TestMRStatus_HookClaudeEnvelope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{
			{Repo: "services/loom-core", IID: 42, State: "conflict", Reason: "rebase needed", WebURL: "https://gl/mr/42"},
		},
		Count: 1,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/x", "--hook", "claude")
	if err != nil {
		t.Fatalf("mr-status --hook claude error: %v", err)
	}
	var env hookEnvelope
	if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
		t.Fatalf("hook output is not valid JSON: %v (%q)", jerr, out)
	}
	if env.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", env.HookSpecificOutput.HookEventName)
	}
	ctx := env.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "!42 conflict rebase needed https://gl/mr/42") {
		t.Errorf("additionalContext missing MR line: %q", ctx)
	}
	if !strings.Contains(ctx, "feat/x") {
		t.Errorf("additionalContext missing branch name: %q", ctx)
	}
}

func TestMRStatus_HookGeminiEnvelope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{{Repo: "r", IID: 7, State: "automerge_unarmed", WebURL: "https://gl/mr/7"}},
		Count:         1,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/y", "--hook", "gemini")
	if err != nil {
		t.Fatalf("mr-status --hook gemini error: %v", err)
	}
	var env hookEnvelope
	if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
		t.Fatalf("hook output is not valid JSON: %v (%q)", jerr, out)
	}
	if env.HookSpecificOutput.HookEventName != "BeforeAgent" {
		t.Errorf("hookEventName = %q, want BeforeAgent", env.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(env.HookSpecificOutput.AdditionalContext, "!7 automerge_unarmed") {
		t.Errorf("additionalContext missing MR line: %q", env.HookSpecificOutput.AdditionalContext)
	}
}

// TestMRStatus_HookDeltaGated: first call emits, unchanged second call is
// silent, and the claude/gemini caches are independent (a gemini call on the
// same branch still emits on its own first sighting).
func TestMRStatus_HookDeltaGated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{{Repo: "r", IID: 5, State: "ci_failed_flaky", WebURL: "https://gl/mr/5"}},
		Count:         1,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	// Claude first call: emits.
	out1, err := runMRStatus(t, "--branch", "feat/d", "--hook", "claude")
	if err != nil || out1 == "" {
		t.Fatalf("first claude hook call should emit (err=%v out=%q)", err, out1)
	}
	// Claude second call, unchanged: silent.
	out2, err := runMRStatus(t, "--branch", "feat/d", "--hook", "claude")
	if err != nil {
		t.Fatalf("second claude hook call error: %v", err)
	}
	if out2 != "" {
		t.Fatalf("second (unchanged) claude hook call should be silent, got %q", out2)
	}
	// Gemini on the SAME branch uses an independent cache scope → still emits.
	out3, err := runMRStatus(t, "--branch", "feat/d", "--hook", "gemini")
	if err != nil || out3 == "" {
		t.Fatalf("gemini hook call should emit on its own first sighting (err=%v out=%q)", err, out3)
	}
}

// TestMRStatus_HookNoMRsSilent: zero open MRs → nothing injected.
func TestMRStatus_HookNoMRsSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{MergeRequests: nil, Count: 0}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/none", "--hook", "claude")
	if err != nil {
		t.Fatalf("hook call error: %v", err)
	}
	if out != "" {
		t.Fatalf("no-MR hook call should be silent, got %q", out)
	}
}

// TestMRStatus_HookUnreachableSilent: HUD down → silent exit 0 even though a
// stray --json is also passed (hook mode must force hook-safety).
func TestMRStatus_HookUnreachableSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOOM_HUD_URL", "http://127.0.0.1:1")

	out, err := runMRStatus(t, "--branch", "feat/x", "--hook", "claude", "--json")
	if err != nil {
		t.Fatalf("hook mode must exit 0 on unreachable HUD, got err: %v", err)
	}
	if out != "" {
		t.Fatalf("hook mode must print nothing on unreachable HUD, got %q", out)
	}
}

func TestMRStatusHookEnvelope_UnknownVendor(t *testing.T) {
	if _, err := mrStatusHookEnvelope("codex", "feat/x", "!1 ok\n"); err == nil {
		t.Fatal("expected error for unknown hook vendor")
	}
	got, err := mrStatusHookEnvelope("claude", "feat/x", "!1 ok -\n")
	if err != nil {
		t.Fatalf("claude envelope error: %v", err)
	}
	if !strings.Contains(got, `"hookEventName":"UserPromptSubmit"`) {
		t.Errorf("claude envelope wrong: %q", got)
	}
}

func TestSanitizeBranchForFile(t *testing.T) {
	cases := map[string]string{
		"feat/mrwatch-m2":  "feat-mrwatch-m2",
		"fix/a_b.c":        "fix-a_b.c",
		"weird::name*here": "weird--name-here",
		"":                 "_",
		"plain":            "plain",
	}
	for in, want := range cases {
		if got := sanitizeBranchForFile(in); got != want {
			t.Errorf("sanitizeBranchForFile(%q) = %q, want %q", in, got, want)
		}
	}
}

// mutableMRStatusHUD serves whatever payload *cur points at, so a test can
// simulate an MR changing state across successive hook invocations.
func mutableMRStatusHUD(t *testing.T, cur *mrStatusResponse) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.NotFound(w, r)
			return
		}
		resp := *cur
		if b := r.URL.Query().Get("branch"); b != "" {
			resp.Branch = b
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestMRStatus_HookSkipsBenignStates asserts the CLASS gate: an MR that is
// healthy or merely in progress must never interrupt a turn, even though it is
// an open MR whose state just changed. Without this gate the hook would fire on
// ordinary pipeline progress and the injected "need attention" wording would be
// a lie.
func TestMRStatus_HookSkipsBenignStates(t *testing.T) {
	for _, state := range []string{"ok", "ci_running", "awaiting_pipeline", "draft_idle", "merged"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			payload := mrStatusResponse{
				MergeRequests: []mrStatusMR{{Repo: "r", IID: 3, State: state, WebURL: "https://gl/mr/3"}},
				Count:         1,
			}
			ts := fakeMRStatusHUD(t, payload)
			t.Setenv("LOOM_HUD_URL", ts.URL)

			out, err := runMRStatus(t, "--branch", "feat/benign", "--hook", "claude")
			if err != nil {
				t.Fatalf("hook call error: %v", err)
			}
			if out != "" {
				t.Fatalf("state %q is not attention-worthy and must not inject, got %q", state, out)
			}
		})
	}
}

// TestMRStatus_HookInjectsOnlyAttentionMRs asserts a mixed set is narrowed to
// the attention-worthy members: the healthy MR must not appear in the injected
// context alongside the conflicted one.
func TestMRStatus_HookInjectsOnlyAttentionMRs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := mrStatusResponse{
		MergeRequests: []mrStatusMR{
			{Repo: "r", IID: 10, State: "ok", WebURL: "https://gl/mr/10"},
			{Repo: "r", IID: 11, State: "conflict", Reason: "rebase needed", WebURL: "https://gl/mr/11"},
			{Repo: "r", IID: 12, State: "ci_running", WebURL: "https://gl/mr/12"},
		},
		Count: 3,
	}
	ts := fakeMRStatusHUD(t, payload)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out, err := runMRStatus(t, "--branch", "feat/mixed", "--hook", "claude")
	if err != nil {
		t.Fatalf("hook call error: %v", err)
	}
	var env hookEnvelope
	if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
		t.Fatalf("hook output is not valid JSON: %v (%q)", jerr, out)
	}
	ctx := env.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "!11 conflict") {
		t.Errorf("attention MR !11 missing from injected context: %q", ctx)
	}
	for _, benign := range []string{"!10", "!12"} {
		if strings.Contains(ctx, benign) {
			t.Errorf("benign MR %s must not be injected: %q", benign, ctx)
		}
	}
}

// TestMRStatus_HookRegressionAfterRecoveryReEmits is the cache-correctness
// regression for the class gate. The delta cache must track the ATTENTION set,
// not the raw set: conflict → emit; fixed → silent (but cache moves to the
// empty-set hash); conflict again → MUST emit again. If the gate hashed the raw
// response instead, the third call would hash-match the first and the returning
// conflict would be silently swallowed.
func TestMRStatus_HookRegressionAfterRecoveryReEmits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	conflicted := mrStatusResponse{
		MergeRequests: []mrStatusMR{{Repo: "r", IID: 9, State: "conflict", Reason: "rebase needed", WebURL: "https://gl/mr/9"}},
		Count:         1,
	}
	healthy := mrStatusResponse{
		MergeRequests: []mrStatusMR{{Repo: "r", IID: 9, State: "ok", WebURL: "https://gl/mr/9"}},
		Count:         1,
	}
	cur := conflicted
	ts := mutableMRStatusHUD(t, &cur)
	t.Setenv("LOOM_HUD_URL", ts.URL)

	out1, err := runMRStatus(t, "--branch", "feat/regress", "--hook", "claude")
	if err != nil || out1 == "" {
		t.Fatalf("conflict should inject (err=%v out=%q)", err, out1)
	}

	cur = healthy
	out2, err := runMRStatus(t, "--branch", "feat/regress", "--hook", "claude")
	if err != nil {
		t.Fatalf("recovered call error: %v", err)
	}
	if out2 != "" {
		t.Fatalf("recovered MR must not inject, got %q", out2)
	}

	cur = conflicted
	out3, err := runMRStatus(t, "--branch", "feat/regress", "--hook", "claude")
	if err != nil {
		t.Fatalf("regressed call error: %v", err)
	}
	if out3 == "" {
		t.Fatal("conflict returning after a recovery MUST re-inject; the delta cache is tracking the raw set instead of the attention set")
	}
}

// TestFilterMRStatusAttention_MirrorsNotifyStates pins the attention taxonomy
// against internal/hud/mrwatch.notifyStates. If a state is added there, this
// test is the reminder to mirror it here.
func TestFilterMRStatusAttention_MirrorsNotifyStates(t *testing.T) {
	want := []string{"conflict", "ci_failed_flaky", "ci_failed_deterministic", "automerge_unarmed", "pipeline_skipped", "stale_branch"}
	if len(mrStatusAttentionStates) != len(want) {
		t.Fatalf("attention set size = %d, want %d (keep in sync with mrwatch/notifier.go notifyStates)", len(mrStatusAttentionStates), len(want))
	}
	for _, s := range want {
		if _, ok := mrStatusAttentionStates[s]; !ok {
			t.Errorf("state %q missing from mrStatusAttentionStates", s)
		}
	}
	// Benign states must be absent.
	for _, s := range []string{"ok", "ci_running", "awaiting_pipeline", "draft_idle", "merged"} {
		if _, ok := mrStatusAttentionStates[s]; ok {
			t.Errorf("benign state %q must not be attention-worthy", s)
		}
	}
}
