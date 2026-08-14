package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
)

// FlexInferCouncilReviewer adapts FlexInfer chat completions to the
// council.Reviewer contract. It is the production-safe local-tier reviewer:
// deterministic temperature, bounded output, and no frontier dependency.
type FlexInferCouncilReviewer struct {
	Client    *FlexInferClient
	MaxTokens int
}

// Review implements council.Reviewer.
func (r *FlexInferCouncilReviewer) Review(ctx context.Context, brief *council.Brief, lens council.ReviewerLens) (council.ReviewerOutput, error) {
	if r == nil || r.Client == nil {
		return council.ReviewerOutput{Lens: lens}, errors.New("flexinfer council reviewer: client not configured")
	}
	if brief == nil {
		return council.ReviewerOutput{Lens: lens}, errors.New("flexinfer council reviewer: brief required")
	}
	ctx = llmusage.WithComponent(ctx, ComponentCouncilReviewer)
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 384
	}
	prompt := fmt.Sprintf(`You are the %q reviewer in the Loom Mills council.

Review the brief through this lens only. Return concise markdown with:
- material risks
- missing acceptance criteria
- concrete next slice recommendation
- any reason autonomy should fail closed

Brief:
%s`, lens.Name, brief.Markdown)
	out, resp, cost, providerReported, err := r.Client.chatCompletionResponseCostStatus(ctx, lens.Model, prompt, maxTokens, true)
	cost, providerReported = knownRemoteCouncilCost(lens.Model, lens.Backend, resp, cost, providerReported)
	if err != nil {
		return council.ReviewerOutput{
			Lens: lens, CostUSD: cost,
			CostUnpriced: !isLocalCouncilBackend(lens.Backend) && !providerReported,
		}, err
	}
	return council.ReviewerOutput{
		Lens:         lens,
		Markdown:     strings.TrimSpace(out),
		CostUSD:      cost,
		CostUnpriced: !isLocalCouncilBackend(lens.Backend) && !providerReported,
	}, nil
}

// FlexInferCouncilEditor adapts FlexInfer chat completions to the
// council.Editor contract. It emits the three canonical council artifacts from
// one model response so the existing writer/mutator/eval flow stays unchanged.
type FlexInferCouncilEditor struct {
	Client    *FlexInferClient
	Backend   string
	Model     string
	MaxTokens int
	// Patterns, when set, supplies the approved-pattern catalog injected
	// into the editor prompt (Pattern Loom A1). Nil = no catalog (the
	// prompt's pattern_id field stays advisory). Best-effort: a fetch
	// failure is logged and the catalog omitted — never blocks decomposition.
	Patterns council.PatternLister
	// RepoRoot grounds the decomposition: when set to the operator's
	// loom-core checkout, the editor prompt is prefixed with the real
	// top-of-tree layout so the model scopes each slice's `files` to paths
	// that actually exist instead of inventing a plausible-but-fictional
	// architecture (the live failure: council slices referencing
	// pkg/planning/ and pkg/pipeline/, which do not exist, escalated every
	// council item). Empty ⇒ no layout section (back-compat); mirrors the
	// research stage's RepoTreeDigest prompt grounding.
	RepoRoot string
	// Memory, when set AND LOOM_MILLS_COUNCIL_MEMORY is on, supplies the
	// council lane's durable cross-run journal, rendered into the stable half
	// of the editor prompt so this tick knows what earlier ticks minted. Nil
	// (or the flag off) ⇒ the prompt is byte-identical to the pre-feature one.
	// Best-effort: a load failure omits the block, never blocks the run.
	Memory council.MemoryLoader
}

