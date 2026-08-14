package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/council"
)

// anthropicCouncilDefaultMaxTokens bounds one Anthropic editor completion. The
// council editor emits three markdown artifacts plus the structured proposals
// block; 32k output tokens is generous headroom (the call streams, so a large
// ceiling is safe).
const anthropicCouncilDefaultMaxTokens int64 = 32000

type anthropicTokenPrice struct {
	InputPerMillion      float64
	CacheWritePerMillion float64
	CacheReadPerMillion  float64
	OutputPerMillion     float64
}

// Standard first-party Claude API prices. Sonnet 5 deliberately uses its
// $3/$15 sticker price rather than the $2/$10 introductory price through
// 2026-08-31, so accounting remains conservative after that promotion expires.
var anthropicCouncilTokenPrices = map[string]anthropicTokenPrice{
	"claude-fable-5":    {InputPerMillion: 10, CacheWritePerMillion: 12.50, CacheReadPerMillion: 1, OutputPerMillion: 50},
	"claude-mythos-5":   {InputPerMillion: 10, CacheWritePerMillion: 12.50, CacheReadPerMillion: 1, OutputPerMillion: 50},
	"claude-opus-4-8":   {InputPerMillion: 5, CacheWritePerMillion: 6.25, CacheReadPerMillion: 0.50, OutputPerMillion: 25},
	"claude-opus-4-7":   {InputPerMillion: 5, CacheWritePerMillion: 6.25, CacheReadPerMillion: 0.50, OutputPerMillion: 25},
	"claude-opus-4-6":   {InputPerMillion: 5, CacheWritePerMillion: 6.25, CacheReadPerMillion: 0.50, OutputPerMillion: 25},
	"claude-opus-5":     {InputPerMillion: 5, CacheWritePerMillion: 6.25, CacheReadPerMillion: 0.50, OutputPerMillion: 25},
	"claude-sonnet-5":   {InputPerMillion: 3, CacheWritePerMillion: 3.75, CacheReadPerMillion: 0.30, OutputPerMillion: 15},
	"claude-sonnet-4-6": {InputPerMillion: 3, CacheWritePerMillion: 3.75, CacheReadPerMillion: 0.30, OutputPerMillion: 15},
	"claude-haiku-4-5":  {InputPerMillion: 1, CacheWritePerMillion: 1.25, CacheReadPerMillion: 0.10, OutputPerMillion: 5},
}

// AnthropicCouncilEditor drives a frontier Claude model (e.g. claude-opus-4-8
// or claude-fable-5) via the Messages API as the council editor / Spinning Room
// frame — a native-Anthropic peer to OpenAIResponsesCouncilEditor. It emits the
// SAME three markdown artifacts + the optional structured "## Backlog Proposals"
// block the flexinfer and OpenAI editors do (reusing the shared prompt +
// parsers), so the writer / mutator / eval flow is unchanged; only the
// synthesis model differs. A single stateless streaming turn per Edit.
type AnthropicCouncilEditor struct {
	Client  anthropicMessenger
	Model   string // e.g. "claude-opus-4-8"
	Backend string // attribution only; default "anthropic"
	// Patterns, when set, injects the approved-pattern catalog into the editor
	// prompt (Pattern Loom A1). Nil = no catalog. Best-effort: a fetch failure
	// is logged and the catalog omitted — never blocks decomposition. Shares
	// the flexinfer editor's fetch + prompt.
	Patterns council.PatternLister
	// RepoRoot grounds the decomposition in the real repo layout (shares the
	// flexinfer/OpenAI editor's RepoTreeDigest prompt grounding). Empty ⇒ no
	// layout section.
	RepoRoot string
	// Memory, when set AND LOOM_MILLS_COUNCIL_MEMORY is on, supplies the
	// council lane's durable cross-run journal. It renders into the
	// cache_control'd system block ahead of the repo layout, so a growing
	// memory extends the warm prefix instead of sitting behind a churning one.
	// Nil (or the flag off) ⇒ the prompt is byte-identical to the pre-feature
	// one.
	Memory council.MemoryLoader
	// MaxTokens overrides the per-Edit output ceiling. 0 ⇒ the package default.
	MaxTokens int64
}

