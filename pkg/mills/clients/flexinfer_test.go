package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// roundTripFn turns a function into an http.RoundTripper for stubbing
// the FlexInfer proxy without standing up a listener.
type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newStubClient(t *testing.T, body string, status int) *FlexInferClient {
	t.Helper()
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/chat/completions" {
			return nil, errors.New("unexpected path: " + req.URL.Path)
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     make(http.Header),
		}, nil
	}))
	return cli
}

const successBody = `{
  "model": "qwen3-8b-instruct",
  "choices": [
    {"message": {"role": "assistant", "content": "Here is my verdict: {\"score\": 0.85, \"reasons\": [\"covers requirement A\", \"covers requirement B\"]}"}}
  ],
  "usage": {"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}
}`

func TestFlexInferClient_RequiresProxyURL(t *testing.T) {
	if _, err := NewFlexInferClient(FlexInferConfig{}); err == nil {
		t.Error("expected error when ProxyURL empty")
	}
}

func TestRubricJudge_ParsesScoreEnvelope(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	judge := NewRubricJudge(cli)
	v, err := judge.Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{
		Item:         &store.BacklogItem{ID: "BL-X", Title: "x"},
		FilesChanged: []string{"foo.go"},
	})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.85 {
		t.Errorf("score = %v, want 0.85", v.Score)
	}
	if len(v.Reasons) != 2 {
		t.Errorf("reasons len = %d, want 2", len(v.Reasons))
	}
	if !strings.Contains(v.Model, "qwen") {
		t.Errorf("model id = %q, want qwen substring", v.Model)
	}
}

// Regression: the deployed Qwen 3.5 chat template emits a long visible
// "Thinking Process" before its answer unless callers explicitly disable
// thinking. Structured Mills judges have small completion budgets, so that
// preamble can consume the whole response before the JSON envelope appears.
func TestRubricJudge_DisablesThinkingForStructuredVerdict(t *testing.T) {
	var captured map[string]json.RawMessage
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "qwen35"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(successBody)),
			Header:     make(http.Header),
		}, nil
	}))

	if _, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	var kwargs struct {
		EnableThinking *bool `json:"enable_thinking"`
	}
	if err := json.Unmarshal(captured["chat_template_kwargs"], &kwargs); err != nil {
		t.Fatalf("decode chat_template_kwargs: %v", err)
	}
	if kwargs.EnableThinking == nil || *kwargs.EnableThinking {
		t.Fatalf("enable_thinking = %v, want false", kwargs.EnableThinking)
	}
}

func TestStructuredChat_ChatTemplateKwargsOnlyForLocalModels(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		wantKwargs bool
	}{
		{name: "local qwen", model: "qwen3-32b", wantKwargs: true},
		{name: "OpenAI gateway", model: "oa/gpt-5.6-luna", wantKwargs: false},
		{name: "OpenRouter gateway", model: "or/kimi-k3", wantKwargs: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured map[string]json.RawMessage
			cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: tt.model})
			if err != nil {
				t.Fatal(err)
			}
			cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
					t.Fatalf("decode marshalled request: %v", err)
				}
				body := `{"model":"` + tt.model + `","choices":[{"message":{"role":"assistant","content":"ok"}}]}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			}))

			if _, _, err := cli.ChatStructured(context.Background(), tt.model, "review this", 64); err != nil {
				t.Fatalf("ChatStructured: %v", err)
			}
			_, gotKwargs := captured["chat_template_kwargs"]
			if gotKwargs != tt.wantKwargs {
				t.Errorf("chat_template_kwargs present = %v, want %v; request = %v", gotKwargs, tt.wantKwargs, captured)
			}
		})
	}
}

func TestChatKeepsFreeFormTemplateDefaults(t *testing.T) {
	var captured map[string]json.RawMessage
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", WeaverModel: "qwen35"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body := `{"model":"qwen35","choices":[{"message":{"role":"assistant","content":"free-form response"}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}))

	if _, _, err := cli.Chat(context.Background(), "qwen35", "review this", 64); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := captured["chat_template_kwargs"]; ok {
		t.Fatalf("free-form Chat unexpectedly set chat_template_kwargs: %s", captured["chat_template_kwargs"])
	}
}

func TestRubricJudge_FencedJSONBlock(t *testing.T) {
	// Build a properly-encoded body: model output is fenced JSON
	// embedded inside a chat completion's content field.
	content := "Reasoning...\n```json\n{\"score\": 0.4, \"reasons\": [\"missing tests\"]}\n```\nDone."
	resp := chatResponse{Model: "x"}
	resp.Choices = append(resp.Choices, chatChoice{Message: chatMessage{Role: "assistant", Content: content}})
	bodyBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cli := newStubClient(t, string(bodyBytes), 200)
	v, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.4 {
		t.Errorf("score = %v", v.Score)
	}
}

func TestRubricJudge_ScoreOutOfRangeErrors(t *testing.T) {
	body := `{"model": "x", "choices": [{"message": {"content": "{\"score\": 1.5, \"reasons\": []}"}}]}`
	cli := newStubClient(t, body, 200)
	if _, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{}); err == nil {
		t.Error("expected error for score > 1")
	}
}

// M2.5: parser errors must wrap ErrRubricUnparseable (sentinel) AND
// implement the duck-typed IsRubricUnparseable() bool predicate. The
// gates.LLMGate uses the duck-type from across the package boundary to
// route the failure into the runner's retry path instead of escalation.
func TestParseRubricEnvelope_WrapsSentinelOnNoJSON(t *testing.T) {
	_, _, err := parseRubricEnvelope("please provide the diff and I will judge it")
	if err == nil {
		t.Fatal("expected error for free-text response")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("error chain must include ErrRubricUnparseable: %v", err)
	}
	type unparseable interface {
		IsRubricUnparseable() bool
	}
	var u unparseable
	if !errors.As(err, &u) || !u.IsRubricUnparseable() {
		t.Errorf("error must implement duck-typed IsRubricUnparseable() predicate: %v", err)
	}
}

func TestParseRubricEnvelope_WrapsSentinelOnOutOfRangeScore(t *testing.T) {
	_, _, err := parseRubricEnvelope(`{"score": 2.5, "reasons": []}`)
	if err == nil {
		t.Fatal("expected error for score > 1")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("out-of-range score must also wrap ErrRubricUnparseable for retry routing: %v", err)
	}
}

func TestParseRubricEnvelope_SkipsExampleBeforeFinalVerdict(t *testing.T) {
	raw := "Thinking Process:\nExpected shape: {\"score\": \"number\"}\n" +
		"Final: {\"score\": 0.93, \"reasons\": [\"matches the requested scope\"]}"
	score, reasons, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.93 || len(reasons) != 1 {
		t.Fatalf("score=%v reasons=%v, want 0.93 and one reason", score, reasons)
	}
}