// Edit implements council.Editor.
func (e *FlexInferCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	if e == nil || e.Client == nil {
		return nil, errors.New("flexinfer council editor: client not configured")
	}
	if brief == nil {
		return nil, errors.New("flexinfer council editor: brief required")
	}
	ctx = llmusage.WithComponent(ctx, ComponentCouncilEditor)
	maxTokens := e.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	patterns := fetchApprovedPatterns(ctx, e.Patterns)
	repoTree := RepoPackageLayout(e.RepoRoot, councilLayoutMaxEntries)
	memory := council.MemoryBlock(ctx, e.Memory)
	prompt := buildCouncilEditorPrompt(brief, reviews, patterns, repoTree, memory)
	// Capture StartedAt BEFORE the chat call so the artifact writer
	// (artifacts.go) sees a non-zero StartedAt and preserves it instead
	// of stamping the post-write time over it. Without this, both
	// StartedAt and EndedAt got the same post-write timestamp, the
	// council_runs row reported 0ms duration on every successful run,
	// and the Council tab rendered the misleading 'instant' badge for
	// runs that actually took multiple seconds.
	started := time.Now().UTC()
	raw, cost, providerReported, err := e.Client.chatCompletionCostStatus(ctx, e.Model, prompt, maxTokens, false)
	if err != nil {
		backend := e.backend()
		return &council.EditorOutput{
			Backend: backend, Model: e.Model, CostUSD: cost,
			CostUnpriced: !isLocalCouncilBackend(backend) && !providerReported,
			Sidecar:      council.Sidecar{StartedAt: started, CostUSD: councilCostForBackend(backend, cost)},
		}, err
	}
	// Detect the empty-response case so the runner can mark the council
	// run as failed instead of silently writing "No model output returned."
	// as the research body and marking the run success.
	empty := strings.TrimSpace(raw) == ""
	sections := splitCouncilSections(raw)
	// S3 (.loom/163): lift the optional structured decomposition out of the
	// editor's output. Lenient — absent/malformed ⇒ nil, preserving the prior
	// markdown-only behavior. Trim the JSON appendix out of the implementation
	// doc so the rendered artifact stays clean.
	proposals, propStatus, propDetail := parseCouncilProposalsDiag(raw)
	if i := strings.Index(sections.implementation, "## Backlog Proposals"); i >= 0 {
		sections.implementation = strings.TrimSpace(sections.implementation[:i])
	}
	models := councilModels(e.Model, reviews)
	notes := "generated by FlexInfer-backed council participants"
	if empty {
		notes = fmt.Sprintf("editor model %q returned no content; run marked error", e.Model)
	} else if n := councilProposalsNote(propStatus, propDetail); n != "" {
		notes += "; " + n
	}
	backend := e.backend()
	out := &council.EditorOutput{
		Backend:      backend,
		Model:        e.Model,
		CostUSD:      cost,
		CostUnpriced: !isLocalCouncilBackend(backend) && !providerReported,
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
			CostUSD:       councilCostForBackend(backend, cost),
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

func (e *FlexInferCouncilEditor) backend() string {
	if strings.TrimSpace(e.Backend) != "" {
		return e.Backend
	}
	return "flexinfer"
}

func isLocalCouncilBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "flexinfer", "local", "ollama", "vllm", "llamacpp":
		return true
	default:
		return false
	}
}

func councilCostForBackend(backend string, cost float64) council.SidecarCost {
	if isLocalCouncilBackend(backend) {
		return council.SidecarCost{Local: cost}
	}
	return council.SidecarCost{Frontier: cost}
}

// councilLayoutMaxEntries bounds the package-layout digest. The real code
// layout (top-level dirs + two levels under pkg/, internal/, cmd/) is well
// under this on loom-core; the cap is a safety valve, not an expected limit.
const councilLayoutMaxEntries = 250

// codeSourceRoots are the directories the package-layout digest enumerates two
// levels deep — the Go code lives here, so this is where slice files must land.
var codeSourceRoots = []string{"pkg", "internal", "cmd"}

