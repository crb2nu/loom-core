package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
)

// scoreSignalRe detects whether a raw judge response carries a genuine
// score signal: a JSON `"score":` key followed by an in-range numeric
// literal. It intentionally mirrors what the hardened parser will accept
// so the kill test's per-sample expectation is derived from the bytes, not
// hard-coded to fixture ordering. A schema echo like `{"score": <number>}`
// (literal placeholder) or prose like `return a score of 1.0` does NOT
// match — there is no quoted key + numeric value.
var scoreSignalRe = regexp.MustCompile(`"score"\s*:\s*(-?\d+(?:\.\d+)?)`)

func rawHasInRangeScore(raw string) bool {
	for _, m := range scoreSignalRe.FindAllStringSubmatch(raw, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v <= 1 {
			return true
		}
	}
	return false
}

// TestRubricEnvelopeLiveFixtures is the S1 kill test. It replays the 7 REAL
// production judge responses captured 2026-07-16 from gate_outcomes.reasons
// (log-truncated prefixes of the raw model output). Each fixture reason
// embeds the raw response as a Go-quoted string after "raw="; we unquote it
// and feed it to the hardened parser. The parser MUST recover a score for
// every sample that carries a score signal, and return the unparseable
// sentinel (never a panic or a mis-score) for samples with no score at all.
func TestRubricEnvelopeLiveFixtures(t *testing.T) {
	path := filepath.Join("testdata", "judge_unparseable_live_2026-07-16.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixtures []struct {
		Run    string `json:"run"`
		Gate   string `json:"gate"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixtures) != 7 {
		t.Fatalf("expected 7 live fixtures, got %d", len(fixtures))
	}

	var recovered, sentinels int
	for i, f := range fixtures {
		raw := extractRawPayload(t, f.Reason)
		if raw == "" {
			t.Fatalf("sample %d (%s/%s): no raw= payload extracted", i, f.Run, f.Gate)
		}
		score, reasons, perr := parseRubricEnvelope(raw)
		if rawHasInRangeScore(raw) {
			if perr != nil {
				t.Errorf("sample %d (%s/%s): score signal present but parser failed: %v", i, f.Run, f.Gate, perr)
				continue
			}
			if score < 0 || score > 1 {
				t.Errorf("sample %d (%s/%s): recovered score %v out of [0,1]", i, f.Run, f.Gate, score)
			}
			recovered++
			continue
		}
		// No score signal: must return the unparseable sentinel, not a
		// fabricated score.
		if perr == nil {
			t.Errorf("sample %d (%s/%s): expected unparseable sentinel, recovered score %v reasons %v", i, f.Run, f.Gate, score, reasons)
			continue
		}
		if !errors.Is(perr, ErrRubricUnparseable) {
			t.Errorf("sample %d (%s/%s): error must wrap ErrRubricUnparseable: %v", i, f.Run, f.Gate, perr)
		}
		sentinels++
	}
	// The fixture set must exercise both arms: at least one real recovery
	// (the truncated {"score":0.8,...} verdict) and at least one genuine
	// sentinel (the truncated "Thinking Process:" preambles).
	if recovered == 0 {
		t.Errorf("no fixture recovered a score; the truncation-tolerant parser is not exercised")
	}
	if sentinels == 0 {
		t.Errorf("no fixture hit the unparseable sentinel; the negative path is not exercised")
	}
}

// extractRawPayload pulls the Go-quoted raw model output out of a captured
// gate reason string. The reason ends with `...; raw="<go-quoted>"`.
func extractRawPayload(t *testing.T, reason string) string {
	t.Helper()
	idx := strings.LastIndex(reason, "raw=")
	if idx < 0 {
		return ""
	}
	quoted := strings.TrimSpace(reason[idx+len("raw="):])
	raw, err := strconv.Unquote(quoted)
	if err != nil {
		t.Fatalf("unquote raw payload: %v\nquoted=%q", err, quoted)
	}
	return raw
}

// The specific captured verdict that carries a score signal must recover
// the exact score the model emitted (0.8) with its (truncated) reasons.
func TestRubricEnvelopeLiveFixtures_RecoversKnownVerdict(t *testing.T) {
	path := filepath.Join("testdata", "judge_unparseable_live_2026-07-16.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixtures []struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var found bool
	for _, f := range fixtures {
		raw := extractRawPayload(t, f.Reason)
		if !strings.Contains(raw, `"score": 0.8`) {
			continue
		}
		found = true
		score, reasons, perr := parseRubricEnvelope(raw)
		if perr != nil {
			t.Fatalf("expected recovery of truncated verdict, got: %v", perr)
		}
		if score != 0.8 {
			t.Errorf("score = %v, want 0.8", score)
		}
		if len(reasons) == 0 {
			t.Errorf("expected at least one recovered reason, got none")
		}
	}
	if !found {
		t.Fatal("fixture with score signal not found")
	}
}

func TestRecoverTruncatedEnvelope_MidStringTruncation(t *testing.T) {
	// Envelope truncated inside the final reason string (array + object
	// left open).
	raw := `{"score": 0.72, "reasons": ["complete reason", "second reason", "third reason cut off here`
	score, reasons, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.72 {
		t.Errorf("score = %v, want 0.72", score)
	}
	if len(reasons) != 3 {
		t.Errorf("reasons len = %d, want 3", len(reasons))
	}
	if reasons[0] != "complete reason" {
		t.Errorf("reasons[0] = %q", reasons[0])
	}
}

func TestRecoverTruncatedEnvelope_TrailingComma(t *testing.T) {
	raw := `{"score": 0.5, "reasons": ["a", "b", `
	score, reasons, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.5 {
		t.Errorf("score = %v, want 0.5", score)
	}
	if len(reasons) != 2 {
		t.Errorf("reasons len = %d, want 2", len(reasons))
	}
}

func TestRecoverTruncatedEnvelope_DanglingReasonsKey(t *testing.T) {
	raw := `{"score": 0.9, "reasons":`
	score, _, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.9 {
		t.Errorf("score = %v, want 0.9", score)
	}
}

func TestRecoverTruncatedEnvelope_ScoreOnly(t *testing.T) {
	raw := `{"score": 0.33`
	score, _, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.33 {
		t.Errorf("score = %v, want 0.33", score)
	}
}

func TestRecoverTruncatedEnvelope_PrefersLastVerdictAnchor(t *testing.T) {
	// A schema echo whose value is a literal placeholder (invalid JSON)
	// precedes the real, truncated verdict. Recovery must land on the
	// verdict, not the echo.
	raw := "Format: {\"score\": <number>}\nVerdict: {\"score\": 0.61, \"reasons\": [\"real reason"
	score, _, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.61 {
		t.Errorf("score = %v, want 0.61", score)
	}
}

func TestRecoverTruncatedEnvelope_RegexFallbackNoBraces(t *testing.T) {
	// No JSON structure survives, but a score literal does. Strategy B
	// recovers it and flags the reasons as best-effort.
	raw := `Based on my review "score": 0.42 and then the response was cut off before`
	score, reasons, err := parseRubricEnvelope(raw)
	if err != nil {
		t.Fatalf("parseRubricEnvelope: %v", err)
	}
	if score != 0.42 {
		t.Errorf("score = %v, want 0.42", score)
	}
	if len(reasons) == 0 || reasons[len(reasons)-1] != truncationRecoveryNote {
		t.Errorf("expected trailing recovery note, got %v", reasons)
	}
}

func TestRecoverTruncatedEnvelope_OutOfRangeRejected(t *testing.T) {
	// A truncated envelope whose (recovered) score is out of [0,1] must NOT
	// be accepted; it stays unparseable rather than mis-scoring.
	raw := `{"score": 1.4, "reasons": ["something`
	_, _, err := parseRubricEnvelope(raw)
	if err == nil {
		t.Fatal("expected unparseable error for out-of-range recovered score")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("error must wrap ErrRubricUnparseable: %v", err)
	}
}

func TestRecoverTruncatedEnvelope_ThinkingPreambleNoScore(t *testing.T) {
	// The dominant negative case: a chain-of-thought preamble truncated
	// before any envelope. No score signal ⇒ unparseable sentinel.
	raw := "Thinking Process:\n1. Analyze the request. Output format: JSON object {\"score\": <number>}. The diff is empty so I should return a score of 1.0 but I ran out of"
	_, _, err := parseRubricEnvelope(raw)
	if err == nil {
		t.Fatal("expected unparseable error for scoreless preamble")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("error must wrap ErrRubricUnparseable: %v", err)
	}
}

// --- FLEXINFER_JUDGE_MAX_TOKENS env override ---

func TestNewRubricJudge_DefaultMaxTokens(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "")
	cli := newStubClient(t, successBody, 200)
	if got := NewRubricJudge(cli).MaxTokens; got != defaultJudgeMaxTokens {
		t.Errorf("default MaxTokens = %d, want %d", got, defaultJudgeMaxTokens)
	}
	if defaultJudgeMaxTokens != 1024 {
		t.Errorf("defaultJudgeMaxTokens = %d, want 1024", defaultJudgeMaxTokens)
	}
}

func TestNewRubricJudge_EnvOverrideMaxTokens(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "2048")
	cli := newStubClient(t, successBody, 200)
	if got := NewRubricJudge(cli).MaxTokens; got != 2048 {
		t.Errorf("MaxTokens = %d, want 2048", got)
	}
}

func TestNewRubricJudge_EnvOverrideIgnoresInvalid(t *testing.T) {
	for _, v := range []string{"nan", "0", "-5"} {
		t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", v)
		cli := newStubClient(t, successBody, 200)
		if got := NewRubricJudge(cli).MaxTokens; got != defaultJudgeMaxTokens {
			t.Errorf("MaxTokens for %q = %d, want default %d", v, got, defaultJudgeMaxTokens)
		}
	}
}

func TestJudge_RequestsConfiguredMaxTokens(t *testing.T) {
	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "1500")
	var captured struct {
		MaxTokens int `json:"max_tokens"`
	}
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "qwen35"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(successBody)), Header: make(http.Header)}, nil
	}))
	if _, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if captured.MaxTokens != 1500 {
		t.Errorf("max_tokens = %d, want 1500", captured.MaxTokens)
	}
}

