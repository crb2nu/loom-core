package clients

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

// responsesClient is the minimal openairesponses surface the editor needs,
// satisfied by *openairesponses.APIClient. Declared locally so tests can
// inject a fake without standing up the HTTP client.
type responsesClient interface {
	Create(ctx context.Context, req openairesponses.TurnRequest) (openairesponses.TurnResponse, error)
}

// openAICouncilPromptCacheKey groups every council/Spinning-Room editor request
// under one OpenAI prompt-cache routing key. All editor calls share the same
// stable instruction/repo-layout/pattern prefix, so a single key maximizes hit
// rate (OpenAI combines it with the prefix hash). Kept well under the ~15
// req/min-per-key overflow threshold by the editor's cadence.
const openAICouncilPromptCacheKey = "loom-mills-council-editor"

type openAITokenPrice struct {
	InputPerMillion       float64
	CachedInputPerMillion float64
	OutputPerMillion      float64
}

// Prices are pinned to the official model catalog. Unknown models remain
// unpriced (zero) rather than inheriting an incorrect alias rate.
var openAICouncilTokenPrices = map[string]openAITokenPrice{
	"gpt-5.4": {
		InputPerMillion: 2.50, CachedInputPerMillion: 0.25, OutputPerMillion: 15.00,
	},
	"gpt-5.4-2026-03-05": {
		InputPerMillion: 2.50, CachedInputPerMillion: 0.25, OutputPerMillion: 15.00,
	},
	// The gpt-5.6 family launched 2026-07-09. Do not let gateway aliases
	// inherit another model's rate: lookup below strips only the oa/ prefix.
	"gpt-5.6-sol": {
		InputPerMillion: 5.00, CachedInputPerMillion: 0.50, OutputPerMillion: 30.00,
	},
	"gpt-5.6-terra": {
		InputPerMillion: 2.50, CachedInputPerMillion: 0.25, OutputPerMillion: 15.00,
	},
	"gpt-5.6-luna": {
		InputPerMillion: 1.00, CachedInputPerMillion: 0.10, OutputPerMillion: 6.00,
	},
}

// OpenAIResponsesCouncilEditor drives a frontier OpenAI model (e.g. gpt-5.4)
// via the Responses API as the council editor — for higher-quality roadmap
// decomposition than the local flexinfer editor (.loom/163 S3, "stronger
// editor model"). It emits the SAME three markdown artifacts + the optional
// structured "## Backlog Proposals" block the flexinfer editor does (reusing
// the shared prompt + parsers), so the writer / mutator / eval flow is
// unchanged; only the synthesis model differs. A single stateless Responses
// turn per Edit — no tools, no conversation state.
type OpenAIResponsesCouncilEditor struct {
	Client  responsesClient
	Model   string // e.g. "gpt-5.4"
	Backend string // attribution only; default "openai-responses"
	// Patterns, when set, supplies the approved-pattern catalog injected
	// into the editor prompt (Pattern Loom A1). Nil = no catalog. Best-
	// effort: a fetch failure is logged and the catalog omitted — never
	// blocks decomposition. Shares the flexinfer editor's fetch + prompt.
	Patterns council.PatternLister
	// RepoRoot grounds the decomposition in the real repo layout (shares the
	// flexinfer editor's RepoTreeDigest prompt grounding). Empty ⇒ no layout
	// section. This is the frontier (gpt-5.4) editor that runs in prod, so it
	// is the primary beneficiary of the grounding.
	RepoRoot string
	// Memory, when set AND LOOM_MILLS_COUNCIL_MEMORY is on, supplies the
	// council lane's durable cross-run journal, rendered into the stable
	// prefix ahead of the repo layout. Nil (or the flag off) ⇒ the prompt is
	// byte-identical to the pre-feature one. Shares the flexinfer editor's
	// loader + render.
	Memory council.MemoryLoader
}