// RepoPackageLayout returns a bounded, deterministic listing of the repo's real
// package directories so the council editor scopes each slice's `files` to
// packages that EXIST. The digest lists top-level directories plus up to two
// levels beneath the Go source roots (pkg/, internal/, cmd/). maxEntries is a
// safety cap for pathological or unexpectedly large trees, not an expected
// limit for loom-core's normal package layout.
//
// Why not RepoTreeDigest: that helper sorts the WHOLE tree and truncates at a
// flat entry cap; on a large repo the alphabetically-early apps/ and docs/
// subtrees exhaust the budget before pkg/ ever appears (on loom-core pkg/ first
// sorts at entry ~1100 of ~1700), so the grounding would omit the very packages
// the model must target. Rooting at the code dirs guarantees they show. An
// empty/missing root yields "" (grounding inert; mirrors the research guard).
func RepoPackageLayout(root string, maxEntries int) string {
	root = strings.TrimSpace(root)
	if root == "" || maxEntries <= 0 {
		return ""
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	seen := map[string]struct{}{}
	// Top-level directories — the real top namespace (so a sibling of pkg/
	// cannot be invented).
	if tops, derr := os.ReadDir(root); derr == nil {
		for _, e := range tops {
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			if _, skip := researchTreeSkipDirs[name]; skip {
				continue
			}
			seen[name+"/"] = struct{}{}
		}
	}
	// Two levels beneath each Go source root — the real package list.
	for _, src := range codeSourceRoots {
		srcPath := filepath.Join(root, src)
		_ = filepath.WalkDir(srcPath, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() || path == srcPath {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if _, skip := researchTreeSkipDirs[name]; skip {
				return fs.SkipDir
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			seen[rel+"/"] = struct{}{}
			// Stop at two levels beneath the source root (src/child,
			// src/child/grandchild) to keep the digest compact.
			if strings.Count(rel, "/") >= 2 {
				return fs.SkipDir
			}
			return nil
		})
	}
	if len(seen) == 0 {
		return ""
	}
	entries := make([]string, 0, len(seen))
	for k := range seen {
		entries = append(entries, k)
	}
	sort.Strings(entries)
	truncated := false
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
		truncated = true
	}
	out := strings.Join(entries, "\n")
	if truncated {
		out += "\n… (truncated)"
	}
	return out
}

func buildCouncilEditorPrompt(brief *council.Brief, reviews []council.ReviewerOutput, patterns []council.PatternRef, repoTree, memory string) string {
	stable, volatile := buildCouncilEditorPromptParts(brief, reviews, patterns, repoTree, memory)
	return stable + volatile
}

// buildCouncilEditorPromptParts splits the editor prompt at the stable ⇄
// volatile boundary so a caching backend can mark the prefix cacheable. The
// `stable` half — the instruction preamble, editor guardrails, repo-layout
// grounding, and approved-pattern catalog — is identical across every council
// run and Spinning-Room spin against the same repo, so it is what benefits from
// prompt caching (Anthropic backend puts it in a cache_control'd system block).
// The `volatile` half — this run's brief + reviewer notes — varies per run and
// stays in the user message. Concatenated (stable+volatile) they equal the
// single-string prompt the flexinfer / OpenAI editors send, so their behavior
// is byte-for-byte unchanged.
//
// `memory` is the council lane's durable cross-run journal render (empty unless
// LOOM_MILLS_COUNCIL_MEMORY is on). It is emitted STABILITY-ORDERED: constant
// preamble + guardrails → append-only memory → churning repo tree / pattern
// catalog → volatile brief. The memory block grows only by appending, so a
// warm prefix survives it; the repo digest changes whenever the tree does, so
// anything placed after it is cold on the next commit. Empty `memory` writes
// zero bytes, which is what keeps the flag-off prompt byte-identical.
func buildCouncilEditorPromptParts(brief *council.Brief, reviews []council.ReviewerOutput, patterns []council.PatternRef, repoTree, memory string) (stable, volatile string) {
	var b strings.Builder
	b.WriteString(`You are the Loom Mills council editor.

Synthesize the brief and reviewer notes into three markdown artifacts. Use these
exact top-level headings, in this order:

## Research
## Product Spec
## Implementation Plan

Keep the output implementation-ready and include machine-checkable success
criteria. Do not invent credentials or claim actions were completed.

ALWAYS end your output with this section, titled exactly:

## Backlog Proposals

containing a single fenced ` + "```json" + ` block of this shape:

{"objective": "<2-4 sentences: the end-state this plan reaches and the through-line that connects its slices — NOT a restatement of the brief>",
 "proposals": [
  {"title": "<imperative one-line summary>", "priority": "P2",
   "labels": ["docs"], "pattern_id": "<one of the approved pattern ids, or \"none\">",
   "slices": [
     {"name": "<short slice name>",
      "goal": "<one sentence: what this slice does, end to end>",
      "files": ["pkg/path/file.go"],
      "depends_on": ["<name of an earlier slice this one needs merged first>"],
      "interface_contracts": "<the contract this slice PROVIDES for later slices, or CONSUMES from earlier ones — e.g. \"publishes the FooStore schema the later slices code against\">",
      "acceptance_criteria": "<how a reviewer confirms this slice is done>"}
   ]}
]}

Rules for the JSON block:
- "objective" states the whole plan's end-state and the through-line linking its
  slices in 2-4 sentences. Do NOT echo the brief back; synthesize the goal the
  slices add up to. Omit only if there is genuinely a single slice.
- This is the NORMAL outcome for a brief that decomposes into two or more
  independently-shippable units: if the Implementation Plan above lists steps
  that each land as their own merge request, EMIT one proposal per shippable
  unit. Prefer emitting proposals over omitting them — proposals are what turn
  the plan into runnable work.
- Each entry in "slices" MUST be independently mergeable (its own MR). Only put
  multiple slices on a proposal when each truly ships on its own; never list
  sub-steps that only make sense merged together.
- "depends_on" lists the NAMES (exactly as spelled in this JSON) of the earlier
  slices that must merge before this one — the DAG edges. Reference other slices
  by their "name"; leave it out (or empty) for a slice with no prerequisites.
  Do NOT invent slice ids.
- "interface_contracts" names the concrete contract a slice hands to later slices
  (a schema, an API, a table) or relies on from earlier ones, so the slices
  compose. "acceptance_criteria" is the slice's done-definition. Both optional
  but strongly preferred — they are the connective tissue that makes the plan
  reviewable.
- "priority" is one of P0,P1,P2,P3 (default P2 if unsure). Only "title" and at
  least one slice with a real "files" entry are required; "labels", "pattern_id",
  "spec_doc", "depends_on", "interface_contracts", and "acceptance_criteria" are
  optional. Do NOT drop a proposal merely because you are unsure about a
  secondary field.
- Every path in "files" MUST be a real file from the repository layout below, or
  a NEW file inside a directory shown there.
- If — and only if — the work is a single merge-sized unit, OR you genuinely
  cannot map slices to real repo paths, emit an EMPTY list with a short reason
  instead of inventing paths or dropping the section:
  {"proposals": [], "omit_reason": "single merge-sized unit"}
  An absent or malformed "## Backlog Proposals" section is recorded as the model
  failing to follow this contract, so always emit the section.
`)
	b.WriteString(council.EditorGuardrailsPromptSection())
	b.WriteString(buildCouncilMemorySection(memory))
	b.WriteString(buildRepoLayoutSection(repoTree))
	b.WriteString(buildPatternCatalogSection(patterns))
	stable = b.String()

	// Volatile tail: this run's brief + reviewer notes.
	var v strings.Builder
	v.WriteString("\nBrief:\n")
	v.WriteString(brief.Markdown)
	if len(reviews) > 0 {
		v.WriteString("\n\nReviewer notes:\n")
	}
	for _, r := range reviews {
		fmt.Fprintf(&v, "\n### %s (%s/%s)\n%s\n",
			r.Lens.Name, r.Lens.Backend, r.Lens.Model, strings.TrimSpace(r.Markdown))
	}
	return stable, v.String()
}

// buildCouncilMemorySection wraps the council lane's durable cross-run journal
// render (council.MemoryBlock) for the editor prompt. Empty input → empty
// string, so an editor with no memory wired — or the flag off — emits the prior
// prompt verbatim.
//
// It sits between the guardrails and the repo layout on purpose: the journal
// only ever grows by appending, so the bytes above it never move and the
// engine's prefix cache keeps matching; the repo digest below it churns per
// commit, and anything placed after a churned byte is cold every tick.
func buildCouncilMemorySection(memory string) string {
	if strings.TrimSpace(memory) == "" {
		return ""
	}
	return "\n" + memory + "\n"
}

// buildRepoLayoutSection grounds the decomposition in the target repo's real
// layout. Without it the editor invents plausible-but-fictional file paths for
// every slice's "files" (the live failure that escalated every council item:
// slices scoped to pkg/planning/ and pkg/pipeline/, neither of which exists in
// loom-core). Empty digest → empty string, so an editor with no RepoRoot emits
// the prior prompt verbatim. Mirrors the research stage's prompt grounding.
func buildRepoLayoutSection(repoTree string) string {
	if strings.TrimSpace(repoTree) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Repository layout — ground EVERY file path in this\n\n")
	b.WriteString("This is the target repository's real top-of-tree layout. EVERY path in\n")
	b.WriteString("any slice's \"files\" MUST be a file shown below, or a NEW file inside a\n")
	b.WriteString("directory shown below. Do NOT invent directories that are absent from this\n")
	b.WriteString("tree (e.g. do not write `pkg/planning/...` or `pkg/pipeline/...` if they do\n")
	b.WriteString("not appear here). If you cannot map a slice to real paths in this tree,\n")
	b.WriteString("emit an EMPTY \"## Backlog Proposals\" list with an omit_reason rather than\n")
	b.WriteString("inventing paths — a fictional path is worse than no proposal, because the\n")
	b.WriteString("implement stage cannot act on it.\n\n")
	b.WriteString("```\n")
	b.WriteString(repoTree)
	b.WriteString("\n```\n")
	return b.String()
}

// fetchApprovedPatterns best-effort loads the approved-pattern catalog from
// lister (Pattern Loom A1). A nil lister or any error yields nil — the editor
// then runs without a catalog, NEVER blocking decomposition on a pattern
// fetch. Mirrors the resilience contract of the GitLab importer + plan author.
func fetchApprovedPatterns(ctx context.Context, lister council.PatternLister) []council.PatternRef {
	if lister == nil {
		return nil
	}
	patterns, err := lister.ListApprovedPatterns(ctx)
	if err != nil {
		slog.Default().Warn("council pattern catalog fetch failed; proceeding without it", "err", err)
		return nil
	}
	return patterns
}

// buildPatternCatalogSection renders the approved-pattern catalog the editor
// must conform proposals to (Pattern Loom A1). Empty catalog → empty string,
// so an editor with no PatternLister (or a fetch that returned nothing) emits
// the prior prompt verbatim and the pattern_id field stays advisory.
func buildPatternCatalogSection(patterns []council.PatternRef) string {
	if len(patterns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Approved patterns — conform to one\n\n")
	b.WriteString("Each proposal in the JSON block MUST set \"pattern_id\" to exactly one of\n")
	b.WriteString("the ids below, or to \"none\" with a one-line reason in the proposal title\n")
	b.WriteString("explaining why no approved pattern fits. Prefer conforming over \"none\".\n\n")
	for _, p := range patterns {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(p.Makes)
		if label == "" {
			label = strings.TrimSpace(p.Name)
		}
		if label != "" {
			fmt.Fprintf(&b, "- %s (%s)\n", id, label)
		} else {
			fmt.Fprintf(&b, "- %s\n", id)
		}
	}
	return b.String()
}

type councilSections struct {
	research       string
	productSpec    string
	implementation string
}

func splitCouncilSections(raw string) councilSections {
	raw = strings.TrimSpace(raw)
	sections := councilSections{
		research:       raw,
		productSpec:    "See research synthesis.",
		implementation: "See research synthesis.",
	}
	if raw == "" {
		sections.research = "No model output returned."
		return sections
	}

	type marker struct {
		key   string
		title string
		idx   int
	}
	markers := []marker{
		{key: "research", title: "## Research", idx: strings.Index(raw, "## Research")},
		{key: "product", title: "## Product Spec", idx: strings.Index(raw, "## Product Spec")},
		{key: "implementation", title: "## Implementation Plan", idx: strings.Index(raw, "## Implementation Plan")},
	}
	for _, m := range markers {
		if m.idx < 0 {
			return sections
		}
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].idx < markers[j].idx })
	values := map[string]string{}
	for i, m := range markers {
		start := m.idx + len(m.title)
		end := len(raw)
		if i+1 < len(markers) {
			end = markers[i+1].idx
		}
		values[m.key] = strings.TrimSpace(raw[start:end])
	}
	if values["research"] != "" {
		sections.research = values["research"]
	}
	if values["product"] != "" {
		sections.productSpec = values["product"]
	}
	if values["implementation"] != "" {
		sections.implementation = values["implementation"]
	}
	return sections
}

