package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

// ChatClient is the minimal LLM surface the overseers need. Satisfied by
// *clients.FlexInferClient — the operator's already-resolved judge client —
// so the overseers inherit the exact backend selection (and its "no litellm
// for judge" fallback rules) without new wiring.
type ChatClient interface {
	ChatStructured(ctx context.Context, model, prompt string, maxTokens int) (string, float64, error)
	JudgeModel() string
}

// defaultTriageMaxTokens bounds one verdict response. Verdicts are compact
// JSON envelopes; anything longer is the model wandering.
const defaultTriageMaxTokens = 512

// Triage is the nil-safe LLM judgment adapter. Agents call Available()
// first and fall back to deterministic-only behavior when the judge backend
// is not wired or a call fails — the hybrid-brain fail-safe: an LLM outage
// degrades the overseers to flag-only, it never blocks their deterministic
// work and never causes a judgment-free action.
type Triage struct {
	Client ChatClient
	// Model overrides the model the verdicts dial. Empty keeps the client's
	// JudgeModel() (the historical wiring); the operator sets it from
	// MILLS_TRIAGE_BACKEND / FLEXINFER_TRIAGE_MODEL so the overseers can run
	// on a warm local lane while the gates keep the frontier judge.
	Model     string
	MaxTokens int
	Logger    *slog.Logger
}

// Available reports whether LLM verdicts can be requested.
func (t *Triage) Available() bool { return t != nil && t.Client != nil }

// model is the id every triage call dials: the override when set, else the
// client's judge model.
func (t *Triage) model() string {
	if t.Model != "" {
		return t.Model
	}
	return t.Client.JudgeModel()
}

// chat is the single dial-out for triage traffic: it tags the context with
// clients.ComponentTriage so usage lands on
// mills_llm_prompt_tokens_total{component="mills-triage"} rather than being
// folded into the judge's counters, and dials model().
func (t *Triage) chat(ctx context.Context, prompt string, maxTokens int) (string, float64, error) {
	if !t.Available() {
		return "", 0, errors.New("overseer triage: no client")
	}
	ctx = llmusage.WithComponent(ctx, clients.ComponentTriage)
	return t.Client.ChatStructured(ctx, t.model(), prompt, maxTokens)
}

// Verdict asks the judge model one structured question and decodes the JSON
// object in its reply into out. Returns the call's cost in USD. Any error —
// transport, empty reply, no decodable JSON — means "no verdict": the caller
// must skip the judgment-gated action, never guess.
func (t *Triage) Verdict(ctx context.Context, prompt string, out any) (float64, error) {
	if !t.Available() {
		return 0, errors.New("overseer triage: no client")
	}
	maxTokens := t.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultTriageMaxTokens
	}
	content, cost, err := t.chat(ctx, prompt, maxTokens)
	if err != nil {
		return cost, fmt.Errorf("overseer triage: %w", err)
	}
	for _, candidate := range clients.ExtractJSONCandidates(content) {
		if err := json.Unmarshal([]byte(candidate), out); err == nil {
			return cost, nil
		}
	}
	if t.Logger != nil {
		t.Logger.Warn("overseer triage: no decodable JSON in reply", "reply_len", len(content))
	}
	return cost, errors.New("overseer triage: no decodable JSON verdict in reply")
}