// Edit implements council.Editor.
func (e *AnthropicCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	if e == nil || e.Client == nil {
		return nil, errors.New("anthropic council editor: client not configured")
	}
	if brief == nil {
		return nil, errors.New("anthropic council editor: brief required")
	}
	// Attribute this turn's token accounting to the council editor — the
	// client-side Observe reads the label off ctx. Same tagging as the
	// flexinfer and OpenAI editors; instrumentation only.
	ctx = llmusage.WithComponent(ctx, ComponentCouncilEditor)
	patterns := fetchApprovedPatterns(ctx, e.Patterns)
	repoTree := RepoPackageLayout(e.RepoRoot, councilLayoutMaxEntries)
	// Split the prompt so the stable prefix (instructions + repo layout +
	// pattern catalog) rides a cache_control'd system block; only the brief +
	// reviews vary per run and stay in the user message.
	memory := council.MemoryBlock(ctx, e.Memory)
	systemPrefix, userPrompt := buildCouncilEditorPromptParts(brief, reviews, patterns, repoTree, memory)
	started := time.Now().UTC()

	maxTokens := e.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicCouncilDefaultMaxTokens
	}
	res, err := e.Client.CreateMessage(ctx, anthropicMessageRequest{
		Model:     e.Model,
		System:    systemPrefix,
		Prompt:    userPrompt,
		MaxTokens: maxTokens,
	})
	if err != nil {
		cost, priced := anthropicCouncilResponseCostUSD(e.Model, res)
		hasUsage := res.InputTokens > 0 || res.OutputTokens > 0 ||
			res.CacheCreationInputTokens > 0 || res.CacheReadInputTokens > 0
		return &council.EditorOutput{
			Backend: e.backend(), Model: e.Model, CostUSD: cost,
			CostUnpriced: !priced || !hasUsage,
			Sidecar:      council.Sidecar{StartedAt: started, CostUSD: council.SidecarCost{Frontier: cost}},
		}, fmt.Errorf("anthropic council editor: %w", err)
	}

	raw := strings.TrimSpace(res.Text)
	// A refused request yields empty/partial content we don't trust; either way
	// the run is unusable, so the FallbackCouncilEditor should fall back.
	empty := raw == "" || res.Refusal
	sections := splitCouncilSections(raw)
	// Lift the optional structured decomposition + trim it out of the
	// implementation doc — same handling as the flexinfer / OpenAI editors.
	proposals, propStatus, propDetail := parseCouncilProposalsDiag(raw)
	if i := strings.Index(sections.implementation, "## Backlog Proposals"); i >= 0 {
		sections.implementation = strings.TrimSpace(sections.implementation[:i])
	}
	models := councilModels(e.Model, reviews)

	var notes string
	switch {
	case res.Refusal:
		notes = fmt.Sprintf("editor model %q refused the request (safety classifier); run marked error", e.Model)
	case raw == "":
		notes = fmt.Sprintf("editor model %q returned no content; run marked error", e.Model)
	default:
		notes = fmt.Sprintf("generated by Anthropic council editor (in=%d out=%d cache_read=%d cache_write=%d tokens)",
			res.InputTokens, res.OutputTokens, res.CacheReadInputTokens, res.CacheCreationInputTokens)
		if n := councilProposalsNote(propStatus, propDetail); n != "" {
			notes += "; " + n
		}
	}

	cost, costPriced := anthropicCouncilResponseCostUSD(e.Model, res)
	hasUsage := res.InputTokens > 0 || res.OutputTokens > 0 ||
		res.CacheCreationInputTokens > 0 || res.CacheReadInputTokens > 0
	out := &council.EditorOutput{
		Backend:      e.backend(),
		Model:        e.Model,
		CostUSD:      cost,
		CostUnpriced: !costPriced || !hasUsage,
		Empty:        empty,
		Objective:    parseCouncilObjective(raw),
		Documents: []council.ArtifactDoc{
			{Kind: council.KindResearch, Title: "Mills council research", Body: sections.research},
			{Kind: council.KindProductSpec, Title: "Mills council product spec", Body: sections.productSpec},
			{Kind: council.KindImplementation, Title: "Mills council implementation plan", Body: sections.implementation},
		},
		BacklogProposals: proposals,
		Sidecar: council.Sidecar{
			Models:        models,
			StartedAt:     started,
			CostUSD:       council.SidecarCost{Frontier: cost},
			BacklogDeltas: council.SidecarBacklog{Created: len(proposals)},
			Notes:         notes,
		},
	}
	if guard := council.ApplyEditorGuardrails(out); guard.Applied() {
		if note := guard.Note(); note != "" {
			if strings.TrimSpace(out.Sidecar.Notes) != "" {
				out.Sidecar.Notes += "; " + note
			} else {
				out.Sidecar.Notes = note
			}
		}
	}
	return out, nil
}

func anthropicCouncilResponseCostUSD(model string, res anthropicMessageResult) (float64, bool) {
	price, ok := anthropicCouncilTokenPrices[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	input := max(res.InputTokens, int64(0))
	output := max(res.OutputTokens, int64(0))
	cacheWrite := max(res.CacheCreationInputTokens, int64(0))
	cacheRead := max(res.CacheReadInputTokens, int64(0))
	return (float64(input)*price.InputPerMillion +
		float64(cacheWrite)*price.CacheWritePerMillion +
		float64(cacheRead)*price.CacheReadPerMillion +
		float64(output)*price.OutputPerMillion) / 1_000_000, true
}

func (e *AnthropicCouncilEditor) backend() string {
	if strings.TrimSpace(e.Backend) != "" {
		return e.Backend
	}
	return "anthropic"
}

var _ council.Editor = (*AnthropicCouncilEditor)(nil)