func councilModels(editorModel string, reviews []council.ReviewerOutput) []string {
	models := []string{}
	if editorModel != "" {
		models = append(models, editorModel)
	}
	for _, r := range reviews {
		if r.Lens.Model != "" {
			models = append(models, r.Lens.Model)
		}
	}
	sort.Strings(models)
	return models
}

var _ council.Reviewer = (*FlexInferCouncilReviewer)(nil)
var _ council.Editor = (*FlexInferCouncilEditor)(nil)

// FlexInferEvalJudge adapts FlexInfer chat completions to eval.LLMJudge for
// the contradiction-free council criterion.
type FlexInferEvalJudge struct {
	Client    *FlexInferClient
	Model     string
	MaxTokens int
	Backend   string
}

// JudgeContradiction implements eval.LLMJudge.
func (j *FlexInferEvalJudge) JudgeContradiction(ctx context.Context, in eval.Input) (eval.LLMJudgeResult, error) {
	result := eval.LLMJudgeResult{Backend: j.backend()}
	if j == nil || j.Client == nil {
		return result, errors.New("flexinfer eval judge: client not configured")
	}
	ctx = llmusage.WithComponent(ctx, ComponentEvalJudge)
	maxTokens := j.MaxTokens
	if maxTokens <= 0 {
		maxTokens = judgeMaxTokensFromEnv()
	}
	prompt := buildContradictionPrompt(in)
	raw, resp, cost, providerReported, err := j.Client.chatCompletionResponseCostStatus(ctx, j.Model, prompt, maxTokens, true)
	cost, providerReported = j.knownRemoteJudgeCost(resp, cost, providerReported)
	addEvalJudgeAttemptCost(&result, cost, providerReported)
	if err != nil {
		return result, err
	}
	score, findings, err := parseContradictionVerdict(raw)
	if err == nil {
		result.Score = score
		result.Findings = findings
		return result, nil
	}
	if !judgeShouldBoostRetry(resp, raw) {
		return result, err
	}

	// Reasoning models can consume the entire completion budget in a separate
	// reasoning field and leave message.content empty. Match the rubric judge's
	// bounded recovery: retry once with a larger budget and an explicit envelope,
	// while retaining the cost (or unpriced marker) from both paid attempts.
	retryPrompt := prompt + "\n\n" + contradictionJSONOnlyRetryInstruction
	retryTokens := boostedRetryTokens(maxTokens, resp)
	raw, retryResp, cost, providerReported, retryErr := j.Client.chatCompletionResponseCostStatus(
		ctx, j.Model, retryPrompt, retryTokens, true,
	)
	// A retry is independently billed; price its returned usage when LiteLLM
	// omitted usage.cost just as for the initial attempt.
	cost, providerReported = j.knownRemoteJudgeCost(retryResp, cost, providerReported)
	addEvalJudgeAttemptCost(&result, cost, providerReported)
	if retryErr != nil {
		return result, fmt.Errorf("flexinfer eval judge boosted retry: %w", retryErr)
	}
	score, findings, err = parseContradictionVerdict(raw)
	if err != nil {
		return result, fmt.Errorf("flexinfer eval judge boosted retry: %w", err)
	}
	result.Score = score
	result.Findings = findings
	return result, nil
}