// Verify the full Judge call surfaces the duck-typed predicate even
// after Judge's own fmt.Errorf("rubric judge: parse: %w; raw=%q", ...)
// wrap layer. This is the path the production gate consumes.
func TestRubricJudge_UnparseableContentExposesPredicateAcrossWrap(t *testing.T) {
	body := `{"model": "x", "choices": [{"message": {"content": "I cannot grade this without more context."}}]}`
	cli := newStubClient(t, body, 200)
	_, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("Judge wrap must preserve ErrRubricUnparseable in chain: %v", err)
	}
	type unparseable interface {
		IsRubricUnparseable() bool
	}
	var u unparseable
	if !errors.As(err, &u) || !u.IsRubricUnparseable() {
		t.Errorf("Judge wrap must preserve IsRubricUnparseable() predicate: %v", err)
	}
}

func TestRubricJudge_HTTP500BubblesError(t *testing.T) {
	cli := newStubClient(t, `{"error": "model overloaded"}`, 500)
	if _, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{}); err == nil {
		t.Error("expected error on 500")
	}
}

func TestRubricJudge_NoChoicesErrors(t *testing.T) {
	cli := newStubClient(t, `{"model": "x", "choices": []}`, 200)
	if _, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{}); err == nil {
		t.Error("expected error for empty choices")
	}
}

func TestRubricJudge_PromptIncludesItemFilesAndDiff(t *testing.T) {
	var captured string
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(req.Body)
		var parsed chatRequest
		if err := json.Unmarshal(buf, &parsed); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(parsed.Messages) > 0 {
			captured = parsed.Messages[0].Content
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(successBody)),
			Header:     make(http.Header),
		}, nil
	}))
	in := gates.StageInput{
		Item:           &store.BacklogItem{ID: "BL-Y", Title: "feature Y", SpecDoc: ".loom/spec.md", SpecAnchor: "phase-1"},
		FilesChanged:   []string{"a.go", "b.go"},
		LinesAdded:     12,
		LinesRemoved:   3,
		DiffPatch:      []byte("diff --git a/a.go b/a.go\n+x\n"),
		CommitMessages: []string{"feat: add y", "test: add y test"},
	}
	if _, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", in); err != nil {
		t.Fatalf("judge: %v", err)
	}
	for _, want := range []string{"BL-Y", "feature Y", ".loom/spec.md", "phase-1", "a.go", "b.go", "+12 / -3", "feat: add y"} {
		if !strings.Contains(captured, want) {
			t.Errorf("prompt missing %q; got=\n%s", want, captured)
		}
	}
}

func TestRubricJudge_DiffTruncatedAtCap(t *testing.T) {
	var captured string
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(req.Body)
		var parsed chatRequest
		_ = json.Unmarshal(buf, &parsed)
		if len(parsed.Messages) > 0 {
			captured = parsed.Messages[0].Content
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(successBody)),
			Header:     make(http.Header),
		}, nil
	}))
	// 40 KiB of newline-terminated lines so the cap (32 KiB) fires and the
	// line-boundary cut has a newline to land on.
	bigDiff := bytes.Repeat([]byte("+ a changed line of code\n"), 40*1024/25+1)
	if _, err := NewRubricJudge(cli).Judge(context.Background(), "spec_conformance_v1", gates.StageInput{DiffPatch: bigDiff}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !strings.Contains(captured, "(truncated)") {
		t.Error("expected truncation marker for oversize diff")
	}
	if !strings.Contains(captured, "TRUNCATED for prompt length by the pipeline harness") {
		t.Error("expected harness-truncation note for oversize diff")
	}
}

func TestComposePrompt_DiffUnderCapNotTruncated(t *testing.T) {
	in := gates.StageInput{DiffPatch: bytes.Repeat([]byte("+ line\n"), 12*1024/7)}
	prompt := composePrompt("rubric", in)
	if strings.Contains(prompt, "(truncated)") || strings.Contains(prompt, "TRUNCATED") {
		t.Error("a 12KiB diff must fit the 32KiB judge cap untruncated (the old 8KiB cap false-failed real slices; escalation #301)")
	}
}

func TestComposePrompt_TruncationCutsOnLineBoundary(t *testing.T) {
	in := gates.StageInput{DiffPatch: bytes.Repeat([]byte("+ a changed line of code\n"), 40*1024/25+1)}
	prompt := composePrompt("rubric", in)
	// Every rendered diff line must be intact: the cut may drop lines but
	// never tear one in half.
	body := prompt[strings.Index(prompt, "```diff\n")+len("```diff\n"):]
	body = body[:strings.Index(body, "\n... (truncated) ...")]
	for i, line := range strings.Split(body, "\n") {
		if line != "+ a changed line of code" && line != "" {
			t.Fatalf("line %d torn by truncation: %q", i, line)
		}
	}
}

func TestComposePrompt_UpstreamTruncationMarkerGetsNote(t *testing.T) {
	for _, marker := range []string{"\n… [diff truncated]\n", "\n... [truncated 4096 bytes]\n"} {
		in := gates.StageInput{DiffPatch: []byte("diff --git a/a.go b/a.go\n+x\n" + marker)}
		prompt := composePrompt("rubric", in)
		if !strings.Contains(prompt, "TRUNCATED for prompt length by the pipeline harness") {
			t.Errorf("upstream marker %q: expected harness-truncation note", marker)
		}
	}
}

// ----- Rubric template snapshot tests (M9) -----
//
// These pin the prompt hygiene fixes that ship the anti-hallucination
// boilerplate, the structural-output envelope, and the explicit
// empty-diff placeholder. They are template-level assertions, not
// LLM-roundtrip tests — gemma's actual verdict quality on a fixture
// corpus is a separate eval slice.

// captureRubricPrompt renders the rubric prompt by sending a stub chat
// completion through the FlexInfer client and capturing the message
// content that hit the wire. Returns the exact string the model would
// see.
func captureRubricPrompt(t *testing.T, rubric string, in gates.StageInput) string {
	t.Helper()
	var captured string
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(req.Body)
		var parsed chatRequest
		if err := json.Unmarshal(buf, &parsed); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(parsed.Messages) > 0 {
			captured = parsed.Messages[0].Content
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(successBody)),
			Header:     make(http.Header),
		}, nil
	}))
	if _, err := NewRubricJudge(cli).Judge(context.Background(), rubric, in); err != nil {
		t.Fatalf("judge: %v", err)
	}
	return captured
}

