package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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
	Client    ChatClient
	MaxTokens int
	Logger    *slog.Logger
}

// Available reports whether LLM verdicts can be requested.
func (t *Triage) Available() bool { return t != nil && t.Client != nil }

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
	content, cost, err := t.Client.ChatStructured(ctx, t.Client.JudgeModel(), prompt, maxTokens)
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