func addEvalJudgeAttemptCost(result *eval.LLMJudgeResult, cost float64, providerReported bool) {
	result.CostUSD += cost
	if !isLocalCouncilBackend(result.Backend) && !providerReported {
		result.CostUnpriced = true
	}
}

// knownRemoteJudgeCost fills a LiteLLM pricing omission from the pinned council
// table. Unknown models deliberately remain unpriced so the runner retains its
// conservative admission-reservation fallback.
func (j *FlexInferEvalJudge) knownRemoteJudgeCost(resp *chatResponse, cost float64, providerReported bool) (float64, bool) {
	return knownRemoteCouncilCost(j.Model, j.backend(), resp, cost, providerReported)
}

// knownRemoteCouncilCost fills a LiteLLM pricing omission (usage tokens
// present, usage.cost absent) for any KNOWN council gateway model — judge,
// reviewer lens, or editor — from the pinned oa/ and or/ price tables. A run
// carrying unpriced paid spend is charged its full admission reservation, so
// leaving a known model unpriced turns a sub-dollar run into a $15 charge
// (COUNCIL-2026-08-03-000011). Unknown models deliberately remain unpriced.
func knownRemoteCouncilCost(model, backend string, resp *chatResponse, cost float64, providerReported bool) (float64, bool) {
	if providerReported || isLocalCouncilBackend(backend) {
		return cost, providerReported
	}
	if resp == nil || (resp.Usage.PromptTokens <= 0 && resp.Usage.CompletionTokens <= 0 &&
		llmusage.CachedTokens(resp.Usage.PromptTokensDetails, resp.Usage.InputTokensDetails) <= 0) {
		return cost, false
	}
	if priced, ok := openAICouncilChatResponseCostUSD(model, resp); ok {
		return priced, true
	}
	if priced, ok := openRouterCouncilChatResponseCostUSD(model, resp); ok {
		return priced, true
	}
	return cost, false
}