// TestRubricTemplate_PRSelfReviewIncludesGroundingInstructions pins the
// anti-hallucination phrase in the pr_self_review template render. Live
// regression: gemma4-26b fabricated "file.py:10 - debug print found"
// against a markdown-only diff. The grounding boilerplate is the fix.
func TestRubricTemplate_PRSelfReviewIncludesGroundingInstructions(t *testing.T) {
	rendered := captureRubricPrompt(t, gates.PRSelfReviewRubricName, gates.StageInput{
		DiffPatch: []byte("diff --git a/.loom/heartbeat.md b/.loom/heartbeat.md\n+HEARTBEAT-123\n"),
	})
	// Anchor on the load-bearing phrases — anything less and the
	// snapshot test wouldn't catch a silent rewrite that removes the
	// grounding requirement.
	for _, want := range []string{
		gates.RubricGroundingInstructions,
		"Ground every concern in EXACTLY ONE specific line",
		"Do NOT reference files, symbols, line numbers, or behaviors that are not present in the diff",
		// The partial-view / no-compile-claims clause (escalation #304: the
		// judge fabricated "undefined: ClassInfra" for package-local symbols
		// defined outside the diff).
		"The diff is a PARTIAL view of an existing repository",
		`NEVER report build/compile concerns such as "undefined symbol"`,
		// The as-written / no-speculation clause (escalation #309: the judge
		// failed correct SQL for being "fragile … if the string concatenation
		// is modified" — grading a hypothetical future edit).
		"Score ONLY defects that exist in the diff AS WRITTEN",
		"If your reason concedes the code currently behaves correctly, it is not a scorable concern",
		"return a score of 1.0 (no scorable concerns)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("pr_self_review template missing %q\n--- rendered ---\n%s", want, rendered)
		}
	}
}

// TestRubricTemplate_TestsPassedRendersVerifiedNote pins the deterministic
// tests-stage verdict line: when the devbox quality gate passed, the judge
// prompt must say so (the rubric promises test_results; without the line the
// judge graded compile health blind — escalation #304's "undefined symbol"
// false-fails on code whose full-repo build was already green). And when the
// stage did not run, the line must be absent — false means "didn't run",
// never "failed".
func TestRubricTemplate_TestsPassedRendersVerifiedNote(t *testing.T) {
	const note = "Tests stage: PASSED"
	with := captureRubricPrompt(t, gates.PRSelfReviewRubricName, gates.StageInput{
		DiffPatch:   []byte("diff --git a/foo.go b/foo.go\n+package foo\n"),
		TestsPassed: true,
	})
	if !strings.Contains(with, note) {
		t.Errorf("TestsPassed=true render missing %q\n--- rendered ---\n%s", note, with)
	}
	if !strings.Contains(with, "Treat build/compile health as verified") {
		t.Errorf("TestsPassed=true render missing the verified-health instruction\n--- rendered ---\n%s", with)
	}
	without := captureRubricPrompt(t, gates.PRSelfReviewRubricName, gates.StageInput{
		DiffPatch: []byte("diff --git a/foo.go b/foo.go\n+package foo\n"),
	})
	if strings.Contains(without, note) {
		t.Errorf("TestsPassed=false render must NOT claim a tests verdict\n--- rendered ---\n%s", without)
	}
}

// TestRubricTemplate_SpecConformanceIncludesStructuralInstructions pins
// the JSON-envelope instructions in the spec_conformance template.
// Live regression: spec_conformance returned judged_by=flexinfer:
// unparseable because gemma replied with prose, ignoring the format ask.
// The structural-output instructions reinforce the contract that
// parseRubricEnvelope consumes.
func TestRubricTemplate_SpecConformanceIncludesStructuralInstructions(t *testing.T) {
	rendered := captureRubricPrompt(t, gates.SpecConformanceRubricName, gates.StageInput{
		DiffPatch: []byte("diff --git a/foo.go b/foo.go\n+package foo\n"),
	})
	for _, want := range []string{
		gates.RubricStructuralOutputInstructions,
		"Respond ONLY with a JSON object",
		`{"score": <number between 0.0 and 1.0>, "reasons": ["<one concern per array entry>"]}`,
		"Do not include any text outside the JSON object",
		"Do not ask clarifying questions",
		`{"score": 1.0, "reasons": ["fixture-only or empty diff; no scorable concerns"]}`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("spec_conformance template missing %q\n--- rendered ---\n%s", want, rendered)
		}
	}
	// Also assert grounding is present — both rubrics share it.
	if !strings.Contains(rendered, gates.RubricGroundingInstructions) {
		t.Errorf("spec_conformance template missing grounding instructions\n--- rendered ---\n%s", rendered)
	}
}

// TestRubricTemplate_EmptyDiffRendersExplicitPlaceholder pins the
// `(empty diff)` placeholder behavior. Without the placeholder the
// `=== Diff ===` section would be omitted entirely and the model would
// be more likely to fall back to its prior context and fabricate
// references — defeating the grounding instructions.
func TestRubricTemplate_EmptyDiffRendersExplicitPlaceholder(t *testing.T) {
	rendered := captureRubricPrompt(t, gates.SpecConformanceRubricName, gates.StageInput{
		// No DiffPatch.
	})
	if !strings.Contains(rendered, "=== Diff ===") {
		t.Errorf("empty-diff render must still include `=== Diff ===` section\n--- rendered ---\n%s", rendered)
	}
	if !strings.Contains(rendered, "(empty diff)") {
		t.Errorf("empty-diff render must include explicit `(empty diff)` placeholder\n--- rendered ---\n%s", rendered)
	}
}

// TestRubricTemplate_NonEmptyDiffSkipsPlaceholder is the symmetric
// guard: when a real diff is present, the placeholder must NOT appear
// or the model would see a self-contradictory section.
func TestRubricTemplate_NonEmptyDiffSkipsPlaceholder(t *testing.T) {
	rendered := captureRubricPrompt(t, gates.SpecConformanceRubricName, gates.StageInput{
		DiffPatch: []byte("diff --git a/foo.go b/foo.go\n+x\n"),
	})
	if strings.Contains(rendered, "(empty diff)") {
		t.Errorf("non-empty diff must not render `(empty diff)` placeholder\n--- rendered ---\n%s", rendered)
	}
	if !strings.Contains(rendered, "diff --git a/foo.go b/foo.go") {
		t.Errorf("non-empty diff must render the actual patch\n--- rendered ---\n%s", rendered)
	}
}

// ----- WeaverClient -----

func TestWeaverClient_ReturnsNotesAndCitation(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{
		BacklogID: "BL-W",
		Prompt:    "research how X works",
	})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if !strings.Contains(resp.Notes, "verdict") {
		t.Errorf("notes did not contain model output: %q", resp.Notes)
	}
	if resp.Citation["model"] == "" {
		t.Errorf("citation.model missing: %+v", resp.Citation)
	}
	if resp.CostUSD <= 0 {
		t.Errorf("cost should be > 0 for non-empty usage: %v", resp.CostUSD)
	}
}

func TestWeaverClient_NilClientErrors(t *testing.T) {
	w := &WeaverClient{}
	if _, err := w.Research(context.Background(), pipeline.WeaverRequest{}); err == nil {
		t.Error("expected error for nil client")
	}
}

// ----- ResearchMode switch (S5 / MW-001/002/003) -----

// fakeDelegator captures Delegate inputs and returns a canned response.
type fakeDelegator struct {
	calls   int
	resp    pipeline.WeaverResponse
	err     error
	lastReq pipeline.WeaverRequest
}

func (f *fakeDelegator) Delegate(_ context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error) {
	f.calls++
	f.lastReq = req
	return f.resp, f.err
}