// --- length-stop retry ---

// lengthStopStub serves a sequence of canned response bodies, one per call,
// and counts calls. The last body is reused if calls exceed the sequence.
func lengthStopStub(t *testing.T, calls *int32, bodies ...string) *FlexInferClient {
	t.Helper()
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "qwen35"})
	if err != nil {
		t.Fatal(err)
	}
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		n := int(atomic.AddInt32(calls, 1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(bodies[n])),
			Header:     make(http.Header),
		}, nil
	}))
	return cli
}

func lengthTruncatedBody(content string) string {
	resp := chatResponse{Model: "qwen35"}
	resp.Choices = append(resp.Choices, chatChoice{
		Message:      chatMessage{Role: "assistant", Content: content},
		FinishReason: "length",
	})
	b, _ := json.Marshal(resp)
	return string(b)
}

func cleanEnvelopeBody(content string) string {
	resp := chatResponse{Model: "qwen35"}
	resp.Choices = append(resp.Choices, chatChoice{
		Message:      chatMessage{Role: "assistant", Content: content},
		FinishReason: "stop",
	})
	b, _ := json.Marshal(resp)
	return string(b)
}

func TestJudge_LengthStopRetryRecoversCleanEnvelope(t *testing.T) {
	var calls int32
	// First call: finish_reason=length, pure preamble, no score at all →
	// unrecoverable → triggers the retry.
	first := lengthTruncatedBody("Thinking Process:\n1. Analyze the request and consider the")
	second := cleanEnvelopeBody(`{"score": 0.88, "reasons": ["looks good"]}`)
	cli := lengthStopStub(t, &calls, first, second)

	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.88 {
		t.Errorf("score = %v, want 0.88 from retry", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count = %d, want exactly 2 (one retry)", got)
	}
}

func TestJudge_LengthStopRetryAppendsJSONOnlyInstruction(t *testing.T) {
	var calls int32
	var prompts []string
	cli, err := NewFlexInferClient(FlexInferConfig{ProxyURL: "http://stub", JudgeModel: "qwen35"})
	if err != nil {
		t.Fatal(err)
	}
	first := lengthTruncatedBody("Thinking Process: still reasoning when the budget ran")
	second := cleanEnvelopeBody(`{"score": 0.5, "reasons": []}`)
	cli.SetTransport(roundTripFn(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages  []struct{ Content string } `json:"messages"`
			MaxTokens int                        `json:"max_tokens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Messages) > 0 {
			prompts = append(prompts, body.Messages[0].Content)
		}
		n := int(atomic.AddInt32(&calls, 1))
		if n == 1 {
			if body.MaxTokens != defaultJudgeMaxTokens {
				t.Errorf("first call max_tokens = %d, want %d", body.MaxTokens, defaultJudgeMaxTokens)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(first)), Header: make(http.Header)}, nil
		}
		if body.MaxTokens != defaultJudgeMaxTokens*2 {
			t.Errorf("retry max_tokens = %d, want %d", body.MaxTokens, defaultJudgeMaxTokens*2)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(second)), Header: make(http.Header)}, nil
	}))

	t.Setenv("FLEXINFER_JUDGE_MAX_TOKENS", "")
	if _, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}
	if strings.Contains(prompts[0], jsonOnlyRetryInstruction) {
		t.Errorf("first prompt must NOT carry the JSON-only instruction")
	}
	if !strings.Contains(prompts[1], jsonOnlyRetryInstruction) {
		t.Errorf("retry prompt must carry the JSON-only instruction; got %q", prompts[1])
	}
}

func TestJudge_LengthStopRetryBoundedToOnce(t *testing.T) {
	var calls int32
	// Both calls truncate with no recoverable score: the judge must retry
	// exactly once, then surface the unparseable error — no third call.
	first := lengthTruncatedBody("Thinking Process: reasoning without end and no envelope")
	second := lengthTruncatedBody("Still thinking, still no JSON envelope emitted before the")
	cli := lengthStopStub(t, &calls, first, second)

	_, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err == nil {
		t.Fatal("expected unparseable error after bounded retry")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("error must wrap ErrRubricUnparseable: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count = %d, want exactly 2 (one bounded retry)", got)
	}
}

func TestJudge_NoRetryWhenFinishReasonNotLength(t *testing.T) {
	var calls int32
	// finish_reason=stop but content is unparseable: this is a genuine
	// judge miss, not a truncation — no retry, surface the sentinel.
	body := cleanEnvelopeBody("I cannot grade this without more context.")
	cli := lengthStopStub(t, &calls, body)

	_, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err == nil {
		t.Fatal("expected unparseable error")
	}
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Errorf("error must wrap ErrRubricUnparseable: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("call count = %d, want 1 (no retry without length stop)", got)
	}
}

func TestJudge_NoRetryWhenFirstResponseParses(t *testing.T) {
	var calls int32
	// finish_reason=length but the truncation-tolerant parser already
	// recovered a score → no wasted retry.
	body := lengthTruncatedBody(`{"score": 0.7, "reasons": ["partial reason cut`)
	cli := lengthStopStub(t, &calls, body)

	v, err := NewRubricJudge(cli).Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v.Score != 0.7 {
		t.Errorf("score = %v, want 0.7", v.Score)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("call count = %d, want 1 (recovered without retry)", got)
	}
}

func TestRecoverTruncatedEnvelope_TruncatedExampleEchoRejected(t *testing.T) {
	// A thinking-style judge echoing the prompt's concrete example envelope,
	// truncated INSIDE the echo, must not balance-repair into a fake passing
	// verdict (review finding on the 2026-07-16 hardening).
	content := `Thinking Process: the instructions say if the diff is empty return ` +
		`{"score": 1.0, "reasons": ["fixture-only`
	if score, _, ok := recoverTruncatedEnvelope(content); ok {
		t.Fatalf("truncated example echo recovered as score=%v; want unrecoverable", score)
	}
}

func TestRecoverTruncatedEnvelope_CompleteEchoThenRealVerdictSurvives(t *testing.T) {
	// A COMPLETE example echo followed by the real (truncated) verdict: the
	// echo is stripped and the genuine score still recovers.
	content := `The example is {"score": 1.0, "reasons": ["fixture-only or empty diff; no scorable concerns"]}. ` +
		`My verdict: {"score": 0.55, "reasons": ["missing test for the new`
	score, _, ok := recoverTruncatedEnvelope(content)
	if !ok {
		t.Fatal("real verdict after complete echo did not recover")
	}
	if score != 0.55 {
		t.Errorf("score = %v, want 0.55 (the real verdict, not the echoed 1.0)", score)
	}
}
