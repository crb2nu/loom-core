package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/llmusage"
)

// MemoryConsolidateEnv gates the consolidation trigger in recordItemMemory.
// Default OFF: the consolidator is constructed and wired regardless (so a
// misconfigured model surfaces at startup, not at the first oversized journal),
// but no LLM call is ever made until this is set.
const MemoryConsolidateEnv = "LOOM_MILLS_MEMORY_CONSOLIDATE"

// ComponentMemory is the llmusage component label for memory-consolidation
// traffic. It joins the roster in pkg/mills/clients/usage.go (which aliases it,
// since clients imports pipeline and not the reverse) and exists for the same
// reason the others do: consolidation re-sends a span of one item's journal
// exactly once and then never again, so its cache behaviour is the opposite of
// every other mills caller and must not be averaged in with them.
const ComponentMemory = "mills-memory"

// memoryConsolidationMaxTokens is the completion budget for one distillation.
// Generous relative to what the prompt asks for (an identity passage plus a
// handful of ledger lines) because the local research model is not a reasoning
// model but the configured one may be, and a squeezed completion returns an
// empty envelope — which journalengine.Consolidate treats as an error and
// therefore costs the call without reclaiming anything.
const memoryConsolidationMaxTokens = 2048

// MemoryConsolidateEnabled reports whether the record-time consolidation
// trigger is armed.
func MemoryConsolidateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(MemoryConsolidateEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// MemoryChatClient is the narrow LLM seam the consolidator needs.
// *clients.FlexInferClient satisfies it; pipeline cannot import clients (clients
// imports pipeline), and a one-method interface keeps the dependency pointing
// the right way and the tests free of a transport.
//
// ChatStructured rather than Chat: the consolidator wants a compact JSON
// envelope, and ChatStructured disables model-visible chain-of-thought on local
// FlexInfer models through the chat_template_kwargs contract.
type MemoryChatClient interface {
	ChatStructured(ctx context.Context, model, prompt string, maxTokens int) (string, float64, error)
}

// MemoryConsolidator is the Mills implementation of journalengine.Consolidator:
// it turns a span of an item's journal into an identity passage plus append-only
// ledger lines, served through the operator's instrumented OpenAI-compatible
// client.
//
// The prompt is built from the ConsolidationRequest alone — this type holds no
// per-item state, so one instance serves every backlog item.
type MemoryConsolidator struct {
	// Client is the completion transport. Nil makes Consolidate return an
	// error, which journalengine.Consolidate turns into "journal left intact".
	Client MemoryChatClient
	// Model is the model id to dial. Empty lets the client fall back to its own
	// configured weaver/judge model.
	Model string
	// MaxTokens overrides the completion budget. Zero uses
	// memoryConsolidationMaxTokens.
	MaxTokens int
	Logger    *slog.Logger
}

// NewMemoryConsolidator wires a consolidator against an instrumented chat
// client. model may be empty (the client then resolves its own default).
func NewMemoryConsolidator(client MemoryChatClient, model string, logger *slog.Logger) *MemoryConsolidator {
	return &MemoryConsolidator{Client: client, Model: model, Logger: logger}
}

func (m *MemoryConsolidator) logger() *slog.Logger {
	if m != nil && m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Consolidate implements journalengine.Consolidator.
//
// Every failure path returns an error rather than a partial result: an empty or
// half-parsed Consolidation would let journalengine.Consolidate drop the span's
// entries in exchange for nothing. Returning the error keeps the journal whole.
func (m *MemoryConsolidator) Consolidate(
	ctx context.Context,
	req journalengine.ConsolidationRequest,
) (journalengine.Consolidation, error) {
	if m == nil || m.Client == nil {
		return journalengine.Consolidation{}, errors.New("mills memory: consolidator not configured")
	}
	if len(req.Entries) == 0 {
		return journalengine.Consolidation{}, errors.New("mills memory: no entries to consolidate")
	}
	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		maxTokens = memoryConsolidationMaxTokens
	}
	// Attribute this call's token accounting to the memory lane rather than to
	// whatever component happens to own the surrounding context.
	ctx = llmusage.WithComponent(ctx, ComponentMemory)
	raw, _, err := m.Client.ChatStructured(ctx, m.Model, memoryConsolidationPrompt(req), maxTokens)
	if err != nil {
		return journalengine.Consolidation{}, fmt.Errorf("mills memory: chat: %w", err)
	}
	c, err := parseConsolidationEnvelope(raw)
	if err != nil {
		return journalengine.Consolidation{}, err
	}
	m.logger().Info("item memory: consolidated span",
		"owner", req.Owner, "epoch_start", req.EpochStart, "epoch_end", req.EpochEnd,
		"entries", len(req.Entries), "ledger_lines", len(c.Ledger))
	return c, nil
}

// memoryConsolidationPrompt renders the distillation ask.
//
// Two rules carry the whole design of pkg/journalengine into the prompt text:
// the identity passage REPLACES its predecessor (so it must integrate the prior
// one rather than start over), while ledger lines are APPENDED and never
// rewritten (so they must be neutral, self-contained, and stamped with the epoch
// span — a later consolidation may add lines beside them but will never correct
// them). Getting either backwards is how repeated summarization preserves voice
// and destroys events.
func memoryConsolidationPrompt(req journalengine.ConsolidationRequest) string {
	var b strings.Builder
	b.WriteString(`You are compacting the working memory of an automated software pipeline so it fits a context budget.

You will be given the OLDEST span of a work journal. Distil it into two artifacts with different rules:

1. "identity" — a short third-person prose passage (3-6 sentences) describing what this
   work item is, what has been attempted on it, and where it currently stands. This
   REPLACES the prior identity passage, so integrate the prior passage below rather
   than starting over. Omit anything the span does not support; never invent progress.
2. "ledger" — 1 to 8 neutral, third-person event lines. Each line must stand alone
   without the span it came from, must name concrete facts (stage names, verdicts,
   file counts, error text), and must be prefixed with the epoch span, e.g.
   "[Epochs 4-9] the implement stage failed the scope gate twice on out-of-envelope paths".
   These lines are APPENDED to a permanent record and will never be rewritten, so do not
   restate or correct earlier lines and do not editorialise.

Respond with ONLY a JSON object and nothing else:
{"identity": "...", "ledger": ["...", "..."]}
`)
	fmt.Fprintf(&b, "\n=== Owner ===\n%s\n", req.Owner)
	fmt.Fprintf(&b, "\n=== Epoch span ===\n%d to %d\n", req.EpochStart, req.EpochEnd)
	b.WriteString("\n=== Prior identity ===\n")
	if strings.TrimSpace(req.PriorIdentity) == "" {
		b.WriteString("(none — this is the first consolidation)\n")
	} else {
		b.WriteString(req.PriorIdentity)
		b.WriteString("\n")
	}
	b.WriteString("\n=== Journal span to distil ===\n")
	b.WriteString(journalengine.RenderEntries(req.Entries))
	b.WriteString("\n")
	return b.String()
}

// consolidationEnvelope is the wire shape parseConsolidationEnvelope decodes.
type consolidationEnvelope struct {
	Identity string   `json:"identity"`
	Ledger   []string `json:"ledger"`
}

// parseConsolidationEnvelope extracts the JSON object from a completion that may
// wrap it in prose or a fenced code block. An envelope that decodes but carries
// nothing usable is an error, not an empty Consolidation: journalengine treats
// an empty result as a failure precisely so the caller cannot trade history for
// silence, and returning the error here makes the reason legible.
func parseConsolidationEnvelope(raw string) (journalengine.Consolidation, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return journalengine.Consolidation{}, errors.New("mills memory: empty consolidation response")
	}
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return journalengine.Consolidation{}, fmt.Errorf("mills memory: no JSON object in consolidation response: %q", truncateTailBytes(trimmed, 256))
	}
	var env consolidationEnvelope
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &env); err != nil {
		return journalengine.Consolidation{}, fmt.Errorf("mills memory: decode consolidation response: %w (raw=%q)", err, truncateTailBytes(trimmed, 256))
	}
	c := journalengine.Consolidation{Identity: strings.TrimSpace(env.Identity)}
	for _, line := range env.Ledger {
		if line = strings.TrimSpace(line); line != "" {
			c.Ledger = append(c.Ledger, line)
		}
	}
	if c.IsEmpty() {
		return journalengine.Consolidation{}, errors.New("mills memory: consolidation response carried neither identity nor ledger")
	}
	return c, nil
}