// fakeDiffRecorder captures recorded diffs for assertion.
type fakeDiffRecorder struct {
	calls     int
	last      map[string]any
	runID     string
	backlogID string
}

func (f *fakeDiffRecorder) Record(_ context.Context, runID, backlogID string, diff map[string]any) {
	f.calls++
	f.runID = runID
	f.backlogID = backlogID
	f.last = diff
}

func TestParseResearchMode(t *testing.T) {
	cases := map[string]ResearchMode{
		"":        ResearchModeOff,
		"off":     ResearchModeOff,
		"  Off  ": ResearchModeOff,
		"shadow":  ResearchModeShadow,
		"SHADOW":  ResearchModeShadow,
		"on":      ResearchModeOn,
		"oN":      ResearchModeOn,
		"bogus":   ResearchModeOff,
		"weaver":  ResearchModeOff,
	}
	for input, want := range cases {
		if got := ParseResearchMode(input); got != want {
			t.Errorf("ParseResearchMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWeaverClient_ResearchModeOff_UsesLegacy(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOff
	delegator := &fakeDelegator{resp: pipeline.WeaverResponse{Notes: "delegated"}}
	w.Delegator = delegator

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if delegator.calls != 0 {
		t.Errorf("delegator should not be called in off mode (calls=%d)", delegator.calls)
	}
	if !strings.Contains(resp.Notes, "verdict") {
		t.Errorf("expected legacy notes, got %q", resp.Notes)
	}
}

func TestWeaverClient_ResearchModeOn_UsesDelegator(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOn
	delegator := &fakeDelegator{
		resp: pipeline.WeaverResponse{
			Notes:   "delegated answer",
			SpawnID: "weaver-router",
			CostUSD: 0.001,
		},
	}
	w.Delegator = delegator

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{
		BacklogID: "BL-1", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if delegator.calls != 1 {
		t.Fatalf("delegator should be called once (got %d)", delegator.calls)
	}
	if delegator.lastReq.BacklogID != "BL-1" {
		t.Errorf("delegator got BacklogID=%q, want BL-1", delegator.lastReq.BacklogID)
	}
	if resp.Notes != "delegated answer" {
		t.Errorf("expected delegated notes, got %q", resp.Notes)
	}
}

func TestWeaverClient_ResearchModeOn_FallsBackOnDelegatorError(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOn
	delegator := &fakeDelegator{err: errors.New("delegator down")}
	w.Delegator = delegator

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("research should fall back, not error: %v", err)
	}
	if delegator.calls != 1 {
		t.Errorf("delegator should be tried once before fallback (got %d)", delegator.calls)
	}
	if !strings.Contains(resp.Notes, "verdict") {
		t.Errorf("expected legacy fallback notes, got %q", resp.Notes)
	}
}

func TestWeaverClient_ResearchModeOn_NoDelegatorFallsBack(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOn
	// No delegator configured.

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if !strings.Contains(resp.Notes, "verdict") {
		t.Errorf("expected legacy fallback notes when delegator absent, got %q", resp.Notes)
	}
}

func TestWeaverClient_ResearchModeOn_ContextCancelPropagates(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOn
	delegator := &fakeDelegator{err: context.Canceled}
	w.Delegator = delegator

	_, err := w.Research(context.Background(), pipeline.WeaverRequest{Prompt: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("context.Canceled should propagate, got %v", err)
	}
}

func TestWeaverClient_ResearchModeShadow_RecordsDiff(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeShadow
	delegator := &fakeDelegator{
		resp: pipeline.WeaverResponse{Notes: "x", CostUSD: 0.002},
	}
	rec := &fakeDiffRecorder{}
	w.Delegator = delegator
	w.DiffRecorder = rec

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{
		RunID: "RUN-9", BacklogID: "BL-9", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	// Returns the legacy result, not the shadow.
	if !strings.Contains(resp.Notes, "verdict") {
		t.Errorf("shadow mode must return legacy notes, got %q", resp.Notes)
	}
	if rec.calls != 1 {
		t.Fatalf("diff recorder should be called once (got %d)", rec.calls)
	}
	if rec.runID != "RUN-9" {
		t.Errorf("diff run_id = %q, want RUN-9", rec.runID)
	}
	if rec.backlogID != "BL-9" {
		t.Errorf("diff backlog_id = %q, want BL-9", rec.backlogID)
	}
	if rec.last["run_id"] != "RUN-9" {
		t.Errorf("diff[run_id] = %v, want RUN-9", rec.last["run_id"])
	}
	if rec.last["backlog_id"] != "BL-9" {
		t.Errorf("diff[backlog_id] = %v, want BL-9", rec.last["backlog_id"])
	}
	if rec.last["legacy_chars"].(int) <= 0 {
		t.Errorf("legacy_chars should be > 0: %v", rec.last["legacy_chars"])
	}
	if rec.last["shadow_chars"].(int) != len("x") {
		t.Errorf("shadow_chars = %v, want 1", rec.last["shadow_chars"])
	}
}

func TestWeaverClient_ResearchModeShadow_NoDelegatorRecordsError(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeShadow
	rec := &fakeDiffRecorder{}
	w.DiffRecorder = rec
	// No delegator.

	if _, err := w.Research(context.Background(), pipeline.WeaverRequest{BacklogID: "BL-X"}); err != nil {
		t.Fatalf("research: %v", err)
	}
	if rec.last["shadow_error"] == nil {
		t.Errorf("shadow_error should be recorded when no delegator (got %+v)", rec.last)
	}
}

// chatBody renders a FlexInfer chat-completion response whose assistant
// message carries content — used to feed the research stage canned
// (possibly hallucinated) notes.
func chatBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "gemma4-26b-a4b-gptq",
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": content}},
		},
		"usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 60, "total_tokens": 100},
	})
	return string(b)
}

// TestWeaverClient_Research_WithholdsHallucinatedPaths reproduces
// PIPE-MILLS-2026-06-29-001: the legacy (research_mode=off) path returns
// notes describing a non-existent Python codebase for a Go repo. With
// RepoRoot pointing at a real checkout, the guard must withhold them.
func TestWeaverClient_Research_WithholdsHallucinatedPaths(t *testing.T) {
	hallucinated := "The system splits into mills/core/council/orchestrator.py, " +
		"mills/core/agent/execution_engine.py, mills/telemetry/metrics_collector.py, " +
		"and mills/state/transition_manager.py."
	cli := newStubClient(t, chatBody(hallucinated), 200)
	w := NewWeaverClient(cli)
	w.Mode = ResearchModeOff
	w.RepoRoot = t.TempDir() // real checkout, but none of those paths exist

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{BacklogID: "MILLS-2026-06-29-001"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if !strings.Contains(resp.Notes, "withheld") {
		t.Errorf("expected notes withheld, got:\n%q", resp.Notes)
	}
	if strings.Contains(resp.Notes, ".py") {
		t.Errorf("fabricated paths leaked into notes:\n%q", resp.Notes)
	}
	dropped, ok := resp.Citation["paths_dropped"].([]string)
	if !ok || len(dropped) != 4 {
		t.Errorf("citation paths_dropped = %v, want 4 paths", resp.Citation["paths_dropped"])
	}
}

// TestWeaverClient_Research_IncrementsGuardMetric confirms the
// observability wiring: a wholesale hallucination bumps the "withheld"
// guard counter and the dropped-paths counter, so the guard's prod
// behavior is visible at /metrics (not silently green).
func TestWeaverClient_Research_IncrementsGuardMetric(t *testing.T) {
	withheld := mills.ResearchNotesGuardTotal.WithLabelValues("withheld")
	before := testutil.ToFloat64(withheld)
	beforePaths := testutil.ToFloat64(mills.ResearchPathsDroppedTotal)

	hallucinated := "See mills/a/ghost_one.py and mills/b/ghost_two.py and mills/c/ghost_three.py."
	cli := newStubClient(t, chatBody(hallucinated), 200)
	w := NewWeaverClient(cli)
	w.RepoRoot = t.TempDir()

	if _, err := w.Research(context.Background(), pipeline.WeaverRequest{BacklogID: "BL-METRIC"}); err != nil {
		t.Fatalf("research: %v", err)
	}
	if got := testutil.ToFloat64(withheld) - before; got != 1 {
		t.Errorf("withheld counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(mills.ResearchPathsDroppedTotal) - beforePaths; got != 3 {
		t.Errorf("dropped-paths counter delta = %v, want 3", got)
	}
}

// TestWeaverClient_Research_KeepsRealPaths confirms the guard is inert
// when every referenced path exists in the checkout.
func TestWeaverClient_Research_KeepsRealPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "mills", "pipeline"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "mills", "pipeline", "runner.go"), []byte("package pipeline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	grounded := "Implementation centers on pkg/mills/pipeline/runner.go."
	cli := newStubClient(t, chatBody(grounded), 200)
	w := NewWeaverClient(cli)
	w.RepoRoot = root

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{BacklogID: "BL-OK"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if resp.Notes != grounded {
		t.Errorf("grounded notes mutated:\n got=%q\nwant=%q", resp.Notes, grounded)
	}
	if _, ok := resp.Citation["paths_dropped"]; ok {
		t.Errorf("no paths should be dropped, citation=%v", resp.Citation)
	}
}

// TestWeaverClient_Research_NoRepoRootSkipsGuard confirms that without a
// validation root the notes pass through untouched (degraded, not
// broken).
func TestWeaverClient_Research_NoRepoRootSkipsGuard(t *testing.T) {
	hallucinated := "See mills/core/council/orchestrator.py and mills/state/transition_manager.py."
	cli := newStubClient(t, chatBody(hallucinated), 200)
	w := NewWeaverClient(cli)
	w.RepoRoot = "" // no grounding source

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{BacklogID: "BL-NOROOT"})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	if resp.Notes != hallucinated {
		t.Errorf("notes should pass through unguarded, got:\n%q", resp.Notes)
	}
}

// TestWeaverClient_Research_WorktreeEnvOverridesRepoRoot confirms the
// run's worktree (req.Env) is preferred over the configured RepoRoot
// when validating paths.
func TestWeaverClient_Research_WorktreeEnvOverridesRepoRoot(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "pkg", "new_feature.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes := "Add logic in pkg/new_feature.go."
	cli := newStubClient(t, chatBody(notes), 200)
	w := NewWeaverClient(cli)
	w.RepoRoot = t.TempDir() // stale checkout WITHOUT the new file

	resp, err := w.Research(context.Background(), pipeline.WeaverRequest{
		BacklogID: "BL-WT",
		Env:       map[string]string{"LOOM_MILLS_WORKTREE": worktree},
	})
	if err != nil {
		t.Fatalf("research: %v", err)
	}
	// pkg/new_feature.go exists in the worktree, so it must be kept.
	if resp.Notes != notes {
		t.Errorf("worktree-valid path should be kept, got:\n%q", resp.Notes)
	}
}

// ----- Composition: gate flow end-to-end against the stub proxy -----

func TestSpecConformanceGate_AgainstFlexInferStub(t *testing.T) {
	cli := newStubClient(t, successBody, 200)
	g := gates.NewSpecConformanceGate(NewRubricJudge(cli))
	out, err := g.Evaluate(context.Background(), gates.StageInput{Item: &store.BacklogItem{ID: "BL-A"}})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !out.Pass {
		t.Errorf("expected pass at score 0.85 (threshold 0.8); reasons=%v", out.Reasons)
	}
	if !strings.HasPrefix(out.JudgedBy, "flexinfer:") {
		t.Errorf("JudgedBy = %q, want flexinfer:* prefix", out.JudgedBy)
	}
}

// ----- chatFallback (model-drift resilience, escalation #230) -----

// modelRoutingTransport answers per-model: models in `serving` return a
// success body naming the model; everything else gets the proxy's
// model-not-found 404 (the exact live shape from escalation #230).
func modelRoutingTransport(t *testing.T, serving map[string]bool, calls *[]string) roundTripFn {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		*calls = append(*calls, body.Model)
		if serving[body.Model] {
			resp := fmt.Sprintf(`{"model": %q, "choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}`, body.Model)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(resp)), Header: make(http.Header)}, nil
		}
		nf := fmt.Sprintf(`{"error":{"message":"Model '%s' not found","type":"not_found_error","param":"model","code":"model_not_found"}}`, body.Model)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(bytes.NewBufferString(nf)), Header: make(http.Header)}, nil
	}
}