// openRouterCouncilTokenPrices pins deliberate price CEILINGS (well above the
// providers' sticker rates) for the or/ OpenRouter council models, consumed
// only when the gateway omits usage.cost. Overestimating keeps the accounting
// conservative while still beating the alternative — consuming the whole run
// reservation. Lookup is exact: aliases never inherit a rate.
var openRouterCouncilTokenPrices = map[string]openAITokenPrice{
	"or/deepseek-chat": {InputPerMillion: 2.00, CachedInputPerMillion: 2.00, OutputPerMillion: 5.00},
	"or/kimi-k3":       {InputPerMillion: 5.00, CachedInputPerMillion: 5.00, OutputPerMillion: 12.00},
}

// openRouterCouncilChatResponseCostUSD prices a known or/ council model from
// its usage block. Cached-prompt tokens are charged at the full input rate —
// no cache discount, conservative by construction.
func openRouterCouncilChatResponseCostUSD(model string, resp *chatResponse) (float64, bool) {
	if resp == nil {
		return 0, false
	}
	price, ok := openRouterCouncilTokenPrices[strings.TrimSpace(model)]
	if !ok {
		return 0, false
	}
	prompt := max(resp.Usage.PromptTokens, 0)
	completion := max(resp.Usage.CompletionTokens, 0)
	return (float64(prompt)*price.InputPerMillion +
		float64(completion)*price.OutputPerMillion) / 1_000_000, true
}