// Edit implements council.Editor.
func (e *OpenAIResponsesCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	if e == nil || e.Client == nil {
		return nil, errors.New("openai council editor: client not configured")
	}
	if brief == nil {
		return nil, errors.New("openai council editor: brief required")
	}
	patterns := fetchApprovedPatterns(ctx, e.Patterns)
	repoTree := RepoPackageLayout(e.RepoRoot, councilLayoutMaxEntries)
	// buildCouncilEditorPrompt is stable-first (instructions + repo layout +
	// pattern catalog, THEN the brief) — the ordering OpenAI prompt caching
	// needs: static content leads, variable content trails. Caching is
	// automatic for prompts >=1024 tokens; the prompt_cache_key below improves
	// routing so repeated editor runs sharing that prefix hit the same cache.
	memory := council.MemoryBlock(ctx, e.Memory)
	prompt := buildCouncilEditorPrompt(brief, reviews, patterns, repoTree, memory)
	started := time.Now().UTC()
	resp, err := e.Client.Create(ctx, openairesponses.TurnRequest{
		Model:          e.Model,
		Input:          prompt,
		Context:        openairesponses.ContextStrategy{Mode: openairesponses.ContextModeStateless},
		PromptCacheKey: openAICouncilPromptCacheKey,
	})
	if err != nil {
		cost, priced := openAICouncilResponseCostUSD(e.Model, resp)
		hasUsage := resp.PromptTokens > 0 || resp.CachedTokens > 0 || resp.CompletionTokens > 0
		return &council.EditorOutput{
			Backend: e.backend(), Model: e.Model, CostUSD: cost,
			CostUnpriced: !priced || !hasUsage,
			Sidecar:      council.Sidecar{StartedAt: started, CostUSD: council.SidecarCost{Frontier: cost}},
		}, fmt.Errorf("openai council editor: %w", err)
	}

	raw := strings.TrimSpace(resp.OutputText)
	empty := raw == ""
	sections := splitCouncilSections(raw)
	// S3 (.loom/163): lift the optional structured decomposition + trim it out
	// of the implementation doc — same handling as the flexinfer editor.
	proposals, propStatus, propDetail := parseCouncilProposalsDiag(raw)
	if i := strings.Index(sections.implementation, "## Backlog Proposals"); i >= 0 {
		sections.implementation = strings.TrimSpace(sections.implementation[:i])
	}
	models := councilModels(e.Model, reviews)
	notes := fmt.Sprintf("generated by OpenAI Responses council editor (in=%d out=%d cached=%d tokens)",
		resp.PromptTokens, resp.CompletionTokens, resp.CachedTokens)
	if empty {
		notes = fmt.Sprintf("editor model %q returned no content; run marked error", e.Model)
	} else if n := councilProposalsNote(propStatus, propDetail); n != "" {
		notes += "; " + n
	}
	cost, costPriced := openAICouncilResponseCostUSD(e.Model, resp)
	hasUsage := resp.PromptTokens > 0 || resp.CachedTokens > 0 || resp.CompletionTokens > 0
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

func openAICouncilResponseCostUSD(model string, resp openairesponses.TurnResponse) (float64, bool) {
	return openAICouncilTokenCostUSD(model, resp.PromptTokens, resp.CachedTokens, resp.CompletionTokens)
}

// openAICouncilChatResponseCostUSD prices the OpenAI-compatible usage returned
// by LiteLLM for council eval judges. LiteLLM does not always include usage.cost,
// but its token counts are sufficient for the explicitly priced oa/* models.
func openAICouncilChatResponseCostUSD(model string, resp *chatResponse) (float64, bool) {
	if resp == nil {
		return 0, false
	}
	cached := llmusage.CachedTokens(resp.Usage.PromptTokensDetails, resp.Usage.InputTokensDetails)
	if cached < 0 {
		cached = 0
	}
	return openAICouncilTokenCostUSD(model, resp.Usage.PromptTokens, cached, resp.Usage.CompletionTokens)
}

// openAICouncilTokenCostUSD prices a known Council OpenAI model from its
// OpenAI-compatible usage block. LiteLLM routes OpenAI models as oa/<model>;
// remove only that gateway prefix so aliases cannot accidentally inherit a
// different model's rate.
func openAICouncilTokenCostUSD(model string, promptTokens, cachedTokens, completionTokens int) (float64, bool) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "oa/")
	price, ok := openAICouncilTokenPrices[model]
	if !ok {
		return 0, false
	}
	prompt := max(promptTokens, 0)
	cached := max(cachedTokens, 0)
	if cached > prompt {
		cached = prompt
	}
	completion := max(completionTokens, 0)
	uncached := prompt - cached
	inputMultiplier, outputMultiplier := 1.0, 1.0
	if prompt > 272_000 {
		inputMultiplier = 2
		outputMultiplier = 1.5
	}
	return inputMultiplier*(float64(uncached)*price.InputPerMillion+float64(cached)*price.CachedInputPerMillion)/1_000_000 +
		outputMultiplier*float64(completion)*price.OutputPerMillion/1_000_000, true
}

func (e *OpenAIResponsesCouncilEditor) backend() string {
	if strings.TrimSpace(e.Backend) != "" {
		return e.Backend
	}
	return "openai-responses"
}

var _ council.Editor = (*OpenAIResponsesCouncilEditor)(nil)

// FallbackCouncilEditor tries Primary and, on any error OR empty output, falls
// back to Fallback for that Edit. It exists so a transient failure of an
// external editor (OpenAI timeout/429/5xx) never hard-fails a scheduled council
// run — the local flexinfer editor still produces artifacts. The fallback is
// per-Edit and stateless; there is no retry of Primary.
type FallbackCouncilEditor struct {
	Primary  council.Editor
	Fallback council.Editor
	Logger   *slog.Logger
}

// Edit implements council.Editor.
func (e *FallbackCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	primaryOut, primaryErr := e.Primary.Edit(ctx, brief, reviews)
	if primaryErr == nil && primaryOut != nil && !primaryOut.Empty {
		return primaryOut, nil
	}
	if e.Fallback == nil {
		return primaryOut, primaryErr
	}
	if e.Logger != nil {
		reason := "empty output"
		if primaryErr != nil {
			reason = primaryErr.Error()
		}
		e.Logger.Warn("primary council editor failed; falling back to secondary editor", "reason", reason)
	}
	fallbackOut, fallbackErr := e.Fallback.Edit(ctx, brief, reviews)
	merged := mergeCouncilEditorAttempts(fallbackOut, primaryOut)
	if merged == nil && (primaryErr != nil || fallbackErr != nil) {
		merged = &council.EditorOutput{}
	}
	if merged != nil && ((primaryErr != nil && primaryOut == nil) || (fallbackErr != nil && fallbackOut == nil)) {
		merged.CostUnpriced = true
	}
	return merged, fallbackErr
}

func mergeCouncilEditorAttempts(result, prior *council.EditorOutput) *council.EditorOutput {
	if result == nil {
		return prior
	}
	merged := *result
	if prior == nil {
		return &merged
	}
	merged.CostUSD += prior.CostUSD
	merged.CostUnpriced = merged.CostUnpriced || prior.CostUnpriced
	merged.Sidecar.CostUSD.Frontier += prior.Sidecar.CostUSD.Frontier
	merged.Sidecar.CostUSD.Local += prior.Sidecar.CostUSD.Local
	return &merged
}

var _ council.Editor = (*FallbackCouncilEditor)(nil)