func TestChatFallback_WalksChainOnModelNotFound(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:             "http://stub",
		WeaverModel:          "gemma4-26b-a4b-gptq",
		WeaverModelFallbacks: []string{"gemma4-26b-a4b-gptq-5930k", "fast-text"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls []string
	cli.SetTransport(modelRoutingTransport(t, map[string]bool{"gemma4-26b-a4b-gptq-5930k": true}, &calls))

	content, resp, err := cli.chatFallback(context.Background(), cli.cfg.WeaverModel, "hi", 16)
	if err != nil {
		t.Fatalf("chatFallback: %v", err)
	}
	if content != "ok" {
		t.Errorf("content = %q, want ok", content)
	}
	if resp.Model != "gemma4-26b-a4b-gptq-5930k" {
		t.Errorf("served by %q, want the -5930k fallback", resp.Model)
	}
	want := []string{"gemma4-26b-a4b-gptq", "gemma4-26b-a4b-gptq-5930k"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want %v (stop at first serving candidate)", calls, want)
	}
}

func TestChatFallback_AllCandidatesGone(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "gone-a",
		JudgeModelFallbacks: []string{"gone-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls []string
	cli.SetTransport(modelRoutingTransport(t, nil, &calls))
	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil {
		t.Fatal("expected error when every candidate is gone")
	}
	if !strings.Contains(err.Error(), "no candidate model deployed") || !strings.Contains(err.Error(), "gone-b") {
		t.Errorf("error should name the tried chain: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("calls = %v, want both candidates tried", calls)
	}
}

// TestChatFallback_NonDegradableErrorReturnsImmediately pins the hard-failure
// side of the status decision table: a 403 is a terminal proxy rejection (auth /
// policy) that the next candidate hits identically, so the chain must NOT walk.
// The transient upstream 5xx (500/502/503/504) DO walk — see
// TestChatFallback_WalksChainOnUpstream5xx.
func TestChatFallback_NonDegradableErrorReturnsImmediately(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "primary",
		JudgeModelFallbacks: []string{"secondary"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls int
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 403, Body: io.NopCloser(bytes.NewBufferString(`{"error":"forbidden"}`)), Header: make(http.Header)}, nil
	}))
	_, _, err = cli.chatFallback(context.Background(), "primary", "hi", 16)
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("want the 403 surfaced, got %v", err)
	}
	if errors.Is(err, pipeline.ErrModelUnavailable) {
		t.Errorf("a 403 must stay a hard failure, not model-unavailable: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (fallbacks fix drift/blips, not auth denials)", calls)
	}
}