func (j *FlexInferEvalJudge) backend() string {
	if j != nil && strings.TrimSpace(j.Backend) != "" {
		return strings.TrimSpace(j.Backend)
	}
	return "flexinfer"
}

func buildContradictionPrompt(in eval.Input) string {
	var b strings.Builder
	now := time.Now().UTC()
	if in.Now != nil {
		now = in.Now().UTC()
	}
	fmt.Fprintf(&b, `Score whether this Mills council output contradicts the supplied evidence.

Evaluation time (UTC): %s

Treat the supplied sidecar and implementation plan as authoritative private system state.
Do not reject facts because they are newer than your training cutoff or unfamiliar from
public knowledge. Dates, model names, run IDs, and internal paths are not contradictions
by themselves. Flag only direct logical conflicts within the supplied evidence.
Do not infer a contradiction merely because no comparison evidence was supplied.

Return only JSON with:
{"score": <number 0..1 where 1 means no contradiction>, "findings": ["..."]}

Council sidecar:
`, now.Format(time.RFC3339))
	if in.Sidecar != nil {
		if data, err := in.Sidecar.Marshal(); err == nil {
			b.Write(data)
		}
	}
	if in.EditorOutput != nil {
		b.WriteString("\n\nImplementation plan excerpt:\n")
		for _, d := range in.EditorOutput.Documents {
			if d.Kind == council.KindImplementation {
				b.WriteString(d.Body)
				break
			}
		}
	}
	return b.String()
}

const contradictionJSONOnlyRetryInstruction = `Respond with ONLY the JSON object {"score":...,"findings":[...]} and nothing else.`

func parseContradictionVerdict(raw string) (float64, []string, error) {
	for _, candidate := range extractJSONCandidates(raw) {
		var payload struct {
			Score    *float64 `json:"score"`
			Findings []string `json:"findings"`
		}
		if err := json.Unmarshal([]byte(candidate), &payload); err != nil || payload.Score == nil {
			continue
		}
		if *payload.Score < 0 || *payload.Score > 1 {
			return 0, nil, fmt.Errorf("contradiction score %.3f outside [0,1]", *payload.Score)
		}
		return *payload.Score, payload.Findings, nil
	}
	return 0, nil, fmt.Errorf("parse contradiction verdict: no score envelope; raw=%q", truncateForLog(raw, 800))
}

var _ eval.LLMJudge = (*FlexInferEvalJudge)(nil)