func TestChatFallback_WalksChainOnProviderModelRouteIncompatibility(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "or/qwen-2.5-72b",
		JudgeModelFallbacks: []string{"or/kimi-k2.7-code"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	const incompatibleRouteBody = `{"error":{"message":"litellm.BadRequestError: OpenrouterException - Provider returned error","metadata":{"raw":"{\"code\":400,\"reason\":\"INVALID_REQUEST_BODY\",\"message\":\"model: qwen/qwen-2.5-72b-instruct does not support endpoint: completions\"}","provider_name":"Novita"}}}`
	var calls []string
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		calls = append(calls, body.Model)
		if body.Model == cli.cfg.JudgeModel {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(incompatibleRouteBody)), Header: make(http.Header)}, nil
		}
		resp := fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, body.Model)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	}))

	content, resp, err := cli.structuredChatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err != nil {
		t.Fatalf("structuredChatFallback: %v", err)
	}
	if content != "ok" || resp.Model != "or/kimi-k2.7-code" {
		t.Errorf("served content=%q model=%q, want ok/or/kimi-k2.7-code", content, resp.Model)
	}
	want := []string{"or/qwen-2.5-72b", "or/kimi-k2.7-code"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want %v (walk incompatible provider route to fallback)", calls, want)
	}
}

func TestChatFallback_AllProviderModelRoutesIncompatible(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "primary",
		JudgeModelFallbacks: []string{"secondary"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	const incompatibleRouteBody = `{"error":{"metadata":{"raw":"{\"reason\":\"INVALID_REQUEST_BODY\",\"message\":\"model does not support endpoint: completions\"}"}}}`
	var calls int
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(incompatibleRouteBody)), Header: make(http.Header)}, nil
	}))

	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil {
		t.Fatal("expected error when every provider model route is incompatible")
	}
	if !errors.Is(err, errModelRouteIncompatible) {
		t.Errorf("error should wrap errModelRouteIncompatible: %v", err)
	}
	if !strings.Contains(err.Error(), "no compatible candidate model route") {
		t.Errorf("error should name the exhausted route-compatible chain: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want both candidates tried", calls)
	}
}

// TestChatFallback_GenericBadRequestWalksChain pins the issue #378 contract:
// a 400 that does NOT carry the strict route-incompatible pair still walks the
// fallback chain. LiteLLM attributes a provider rejection to the ONE model it
// routed to ("Available Model Group Fallbacks=None"), so the verdict says
// nothing about the next configured candidate. Before this, the judge call
// returned on the first 400 with the chain untouched and the run parked.
func TestChatFallback_GenericBadRequestWalksChain(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "ordinary invalid request", body: `{"error":{"message":"invalid max_tokens"}}`},
		{name: "invalid request body without incompatible endpoint", body: `{"error":{"metadata":{"raw":"{\"reason\":\"INVALID_REQUEST_BODY\",\"message\":\"temperature must be between zero and two\"}"}}}`},
		{name: "endpoint phrase without provider reason", body: `{"error":{"message":"model does not support endpoint: completions"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, err := NewFlexInferClient(FlexInferConfig{
				ProxyURL:            "http://stub",
				JudgeModel:          "primary",
				JudgeModelFallbacks: []string{"secondary"},
			})
			if err != nil {
				t.Fatalf("ctor: %v", err)
			}
			var calls int
			cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			}))

			_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
			if err == nil || !strings.Contains(err.Error(), "status 400") {
				t.Fatalf("want the 400 surfaced, got %v", err)
			}
			// The exhausted-chain error must preserve the provider body so the
			// escalation names the real rejection, not a chain summary.
			if !strings.Contains(err.Error(), "every candidate model rejected the request") {
				t.Errorf("error should name the exhausted bad-request chain: %v", err)
			}
			if !errors.Is(err, errModelBadRequest) {
				t.Errorf("error should wrap errModelBadRequest: %v", err)
			}
			if calls != 2 {
				t.Errorf("calls = %d, want 2 (a provider 400 is candidate-specific; walk the chain)", calls)
			}
		})
	}
}

// TestChatFallback_GenericBadRequestServedByFallback is the recovery half of
// #378: the primary 400s, the configured alternate answers, and the judge call
// succeeds instead of erroring the gate.
func TestChatFallback_GenericBadRequestServedByFallback(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "or/kimi-k3",
		JudgeModelFallbacks: []string{"or/kimi-k2.7-code"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// The live #378 body: a LiteLLM-wrapped OpenRouter rejection with no
	// group fallbacks configured on the gateway side.
	const litellmBadRequest = `{"error":{"message":"litellm.BadRequestError: OpenrouterException - Provider returned error. Available Model Group Fallbacks=None","type":"invalid_request_error"}}`
	var calls []string
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		calls = append(calls, body.Model)
		if body.Model == cli.cfg.JudgeModel {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(litellmBadRequest)), Header: make(http.Header)}, nil
		}
		resp := fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, body.Model)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	}))

	content, resp, err := cli.structuredChatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err != nil {
		t.Fatalf("structuredChatFallback: %v", err)
	}
	if content != "ok" || resp.Model != "or/kimi-k2.7-code" {
		t.Errorf("served content=%q model=%q, want ok/or/kimi-k2.7-code", content, resp.Model)
	}
	want := []string{"or/kimi-k3", "or/kimi-k2.7-code"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want %v (walk the litellm 400 to the fallback)", calls, want)
	}
}

// TestChatFallback_RateLimitWalksChainAndParksModel pins the 429 half of the
// #378 fix: a provider rate limit is this candidate being temporarily
// unservable, so the chain walks AND the rate-limited model is parked in the
// unavailable cooldown so the run's remaining re-judges stop re-dialing it.
func TestChatFallback_RateLimitWalksChainAndParksModel(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "limited",
		JudgeModelFallbacks: []string{"alt"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.clock = func() time.Time { return now }
	cli.unavailableCooldown = 60 * time.Second

	const rateLimitBody = `{"error":{"message":"litellm.RateLimitError: RateLimitError: OpenrouterException - rate limit exceeded","type":"rate_limit_error","code":"429"}}`
	var calls []string
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		calls = append(calls, body.Model)
		if body.Model == "limited" {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(rateLimitBody)), Header: make(http.Header)}, nil
		}
		resp := fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, body.Model)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	}))

	content, resp, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err != nil {
		t.Fatalf("chatFallback: %v", err)
	}
	if content != "ok" || resp.Model != "alt" {
		t.Errorf("served content=%q model=%q, want ok/alt", content, resp.Model)
	}
	if !cli.modelInCooldown("limited") {
		t.Error("a 429'd model must be parked in the unavailable cooldown")
	}

	// Second call inside the cooldown skips the rate-limited model entirely.
	calls = nil
	if _, _, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16); err != nil {
		t.Fatalf("second chatFallback: %v", err)
	}
	if fmt.Sprint(calls) != fmt.Sprint([]string{"alt"}) {
		t.Errorf("calls = %v, want only [alt] (parked model must not be re-dialed)", calls)
	}
}

// TestChatFallback_AllCandidatesRateLimited pins the exhausted-429 shape: the
// error must wrap pipeline.ErrModelUnavailable (transient, retryable) so the
// gate's free re-judge budget and the runner's classifier both see a provider
// outage rather than a code defect.
func TestChatFallback_AllCandidatesRateLimited(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "limited-a",
		JudgeModelFallbacks: []string{"limited-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls int
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit exceeded"}}`)), Header: make(http.Header)}, nil
	}))

	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil {
		t.Fatal("expected error when every candidate is rate limited")
	}
	if !errors.Is(err, pipeline.ErrModelUnavailable) {
		t.Errorf("error should wrap pipeline.ErrModelUnavailable: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want both candidates tried", calls)
	}
}

func TestChatFallback_AdHocModelHasNoFallbacks(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "judge",
		JudgeModelFallbacks: []string{"judge-fb"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls []string
	cli.SetTransport(modelRoutingTransport(t, nil, &calls))
	if _, _, err := cli.chatFallback(context.Background(), "someone-elses-model", "hi", 16); err == nil {
		t.Fatal("expected not-found error")
	}
	if len(calls) != 1 || calls[0] != "someone-elses-model" {
		t.Errorf("calls = %v, want only the ad-hoc model (no chain)", calls)
	}
}

// ----- chatFallback (model-unavailable / 503-parked degrade, S2) -----

// parkedBody is the exact live shape the shared-GPU FlexInfer proxy returns
// when a model is parked behind a higher-priority primary (2026-07-16 sweep:
// 24 research 503s in 7d).
func parkedBody(model string) string {
	return fmt.Sprintf(`{"error":{"message":"model '%s' is parked behind a higher-priority primary","type":"service_unavailable"}}`, model)
}

// parkedRoutingTransport answers per-model: models in `serving` return a
// success body; everything else gets a 503 service_unavailable parked body.
func parkedRoutingTransport(t *testing.T, serving map[string]bool, calls *[]string) roundTripFn {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		var body chatRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		*calls = append(*calls, body.Model)
		if serving[body.Model] {
			resp := fmt.Sprintf(`{"model": %q, "choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}`, body.Model)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(resp)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(bytes.NewBufferString(parkedBody(body.Model))), Header: make(http.Header)}, nil
	}
}

func TestChatFallback_WalksChainOnServiceUnavailable(t *testing.T) {
	// A PINNED primary carries an explicit fallback chain; when it 503-parks,
	// the walk lands on the first serving alternate.
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "pinned-parked",
		JudgeModelFallbacks: []string{"alt-a", "alt-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls []string
	cli.SetTransport(parkedRoutingTransport(t, map[string]bool{"alt-a": true}, &calls))

	content, resp, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err != nil {
		t.Fatalf("chatFallback: %v", err)
	}
	if content != "ok" || resp.Model != "alt-a" {
		t.Errorf("served content=%q model=%q, want ok/alt-a", content, resp.Model)
	}
	want := []string{"pinned-parked", "alt-a"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want %v (walk parked primary → first serving alt)", calls, want)
	}
}

func TestChatFallback_AllModelsUnavailableWrapsSentinel(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "parked-a",
		JudgeModelFallbacks: []string{"parked-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls []string
	cli.SetTransport(parkedRoutingTransport(t, nil, &calls))

	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil {
		t.Fatal("expected error when every candidate is parked")
	}
	// Must classify as model-unavailable (transient/retryable), not the
	// "no candidate model deployed" (code) drift error.
	if !errors.Is(err, pipeline.ErrModelUnavailable) {
		t.Errorf("error should wrap pipeline.ErrModelUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "all candidate models unavailable") {
		t.Errorf("error should name the all-unavailable case: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("calls = %v, want both candidates tried on the first pass", calls)
	}
}

func TestChatFallback_CooldownSkipsParkedModel(t *testing.T) {
	// Deterministic clock so the cooldown window is exercised without sleeps.
	now := time.Unix(1_000_000, 0)
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "parked-a",
		JudgeModelFallbacks: []string{"alt-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	cli.clock = func() time.Time { return now }
	cli.unavailableCooldown = 60 * time.Second

	var calls []string
	cli.SetTransport(parkedRoutingTransport(t, map[string]bool{"alt-b": true}, &calls))

	// First call: parked-a 503-parks (recorded in cooldown), alt-b serves.
	if _, _, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16); err != nil {
		t.Fatalf("first chatFallback: %v", err)
	}
	// Second call within the cooldown window: parked-a is skipped, not re-dialed.
	calls = nil
	if _, _, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16); err != nil {
		t.Fatalf("second chatFallback: %v", err)
	}
	if fmt.Sprint(calls) != fmt.Sprint([]string{"alt-b"}) {
		t.Errorf("within cooldown calls = %v, want only [alt-b] (parked-a skipped)", calls)
	}

	// After the cooldown elapses, parked-a is re-dialed again.
	now = now.Add(61 * time.Second)
	calls = nil
	if _, _, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16); err != nil {
		t.Fatalf("post-cooldown chatFallback: %v", err)
	}
	if fmt.Sprint(calls) != fmt.Sprint([]string{"parked-a", "alt-b"}) {
		t.Errorf("post-cooldown calls = %v, want [parked-a alt-b] (cooldown expired)", calls)
	}
}

// ----- chatFallback (raw upstream 5xx degrade) -----

// TestChatFallback_WalksChainOnUpstream5xx pins the fix for the 2026-07-26
// audit: 3 research escalations whose entire log_tail was
// `stage=research attempt=1: flexinfer chat: status 500: Internal Server Error`.
// A raw 5xx carries none of the 503 body needles, so before this it matched no
// fallback signature and returned straight out with the chain untouched. Each
// transient upstream status must now park the candidate and walk on, exactly
// like a 503 park.
func TestChatFallback_WalksChainOnUpstream5xx(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{500, "Internal Server Error"}, // the live research escalation shape
		{502, "Bad Gateway"},
		{504, "Gateway Timeout"},
	} {
		t.Run(fmt.Sprintf("status%d", tc.status), func(t *testing.T) {
			cli, err := NewFlexInferClient(FlexInferConfig{
				ProxyURL:            "http://stub",
				JudgeModel:          "erroring",
				JudgeModelFallbacks: []string{"alt"},
			})
			if err != nil {
				t.Fatalf("ctor: %v", err)
			}
			var calls []string
			cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
				var body chatRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				calls = append(calls, body.Model)
				if body.Model == "erroring" {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
				}
				resp := fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, body.Model)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
			}))

			content, resp, err := cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
			if err != nil {
				t.Fatalf("chatFallback: %v", err)
			}
			if content != "ok" || resp.Model != "alt" {
				t.Errorf("served content=%q model=%q, want ok/alt", content, resp.Model)
			}
			want := []string{"erroring", "alt"}
			if fmt.Sprint(calls) != fmt.Sprint(want) {
				t.Errorf("calls = %v, want %v (walk 5xx primary → serving alt)", calls, want)
			}
			if !cli.modelInCooldown("erroring") {
				t.Errorf("a %d'd model must be parked in the unavailable cooldown", tc.status)
			}
		})
	}
}

// TestChatFallback_AllCandidates500WrapsSentinel: when every candidate returns
// the raw 500, the chain must surface pipeline.ErrModelUnavailable (so the
// research stage soft-skips and Classify maps it to transient) while preserving
// the real status + body in the message.
func TestChatFallback_AllCandidates500WrapsSentinel(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "erroring-a",
		JudgeModelFallbacks: []string{"erroring-b"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls int
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("Internal Server Error")), Header: make(http.Header)}, nil
	}))

	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil {
		t.Fatal("expected error when every candidate 500s")
	}
	if !errors.Is(err, pipeline.ErrModelUnavailable) {
		t.Errorf("error should wrap pipeline.ErrModelUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "all candidate models unavailable") {
		t.Errorf("error should name the all-unavailable case: %v", err)
	}
	// Error-message fidelity: the operator must still see the real upstream
	// status and body, not just the sentinel.
	if !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("error should preserve the upstream status + body: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want both candidates tried", calls)
	}
}

// TestChatFallback_501NotImplementedStaysHard guards the boundary of
// isTransientUpstreamStatus: a 501 is a statement about the endpoint, not a
// per-model blip, so the next candidate hits it identically.
func TestChatFallback_501NotImplementedStaysHard(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:            "http://stub",
		JudgeModel:          "primary",
		JudgeModelFallbacks: []string{"secondary"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var calls int
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 501, Body: io.NopCloser(strings.NewReader("Not Implemented")), Header: make(http.Header)}, nil
	}))
	_, _, err = cli.chatFallback(context.Background(), cli.cfg.JudgeModel, "hi", 16)
	if err == nil || !strings.Contains(err.Error(), "status 501") {
		t.Fatalf("want the 501 surfaced, got %v", err)
	}
	if errors.Is(err, pipeline.ErrModelUnavailable) {
		t.Errorf("a 501 must not park the model as unavailable: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (501 is not a per-model blip)", calls)
	}
}

func TestIsTransientUpstreamStatus(t *testing.T) {
	for status, want := range map[int]bool{
		200: false,
		400: false,
		403: false,
		404: false,
		429: false, // handled by the quota path, not this predicate
		500: true,
		501: false,
		502: true,
		503: true,
		504: true,
	} {
		if got := isTransientUpstreamStatus(status); got != want {
			t.Errorf("isTransientUpstreamStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestNewFlexInferClient_ResolvesFallbackChains(t *testing.T) {
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Registry defaults: judge + research primaries carry the -5930k twin
	// then fast-text as declared fallbacks.
	if len(cli.cfg.JudgeModelFallbacks) == 0 {
		t.Error("registry-resolved JudgeModel should carry the role's fallback chain")
	}
	if len(cli.cfg.WeaverModelFallbacks) == 0 {
		t.Error("registry-resolved WeaverModel should carry the role's fallback chain")
	}
	// A pinned model now gains a degrade chain from the aimodels role chain so
	// a 503-parked / drifted pinned primary can walk to an alternate instead of
	// re-dialing the same unservable GPU (S2 model-unavailable-degrade). The
	// pinned primary is never itself listed as its own fallback.
	pinned, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "pinned-model", WeaverModel: "pinned-weaver"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if len(pinned.cfg.JudgeModelFallbacks) == 0 || len(pinned.cfg.WeaverModelFallbacks) == 0 {
		t.Errorf("pinned models should gain a registry degrade chain: %v / %v",
			pinned.cfg.JudgeModelFallbacks, pinned.cfg.WeaverModelFallbacks)
	}
	for _, m := range pinned.cfg.JudgeModelFallbacks {
		if m == "pinned-model" {
			t.Errorf("pinned primary must not appear in its own fallback chain: %v", pinned.cfg.JudgeModelFallbacks)
		}
	}
}

func TestNewFlexInferClient_PinnedModelEnvFallbacks(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MODEL_FALLBACKS", "judge-fb-a, judge-fb-b ,judge-fb-a")
	t.Setenv("FLEXINFER_WEAVER_MODEL_FALLBACKS", "weaver-fb-a")
	cli, err := NewFlexInferClient(FlexInferConfig{
		ProxyURL:    "http://stub",
		JudgeModel:  "pinned-judge",
		WeaverModel: "pinned-weaver",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Env override wins over the registry chain; blanks + dupes are dropped.
	if got, want := fmt.Sprint(cli.cfg.JudgeModelFallbacks), fmt.Sprint([]string{"judge-fb-a", "judge-fb-b"}); got != want {
		t.Errorf("judge fallbacks = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(cli.cfg.WeaverModelFallbacks), fmt.Sprint([]string{"weaver-fb-a"}); got != want {
		t.Errorf("weaver fallbacks = %s, want %s", got, want)
	}
}

// TestEstimateCostUSD_PrefersProviderReportedCost pins the wave-3 accounting
// contract: LiteLLM/OpenRouter report the real upstream charge in usage.cost;
// when present it beats the flat local-tier estimate (which only fits the
// FlexInfer proxy's GPU models, whose responses never carry cost).
func TestEstimateCostUSD_PrefersProviderReportedCost(t *testing.T) {
	var withCost chatResponse
	withCost.Usage.PromptTokens = 1000
	withCost.Usage.CompletionTokens = 1000
	withCost.Usage.Cost = 0.0172
	if got := estimateCostUSD(&withCost); got != 0.0172 {
		t.Errorf("provider cost should win: got %v want 0.0172", got)
	}
	var withoutCost chatResponse
	withoutCost.Usage.PromptTokens = 1000
	withoutCost.Usage.CompletionTokens = 1000
	if got := estimateCostUSD(&withoutCost); got != 0.0004 {
		t.Errorf("local-tier estimate: got %v want 0.0004", got)
	}
}
