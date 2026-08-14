package council

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/textsim"
	"github.com/crb2nu/loom/pkg/mills/workflow/registry"
)

// PlanAuthor authors a first-class Plan for a backlog item and returns
// its plan_id. Declared locally (not imported from intake) so the
// council package stays decoupled from the importer; *clients.PlanClient
// satisfies both structurally. Implemented by *clients.PlanClient.
type PlanAuthor interface {
	AuthorPlan(ctx context.Context, item *store.BacklogItem, project string) (string, error)
}

// PlanSliceSpec is one independently-shippable slice the council editor
// decomposed a proposal into (.loom/163 S3). Unlike store.Slice (the fan-out
// unit, which carries no description), it carries a Goal so the downstream
// plan slice — and the BacklogItem the plan-slice emitter later emits from it
// — is self-describing.
type PlanSliceSpec struct {
	Name  string
	Goal  string
	Files []string
	// DependsOn names the earlier slices this one needs merged first (the DAG
	// edges). Referenced by slice NAME — the only stable key the editor has,
	// since it cannot know the final slice_id (<plan_id>#<order>) and the spin
	// flattens/reindexes slices. The plan store resolves names to slice_ids.
	DependsOn []string
	// InterfaceContracts is the contract this slice PROVIDES for later slices
	// and/or CONSUMES from earlier ones (e.g. slice 1 publishing the schema the
	// rest code against). Free-form; surfaced inline in the HUD as "provides: …".
	InterfaceContracts string
	// AcceptanceCriteria is the slice's done-definition.
	AcceptanceCriteria string
}

// SlicedPlanInput authors a sliced Plan directly (no flat backlog item) in a
// namespace the plan-slice emitter (S2) watches, so each slice ships as its
// own MR rather than the whole proposal fanning out into one.
type SlicedPlanInput struct {
	Title     string
	Project   string
	Namespace string
	Slices    []PlanSliceSpec
}

// SlicedPlanAuthor is the OPTIONAL extension of PlanAuthor that authors a
// sliced Plan in a watched namespace (the S2 lane). The mutator type-asserts
// for it, so a PlanAuthor that doesn't implement it simply falls back to the
// flat backlog-item path — no breakage for existing implementations or test
// fakes. *clients.PlanClient implements it.
type SlicedPlanAuthor interface {
	AuthorSlicedPlan(ctx context.Context, in SlicedPlanInput) (string, error)
}

// ExistingPlan is the minimal projection of a Plan already in the Plan Store
// that the mutator needs to dedup a to-be-authored sliced Plan against: id +
// title. Declared locally (not imported from clients) so the council package
// stays decoupled from the importer — clients imports council, so the reverse
// would be a cycle. *clients.PlanClient projects agent_plan_list rows into this.
type ExistingPlan struct {
	ID    string
	Title string
}

// PlanLister is the OPTIONAL extension a PlanAuthor may implement so the mutator
// can dedup a proposal against plans ALREADY in the target namespace before
// authoring a new one. This closes the demand-sourcing dedup gap: the flat
// backlog-item dedup (findDuplicate against backlog_items) never fired for the
// plan lane, because plan-lane proposals author Plans instead of backlog items,
// so a re-running council minted a fresh near-duplicate Plan every cron tick
// (36 plans clustered on a handful of over-served themes; 2026-07-05). Listing
// ALL phases (planned/in_progress/merged/done) means a theme already served on
// main — its plan advanced to "merged" by the take-up reconciler — keeps
// blocking re-proposal too, not just still-open plans. The mutator type-asserts
// for it, so a PlanAuthor that doesn't implement it simply authors without
// plan-store dedup (prior behavior). *clients.PlanClient implements it.
type PlanLister interface {
	ListExistingPlans(ctx context.Context, project, namespace string) ([]ExistingPlan, error)
}

// BacklogProposal is the editor's structured ask for one new backlog
// item. The mutator translates one proposal → one canonical
// store.BacklogItem + one .loom/backlog/<id>.yaml export. GitLab sync
// is a separate post-step (BacklogMutator.SyncToGitLab, slot reserved
// for the mcp-gitlab integration in a follow-up slice).
type BacklogProposal struct {
	// IDHint, if set, is used as the canonical id directly. Empty hint
	// → auto-generated MILLS-YYYY-MM-DD-NNN. Tests use IDHint for
	// determinism; the real editor leaves it empty.
	IDHint string

	Title      string
	Labels     []string
	Priority   store.Priority
	SpecDoc    string // .loom/ path back-reference
	SpecAnchor string // section anchor inside SpecDoc
	Slices     []store.Slice
	// PlanSlices, when non-empty, are independently-shippable slices the
	// editor decomposed this proposal into (.loom/163 S3). When the mutator
	// has a PlanSliceNamespace + a SlicedPlanAuthor, the proposal is routed to
	// a sliced Plan in that namespace (one MR per slice via the emitter)
	// INSTEAD of a flat backlog item, avoiding fan-out-vs-emitter double work.
	PlanSlices   []PlanSliceSpec
	Success      store.SuccessCriteria
	Budget       store.Budget
	Policy       store.ItemPolicy
	Dependencies []string

	// PatternID records which approved pattern this proposal conformed to
	// (Pattern Loom A1). It is one of the catalog ids injected into the
	// editor prompt, or "none" when no pattern applies (the editor supplies
	// a one-line reason in Notes in that case). Empty when the editor ran
	// without a pattern catalog. Surfaces in the YAML export + a log line;
	// not yet persisted to the canonical store (no column for it yet).
	PatternID string

	// Notes captures free-form rationale the editor wants to record on
	// the item. Surfaces in the YAML export only; not persisted to the
	// canonical store.
	Notes string
}

// MutationOptions tunes one Apply call. All fields optional.
type MutationOptions struct {
	// MaxNewItems caps the number of created items per run; the spec
	// requires this default to ≤ 10. A proposal slice longer than the
	// cap is truncated (deterministic prefix kept, surplus dropped).
	MaxNewItems int

	// SkipBecausePartial short-circuits the whole call. Set when the
	// eval Loop A judge marked the run partial; the mutator still
	// returns a result with TotalProposed populated so the audit log
	// records what was *intended* even though nothing landed.
	SkipBecausePartial bool

	// RepoRoot is the absolute path to the loom-core checkout the
	// mutator should write the .loom/backlog/ exports into. Empty
	// disables YAML export (canonical store still gets the inserts).
	RepoRoot string

	// DedupSimilarityThreshold is the Jaccard similarity threshold above
	// which a proposal is treated as a duplicate of an existing
	// backlog_items row and skipped. Slice 6.2: range 0..1, zero
	// substitutes the default (0.7). Set to a value >= 1 to disable
	// dedup entirely (only exact title matches would skip — and even
	// those won't, since 1 is exclusive).
	DedupSimilarityThreshold float64

	// MergedWorkGroundingDisabled turns OFF the merged-work grounding pass
	// for this call. The runner sets it from
	// `council.dedup.merged_work.enabled` (default TRUE), so the zero value
	// keeps grounding ON wherever BacklogMutator.MergedWork is wired.
	MergedWorkGroundingDisabled bool

	// MergedWorkLookback overrides how far back the merged-MR corpus reaches.
	// Zero substitutes defaultMergedWorkLookback (14d).
	MergedWorkLookback time.Duration
}

// defaultDedupThreshold is the Jaccard cutoff that flags two titles as
// the same proposal. 0.7 is the spec-recommended starting point: most
// re-asks of the same plan produce ≥ 0.8 overlap, while genuinely
// distinct slices stay well under. Tunable via MutationOptions.
const defaultDedupThreshold = 0.7

// MutationResult is the audit footprint of one Apply call.
type MutationResult struct {
	TotalProposed     int
	Skipped           bool
	SkipReason        string
	CreatedItems      []*store.BacklogItem
	CreatedYAMLPath   []string
	Truncated         int                // proposals dropped to honour MaxNewItems
	DuplicatesSkipped []DuplicateSkipped // backlog-item dedup matches (slice 6.2)
	// PlanDuplicatesSkipped records plan-lane proposals dropped because a
	// near-duplicate Plan already lives in the target namespace (cross-run
	// Plan Store pollution) or was authored earlier in this same batch. Empty
	// when the plan lane is off or the PlanAuthor is not a PlanLister.
	PlanDuplicatesSkipped []PlanDuplicateSkipped
	// MergedWorkSkipped records proposals suppressed because they restated a
	// merge request that already landed on the target branch. Empty when no
	// MergedWorkSource is wired, when policy disabled grounding, or when the
	// merged-MR snapshot failed (grounding fails open).
	MergedWorkSkipped []MergedWorkSkipped
	// RoutedPlanLane holds the plan_ids of proposals routed to the S2 plan
	// lane (sliced Plan in the emitter namespace, no flat item) instead of a
	// flat backlog item (.loom/163 S3).
	RoutedPlanLane []string

	// Slice-grounding guard (OUTPUT side of MR !848): proposal slices whose
	// `files` all referenced not-yet-existing directories are kept and
	// recorded as SlicesSpeculative (flag-never-drop, 2026-06-30); slices
	// mixing real and new-directory paths are kept and flagged
	// (SlicesFlagged). SlicesDropped / FictionalProposalsDropped are retained
	// for compatibility and stay zero (the guard no longer drops on grounding
	// grounds). All zero when RepoRoot is unset (guard inert). Mirrors the
	// research stage.
	SlicesSpeculative         int
	SlicesFlagged             int
	SlicesDropped             int
	FictionalProposalsDropped int
}

// DuplicateSkipped records one proposal the mutator dropped because it
// looked like an existing item. Surfaces in events + the council run
// summary so the editor can avoid re-proposing on the next run.
type DuplicateSkipped struct {
	ProposalIndex int     `json:"proposal_index"`
	ProposalTitle string  `json:"proposal_title"`
	SimilarToID   string  `json:"similar_to_id"`
	SimilarTitle  string  `json:"similar_title"`
	JaccardScore  float64 `json:"jaccard_score"`
}

// PlanDuplicateSkipped records one plan-lane proposal the mutator did NOT
// author because a near-duplicate Plan already exists in the target namespace
// (or was authored earlier in the same Apply). SimilarPlanID is the id of the
// blocking Plan so an operator can trace the collision.
type PlanDuplicateSkipped struct {
	ProposalIndex int     `json:"proposal_index"`
	ProposalTitle string  `json:"proposal_title"`
	SimilarPlanID string  `json:"similar_plan_id"`
	SimilarTitle  string  `json:"similar_title"`
	JaccardScore  float64 `json:"jaccard_score"`
}

// defaultMaxNewItems matches the spec's "≤ 10 new items per council
// run" rule. Tunable via MutationOptions.MaxNewItems.
const defaultMaxNewItems = 10

// BacklogMutator translates EditorOutput.BacklogProposals into canonical
// store inserts + derived YAML exports. Canonical store wins as the
// source of truth; YAML is regenerated on every run from the canonical
// view so manual YAML edits are imported via reconciler diff (slice 4.x).
type BacklogMutator struct {
	Store *store.Store
	// Recorder, when set, writes the mutator's guarded-action audit trail
	// (actor "council.mutator") into the events table — the Mill Staff
	// unified audit the overseers established. Item creates Record (the
	// wired DryRun reports false: the council's artifact-dryrun path never
	// reaches Apply, so a mutation that happens is always real); dedup
	// skips Observe (reality notes, committed kind regardless of dry-run).
	// Nil = no audit (tests, minimal wiring); never blocks a mutation.
	Recorder *guard.ActionRecorder
	// Now is injectable for deterministic IDs.
	Now func() time.Time
	// PlanAuthor, when set, authors a first-class Plan for each created
	// item and stamps its plan_id so the item is born linked (plan store
	// S7b-γ), instead of waiting for the boot-time backfill. Nil =
	// disabled (the default); the backfill still links items later. Set by
	// the operator when LOOM_MILLS_PLAN_AUTHORING is enabled and the MCP
	// hub is reachable.
	PlanAuthor PlanAuthor
	// Project scopes authored plans (canonical GitLab path).
	Project string
	// PlanSliceNamespace, when non-empty, turns on the S2 plan lane: a
	// proposal carrying PlanSlices is authored as a sliced Plan in this
	// namespace (which the plan-slice emitter watches) WITHOUT a flat backlog
	// item, so each slice ships as its own MR. Empty = off (sliced proposals
	// fall back to the flat/fan-out path, preserving prior behavior). Set by
	// the operator from policy.intake.plan_slice_emitter.namespace. (.loom/163 S3)
	PlanSliceNamespace string
	// MergedWork, when set, grounds every proposal against the merge requests
	// the target branch has already taken, so the council stops re-proposing
	// work that shipped between its brief and its mutation. Nil = off (the
	// default, and what tests and GitLab-less deployments get). Set by the
	// operator after the GitLab client exists, the same late-injection the
	// PlanAuthor above uses.
	MergedWork MergedWorkSource
	// Logger is used for best-effort plan-authoring diagnostics. Nil falls
	// back to slog.Default().
	Logger *slog.Logger
}

// logger returns the configured logger or the slog default.
// audit best-effort-writes one staff-audit event; record=true uses the
// dry-run-resolved Record path (mutations), false uses Observe (reality
// notes). A write failure is logged and never blocks the mutation.
func (m *BacklogMutator) audit(ctx context.Context, record bool, action, subjectID string, payload map[string]any) {
	m.auditSubject(ctx, record, action, "backlog", subjectID, payload)
}

// auditSubject is audit for a subject that is not a backlog item — merged-work
// grounding names the merge request that suppressed the proposal, so the events
// table points at the work rather than at an item that was never created.
func (m *BacklogMutator) auditSubject(ctx context.Context, record bool, action, subjectKind, subjectID string, payload map[string]any) {
	if m == nil || m.Recorder == nil {
		return
	}
	var err error
	if record {
		err = m.Recorder.Record(ctx, action, subjectKind, subjectID, payload)
	} else {
		err = m.Recorder.Observe(ctx, action, subjectKind, subjectID, payload)
	}
	if err != nil {
		m.logger().Warn("council mutator audit write failed", "action", action, "error", err)
	}
}

func (m *BacklogMutator) logger() *slog.Logger {
	if m != nil && m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Apply persists every accepted proposal into the canonical store, then
// writes a derived YAML for each. Returns the populated result so the
// caller can update store.CouncilRun.BacklogDeltas in one pass.
func (m *BacklogMutator) Apply(ctx context.Context, runID string, out *EditorOutput, opts MutationOptions) (*MutationResult, error) {
	if m == nil || m.Store == nil {
		return nil, errors.New("council: BacklogMutator not configured")
	}
	if out == nil {
		return nil, errors.New("council: EditorOutput required")
	}
	if runID == "" {
		return nil, errors.New("council: runID required")
	}

	res := &MutationResult{TotalProposed: len(out.BacklogProposals)}
	if opts.SkipBecausePartial {
		res.Skipped = true
		res.SkipReason = "council run scored below eval threshold; mutations dropped"
		return res, nil
	}

	cap := opts.MaxNewItems
	if cap <= 0 {
		cap = defaultMaxNewItems
	}
	proposals := out.BacklogProposals
	if len(proposals) > cap {
		res.Truncated = len(proposals) - cap
		proposals = proposals[:cap]
	}

	// Slice-grounding guard (OUTPUT side of MR !848): drop proposal slices
	// whose `files` all reference directories absent from the repo, and the
	// proposals left slice-less by it, before any persistence. Fail-open —
	// only runs when RepoRoot resolves to a real directory; otherwise the
	// predicate would read every slice as fictional and drop legitimate work.
	if root := strings.TrimSpace(opts.RepoRoot); repoRootIsDir(root) {
		guarded, g := SanitizeProposalSlices(proposals, repoDirChecker(root))
		proposals = guarded
		if g.guarded() {
			res.SlicesSpeculative = g.SlicesSpeculative
			res.SlicesFlagged = g.SlicesFlagged
			res.SlicesDropped = g.SlicesDropped
			res.FictionalProposalsDropped = g.ProposalsDropped
			if g.SlicesSpeculative > 0 {
				mills.CouncilSlicesGuardTotal.WithLabelValues("speculative").Add(float64(g.SlicesSpeculative))
			}
			if g.SlicesFlagged > 0 {
				mills.CouncilSlicesGuardTotal.WithLabelValues("flagged").Add(float64(g.SlicesFlagged))
			}
			if g.SlicesDropped > 0 {
				mills.CouncilSlicesGuardTotal.WithLabelValues("dropped").Add(float64(g.SlicesDropped))
			}
			mills.CouncilSlicePathsDroppedTotal.Add(float64(len(g.DroppedPaths)))
			// Kept + recorded, not dropped (flag-never-drop); log at Info.
			m.logger().Info("council proposal slices referenced not-yet-existing repo directories; kept and recorded",
				"run_id", runID,
				"slices_speculative", g.SlicesSpeculative,
				"slices_flagged", g.SlicesFlagged,
				"paths", strings.Join(g.DroppedPaths, ","),
				"repo_root", root)
		}
	}

	// S7 template-selection guard: a council may select an imperative workflow
	// template, but only from the CLOSED registry
	// (pkg/mills/workflow/registry.go). An invalid selection — unknown
	// name/version or a rejected enum — is stripped at authoring so the item
	// lands as a normal DAG item instead of wedging fail-closed at admission.
	// Admission-side ResolveItemSelection remains the backstop for items
	// authored outside the council.
	templates := registry.NewDefault()
	for i := range proposals {
		p := &proposals[i]
		name := strings.TrimSpace(p.Policy.WorkflowTemplate)
		if name == "" {
			continue
		}
		if err := templates.Validate(
			name, strings.TrimSpace(p.Policy.WorkflowTemplateVersion),
			p.Policy.WorkflowParams, p.Policy.WorkflowEnums,
		); err != nil {
			m.logger().Warn("council proposal selected an invalid workflow template; stripped to DAG",
				"title", p.Title, "template", name,
				"version", p.Policy.WorkflowTemplateVersion, "error", err)
			p.Policy.WorkflowTemplate = ""
			p.Policy.WorkflowTemplateVersion = ""
			p.Policy.WorkflowParams = nil
			p.Policy.WorkflowEnums = nil
		}
	}

	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}

	threshold := opts.DedupSimilarityThreshold
	if threshold == 0 {
		threshold = defaultDedupThreshold
	}

	// Snapshot the canonical backlog once per Apply so we don't pay the
	// query cost per proposal. Just-created items in this same Apply are
	// appended to the candidate pool below so within-batch duplicates
	// also dedup against each other.
	existing, err := m.Store.Backlog.List(ctx)
	if err != nil {
		return res, fmt.Errorf("dedup snapshot: %w", err)
	}
	candidates := make([]*store.BacklogItem, 0, len(existing)+len(proposals))
	candidates = append(candidates, existing...)

	// Snapshot the Plan Store for the plan lane's namespace once per Apply so
	// each plan-lane proposal can dedup against Plans already authored there —
	// the flat backlog-item dedup above never covers the plan lane (those
	// proposals author Plans, not backlog items). Fail-OPEN: a plan-store read
	// blip must never block the council from authoring, so on error we log and
	// author without plan dedup (a re-run's identical title still upserts the
	// same deterministic plan id; only near-duplicate titles can slip through
	// on a failed-snapshot run). Skipped entirely when the plan lane is off or
	// the PlanAuthor is not a PlanLister.
	planNS := strings.TrimSpace(m.PlanSliceNamespace)
	var planCandidates []ExistingPlan
	if planNS != "" {
		if pl, ok := m.PlanAuthor.(PlanLister); ok {
			existingPlans, plErr := pl.ListExistingPlans(ctx, m.Project, planNS)
			if plErr != nil {
				m.logger().Warn("council plan-dedup snapshot failed; authoring without plan-store dedup",
					"run_id", runID, "namespace", planNS, "err", plErr)
			} else {
				planCandidates = existingPlans
			}
		}
	}

	// Snapshot what already merged. The council's brief is assembled before the
	// tick's merges land, so it keeps re-proposing shipped work: on 2026-08-04
	// three of five sparks collided with just-merged MRs (!1419/!1424) or with
	// a sibling item's bolt, and each burned its escalation attempts on an
	// empty diff and a spec_conformance failure. Neither dedup corpus catches
	// that — the backlog snapshot only knows items the council itself authored,
	// and a hand-written or sibling-slice MR appears in neither.
	//
	// Fail-OPEN, deliberately: grounding is a quality guard, not an admission
	// gate, so a GitLab outage leaves the corpus empty (proposals proceed
	// ungrounded) rather than blocking the council. The error counter is the
	// only signal that the guard silently went away.
	var mergedWork []MergedWork
	if m.MergedWork != nil && !opts.MergedWorkGroundingDisabled {
		lookback := opts.MergedWorkLookback
		if lookback <= 0 {
			lookback = defaultMergedWorkLookback
		}
		merged, mwErr := m.MergedWork.ListMergedWork(ctx, now.Add(-lookback))
		if mwErr != nil {
			mills.CouncilMergedWorkErrorsTotal.Inc()
			m.logger().Warn("council merged-work snapshot failed; authoring without merged-work grounding",
				"run_id", runID, "lookback", lookback.String(), "err", mwErr)
		} else {
			mergedWork = merged
		}
	}

	for i, p := range proposals {
		if dup := findDuplicate(p.Title, candidates, threshold); dup != nil {
			res.DuplicatesSkipped = append(res.DuplicatesSkipped, DuplicateSkipped{
				ProposalIndex: i,
				ProposalTitle: p.Title,
				SimilarToID:   dup.item.ID,
				SimilarTitle:  dup.item.Title,
				JaccardScore:  dup.score,
			})
			m.audit(ctx, false, "dedup_skip", dup.item.ID, map[string]any{
				"run_id": runID, "basis": "hard", "proposal_title": p.Title, "score": dup.score,
			})
			continue
		}
		// Gray-band dedup: a reworded re-mint of a RECENT theme scores below
		// the hard threshold — "Add external CI incident classification for
		// GitLab pipeline failures" vs "Add GitLab CI external dependency
		// incident classification to Mills" is Jaccard 0.6 (!970 vs !978,
		// re-minted a third time as !980 the same day). Lowering the hard
		// threshold globally would false-positive on legit near-twins ("…
		// for Mills runs" vs "… for Weaver runs"), so the gray band only
		// fires when the candidate is fresh (created within the window) —
		// in ANY state. Unresolved: don't mint a theme's twin while the
		// first attempt is in flight. Merged: the council's brief doesn't
		// know last week's item shipped, so it re-proposes the same
		// deliverable reworded (six near-identical CI triage runbooks in
		// five days, #308). Only an OLD candidate never gray-blocks — a
		// successor to long-shipped work is legit follow-up.
		if dup := findGrayBandDuplicate(p.Title, candidates, threshold, now); dup != nil {
			res.DuplicatesSkipped = append(res.DuplicatesSkipped, DuplicateSkipped{
				ProposalIndex: i,
				ProposalTitle: p.Title,
				SimilarToID:   dup.item.ID,
				SimilarTitle:  dup.item.Title,
				JaccardScore:  dup.score,
			})
			m.audit(ctx, false, "dedup_skip", dup.item.ID, map[string]any{
				"run_id": runID, "basis": "gray_band", "proposal_title": p.Title, "score": dup.score,
			})
			m.logger().Info("council skipped gray-band re-mint of a recent theme",
				"run_id", runID, "title", p.Title,
				"similar_to", dup.item.ID, "similar_title", dup.item.Title,
				"score", dup.score, "similar_state", string(dup.item.State))
			continue
		}
		// Merged-work grounding: the proposal restates something main already
		// took. Suppress it under its own action so the promotion report counts
		// stale-brief re-proposals separately from backlog dedup — they have
		// different fixes (refresh the brief's merged-MR section vs. tune the
		// dedup threshold). Runs after both backlog bands so an item the
		// council itself authored still reports as dedup_skip, and before the
		// plan lane so it covers sliced and flat proposals alike.
		if hit := findMergedWork(p.Title, mergedWork, threshold, now); hit != nil {
			res.MergedWorkSkipped = append(res.MergedWorkSkipped, MergedWorkSkipped{
				ProposalIndex: i,
				ProposalTitle: p.Title,
				MergedIID:     hit.work.IID,
				MergedTitle:   hit.work.Title,
				MergedURL:     hit.work.WebURL,
				JaccardScore:  hit.score,
				Basis:         hit.basis,
			})
			mills.CouncilMergedWorkSkippedTotal.WithLabelValues(hit.basis).Inc()
			m.auditSubject(ctx, false, "merged_work_skip", "merge_request", hit.work.Ref(), map[string]any{
				"run_id": runID, "basis": hit.basis, "proposal_title": p.Title,
				"merged_title": hit.work.Title, "merged_url": hit.work.WebURL, "score": hit.score,
			})
			m.logger().Info("council skipped proposal restating recently-merged work",
				"run_id", runID, "title", p.Title,
				"merged_ref", hit.work.Ref(), "merged_title", hit.work.Title,
				"score", hit.score, "basis", hit.basis)
			continue
		}
		// S3 (.loom/163): a proposal the editor decomposed into independently-
		// shippable slices is routed to a sliced Plan in the emitter's watched
		// namespace (one MR per slice) instead of a flat backlog item — but
		// ONLY when the S2 lane is wired (namespace set + a SlicedPlanAuthor).
		// On any failure, fall through to the flat item so work is never
		// silently dropped. The emitter dedups its per-slice items by id, so
		// no double execution against the flat/fan-out path.
		if ns := strings.TrimSpace(m.PlanSliceNamespace); ns != "" && len(p.PlanSlices) > 0 {
			if sa, ok := m.PlanAuthor.(SlicedPlanAuthor); ok {
				// Plan-level dedup: skip authoring when a near-duplicate Plan is
				// already in the namespace (any phase — a merged theme counts) or
				// was authored earlier in this same batch. This is what stops the
				// council re-minting a fresh Plan for an already-served theme on
				// every cron tick. Fail-open snapshots leave planCandidates empty,
				// which just means no dedup this run (never a wrongful drop).
				if dup := findDuplicatePlan(p.Title, planCandidates, threshold); dup != nil {
					res.PlanDuplicatesSkipped = append(res.PlanDuplicatesSkipped, PlanDuplicateSkipped{
						ProposalIndex: i,
						ProposalTitle: p.Title,
						SimilarPlanID: dup.plan.ID,
						SimilarTitle:  dup.plan.Title,
						JaccardScore:  dup.score,
					})
					mills.CouncilPlanDedupSkippedTotal.Inc()
					m.audit(ctx, false, "dedup_skip", dup.plan.ID, map[string]any{
						"run_id": runID, "basis": "plan_lane", "proposal_title": p.Title, "score": dup.score,
					})
					m.logger().Info("council skipped near-duplicate demand-sourcing plan",
						"run_id", runID, "title", p.Title,
						"similar_plan_id", dup.plan.ID, "similar_title", dup.plan.Title,
						"score", dup.score, "namespace", ns)
					continue
				}
				planID, perr := sa.AuthorSlicedPlan(ctx, SlicedPlanInput{
					Title:     p.Title,
					Project:   m.Project,
					Namespace: ns,
					Slices:    p.PlanSlices,
				})
				if perr == nil && planID != "" {
					res.RoutedPlanLane = append(res.RoutedPlanLane, planID)
					// Record the just-authored Plan so a second near-duplicate
					// proposal LATER in this same batch dedups against it (the
					// namespace snapshot above predates this author call).
					planCandidates = append(planCandidates, ExistingPlan{ID: planID, Title: p.Title})
					m.logger().Info("council routed sliced proposal to plan lane",
						"plan_id", planID, "title", p.Title,
						"slices", len(p.PlanSlices), "namespace", ns)
					continue
				}
				m.logger().Warn("council sliced-plan routing failed; falling back to flat item",
					"title", p.Title, "err", perr)
			}
		}
		item, err := m.persistOne(ctx, runID, now, i, p)
		if err != nil {
			return res, fmt.Errorf("persist proposal %d (%q): %w", i, p.Title, err)
		}
		res.CreatedItems = append(res.CreatedItems, item)
		candidates = append(candidates, item)
	}

	if opts.RepoRoot != "" {
		if err := m.exportYAML(opts.RepoRoot, res); err != nil {
			return res, fmt.Errorf("export yaml: %w", err)
		}
	}
	return res, nil
}

// dedupHit pairs a duplicate match with the score that produced it.
type dedupHit struct {
	item  *store.BacklogItem
	score float64
}

// findDuplicate returns the highest-scoring candidate whose Jaccard
// similarity to title meets or exceeds threshold, or nil when nothing
// crosses the bar. Empty titles never dedup (defensive — Title is
// validated as non-empty earlier in persistOne, but Apply may run
// before that check).
func findDuplicate(title string, candidates []*store.BacklogItem, threshold float64) *dedupHit {
	if title == "" || threshold <= 0 || threshold > 1 {
		return nil
	}
	tokens := textsim.NormalizeTitleTokens(title)
	if len(tokens) == 0 {
		return nil
	}
	var best *dedupHit
	for _, c := range candidates {
		score := textsim.Jaccard(tokens, textsim.NormalizeTitleTokens(c.Title))
		if score < threshold {
			continue
		}
		if best == nil || score > best.score {
			best = &dedupHit{item: c, score: score}
		}
	}
	return best
}

// grayBandRecentWindow bounds how far back the gray-band dedup looks. An
// item OLDER than this no longer blocks similar proposals — a theme wedged
// for weeks shouldn't suppress fresh attempts forever, and a successor to
// long-shipped work is legitimate follow-up rather than a re-mint.
const grayBandRecentWindow = 7 * 24 * time.Hour

// findGrayBandDuplicate returns the highest-scoring candidate in the gray
// band [grayBandFloor, threshold) that is RECENT (created within
// grayBandRecentWindow of now) — ANY state, merged included. Candidates at
// or above threshold are the hard dedup's business — callers run this only
// after findDuplicate returned nil. Stale candidates never match: a
// similar successor to work shipped more than a window ago is legitimate
// follow-up, not a re-mint. Recently-MERGED lookalikes DO block: the
// council's brief doesn't know last week's item shipped, so it re-proposes
// the same deliverable reworded — main accumulated six near-identical CI
// triage runbooks in five days before this (docs/ci-incident-triage.md,
// ci-triage-runbook.md, council-external-dependency-incidents.md,
// mills-escalation-and-dependency-failures.md, mills-incident-triage.md,
// plus MILLS-2026-07-10-003/#308 blocked only by the scope gate).
func findGrayBandDuplicate(title string, candidates []*store.BacklogItem, threshold float64, now time.Time) *dedupHit {
	// threshold > 1 is the documented "dedup disabled" escape hatch (see
	// MutationOptions.DedupSimilarityThreshold) — the gray band respects it.
	if title == "" || threshold <= textsim.GrayBandFloor || threshold > 1 {
		return nil
	}
	tokens := textsim.NormalizeTitleTokens(title)
	if len(tokens) == 0 {
		return nil
	}
	var best *dedupHit
	for _, c := range candidates {
		if c.CreatedAt.IsZero() || now.Sub(c.CreatedAt) > grayBandRecentWindow {
			continue
		}
		score := textsim.Jaccard(tokens, textsim.NormalizeTitleTokens(c.Title))
		if score < textsim.GrayBandFloor || score >= threshold {
			continue
		}
		if best == nil || score > best.score {
			best = &dedupHit{item: c, score: score}
		}
	}
	return best
}

// planDedupHit pairs a matched existing Plan with the score that produced it.
type planDedupHit struct {
	plan  ExistingPlan
	score float64
}

// findDuplicatePlan is findDuplicate's Plan-Store twin: the highest-scoring
// existing Plan whose Jaccard title similarity meets or exceeds threshold, or
// nil when nothing crosses the bar. Shares the textsim tokenizer with the
// backlog-item path so the two dedup surfaces stay behaviorally identical
// (same stopwords, same 0.7 default cutoff).
func findDuplicatePlan(title string, candidates []ExistingPlan, threshold float64) *planDedupHit {
	if title == "" || threshold <= 0 || threshold > 1 {
		return nil
	}
	tokens := textsim.NormalizeTitleTokens(title)
	if len(tokens) == 0 {
		return nil
	}
	var best *planDedupHit
	for _, c := range candidates {
		score := textsim.Jaccard(tokens, textsim.NormalizeTitleTokens(c.Title))
		if score < threshold {
			continue
		}
		if best == nil || score > best.score {
			best = &planDedupHit{plan: c, score: score}
		}
	}
	return best
}

// proposalItemSlices resolves the slices to stamp onto a flat backlog item.
// It prefers the proposal's explicit store-shaped Slices, but FALLS BACK to
// the editor's parsed PlanSlices when Slices is empty. Without this fallback
// a council-decomposed proposal that took the flat-item path (the S2 plan
// lane is off, OR sliced-plan routing failed and Apply fell through here)
// landed with no slices at all: the parser only ever fills PlanSlices, and
// persistOne used to copy the always-empty Slices field. A slice-less item
// trips the scope gate ("backlog item has no slices; no scope to enforce")
// on every implement attempt, so the run retries until it escalates with an
// empty diff — which is exactly how every council item escalated 2026-06-28
// /29 (MILLS-2026-06-28-001/002, -29-001) while every sliced canary merged.
// The PlanSlice goal is intentionally dropped (store.Slice has no goal field);
// only Name + Files matter to the scope gate. Tests stay empty — the scope
// gate's AllowTests carve-out already lets incidental test files through.
func proposalItemSlices(p BacklogProposal) []store.Slice {
	if len(p.Slices) > 0 {
		return append([]store.Slice(nil), p.Slices...)
	}
	if len(p.PlanSlices) == 0 {
		return nil
	}
	out := make([]store.Slice, 0, len(p.PlanSlices))
	for _, ps := range p.PlanSlices {
		name := strings.TrimSpace(ps.Name)
		if name == "" {
			continue
		}
		out = append(out, store.Slice{
			Name:  name,
			Files: append([]string(nil), ps.Files...),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveBacklogIdentity preserves explicit IDHint upsert compatibility while
// making automatic IDs safe across multiple council runs on the same day. An
// explicit collision carries the current CAS versions into Put; an automatic
// collision advances deterministically to the first free sequence.
func (m *BacklogMutator) resolveBacklogIdentity(
	ctx context.Context, now time.Time, idx int, hint string,
) (string, *store.BacklogItem, error) {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		existing, err := m.Store.Backlog.Get(ctx, hint)
		switch {
		case err == nil:
			return hint, existing, nil
		case errors.Is(err, store.ErrNotFound):
			return hint, nil, nil
		default:
			return "", nil, fmt.Errorf("resolve backlog id %s: %w", hint, err)
		}
	}

	prefix := "MILLS-" + now.Format("2006-01-02") + "-"
	for sequence := idx + 1; ; sequence++ {
		id := fmt.Sprintf("%s%03d", prefix, sequence)
		_, err := m.Store.Backlog.Get(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return id, nil, nil
		case err != nil:
			return "", nil, fmt.Errorf("probe backlog id %s: %w", id, err)
		}
	}
}

// persistOne writes one BacklogProposal to the canonical store. The id is
// either the explicit hint or the first free MILLS-YYYY-MM-DD-NNN sequence at
// or after this proposal's 1-based index.
func (m *BacklogMutator) persistOne(ctx context.Context, runID string, now time.Time, idx int, p BacklogProposal) (*store.BacklogItem, error) {
	id, existing, err := m.resolveBacklogIdentity(ctx, now, idx, p.IDHint)
	if err != nil {
		return nil, err
	}
	if p.Title == "" {
		return nil, fmt.Errorf("proposal missing Title")
	}
	priority := p.Priority
	if priority == "" {
		priority = store.P2
	}
	council := runID
	item := &store.BacklogItem{
		ID:           id,
		Title:        p.Title,
		Labels:       append([]string(nil), p.Labels...),
		State:        store.BacklogQueued,
		Priority:     priority,
		SpecDoc:      p.SpecDoc,
		SpecAnchor:   p.SpecAnchor,
		Success:      p.Success,
		Budget:       p.Budget,
		Policy:       p.Policy,
		Slices:       proposalItemSlices(p),
		Dependencies: append([]string(nil), p.Dependencies...),
		CouncilRunID: &council,
		CreatedBy:    "council",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if existing != nil {
		item.ClaimVersion = existing.ClaimVersion
		item.Revision = existing.Revision
		item.CreatedAt = existing.CreatedAt
	}
	// Record which approved pattern the editor conformed this proposal to
	// (Pattern Loom A1). There is no canonical column yet, so this is a
	// best-effort audit log line + the YAML export below; it never blocks
	// the insert.
	if pid := strings.TrimSpace(p.PatternID); pid != "" {
		m.logger().Info("council proposal conformed to pattern",
			"id", item.ID, "pattern_id", pid, "title", item.Title)
	}
	// Born-linked: author a Plan before the first Put when inline authoring
	// is enabled, so the persisted item already carries a plan_id. Best-
	// effort — a failure leaves the item unlinked and it still persists
	// (the boot backfill links it later), so plan authoring never blocks
	// the council from creating backlog items.
	m.maybeAuthorPlan(ctx, item)
	if err := m.Store.Backlog.Put(ctx, item); err != nil {
		return nil, err
	}
	m.audit(ctx, true, "create_item", item.ID, map[string]any{
		"run_id": runID, "title": item.Title, "priority": string(item.Priority),
	})
	return item, nil
}

// maybeAuthorPlan authors a Plan for item and stamps item.PlanID when an
// inline PlanAuthor is configured. Best-effort: a nil author or any
// failure is a no-op (the item still persists; the boot backfill can link
// it later), mirroring the GitLab importer's resilience.
func (m *BacklogMutator) maybeAuthorPlan(ctx context.Context, item *store.BacklogItem) {
	if m.PlanAuthor == nil || item == nil || item.PlanID != "" {
		return
	}
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	planID, err := m.PlanAuthor.AuthorPlan(ctx, item, m.Project)
	if err != nil {
		logger.Warn("council plan authoring failed", "id", item.ID, "err", err)
		return
	}
	if planID != "" {
		item.PlanID = planID
		logger.Info("council authored plan for item", "id", item.ID, "plan_id", planID)
	}
}

// backlogYAML is the on-disk shape of one .loom/backlog/<id>.yaml
// export. Mirrors .loom/90- §"Backlog item YAML" — fields named
// snake_case for git diff readability.
type backlogYAML struct {
	ID             string                `yaml:"id"`
	GitLabIssueIID *int64                `yaml:"gitlab_issue_iid,omitempty"`
	Title          string                `yaml:"title"`
	Labels         []string              `yaml:"labels,omitempty"`
	State          string                `yaml:"state"`
	Priority       string                `yaml:"priority"`
	SpecDoc        string                `yaml:"spec_doc,omitempty"`
	SpecAnchor     string                `yaml:"spec_anchor,omitempty"`
	Slices         []store.Slice         `yaml:"slices,omitempty"`
	Success        store.SuccessCriteria `yaml:"success,omitempty"`
	Budget         store.Budget          `yaml:"budget,omitempty"`
	Policy         store.ItemPolicy      `yaml:"policy,omitempty"`
	Dependencies   []string              `yaml:"dependencies,omitempty"`
	CreatedBy      string                `yaml:"created_by"`
	CreatedAt      time.Time             `yaml:"created_at"`
	CouncilRunID   string                `yaml:"council_run_id,omitempty"`
}

// exportYAML writes one .loom/backlog/<id>.yaml per created item. Files
// are written atomically (tempfile+rename) so the watcher hooks don't
// observe a partial document.
func (m *BacklogMutator) exportYAML(repoRoot string, res *MutationResult) error {
	dir := filepath.Join(repoRoot, ".loom", "backlog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir backlog: %w", err)
	}
	for _, item := range res.CreatedItems {
		out := backlogYAML{
			ID:             item.ID,
			GitLabIssueIID: item.GitLabIssueIID,
			Title:          item.Title,
			Labels:         item.Labels,
			State:          string(item.State),
			Priority:       string(item.Priority),
			SpecDoc:        item.SpecDoc,
			SpecAnchor:     item.SpecAnchor,
			Slices:         item.Slices,
			Success:        item.Success,
			Budget:         item.Budget,
			Policy:         item.Policy,
			Dependencies:   item.Dependencies,
			CreatedBy:      item.CreatedBy,
			CreatedAt:      item.CreatedAt,
		}
		if item.CouncilRunID != nil {
			out.CouncilRunID = *item.CouncilRunID
		}
		body, err := yaml.Marshal(&out)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", item.ID, err)
		}
		path := filepath.Join(dir, item.ID+".yaml")
		if err := writeFileAtomicCouncil(path, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", item.ID, err)
		}
		res.CreatedYAMLPath = append(res.CreatedYAMLPath,
			filepath.Join(".loom", "backlog", item.ID+".yaml"))
	}
	return nil
}

// CreatedIDs is a tiny helper for callers updating
// store.CouncilRun.BacklogDeltas without iterating themselves.
func (r *MutationResult) CreatedIDs() []string {
	out := make([]string, 0, len(r.CreatedItems))
	for _, it := range r.CreatedItems {
		out = append(out, it.ID)
	}
	return out
}

// summarizeReasons is exported so the operator's structured logging
// can render a short single-string summary of a mutation result.
func (r *MutationResult) Summary() string {
	if r.Skipped {
		return r.SkipReason
	}
	parts := []string{
		fmt.Sprintf("created=%d", len(r.CreatedItems)),
		fmt.Sprintf("proposed=%d", r.TotalProposed),
	}
	if r.Truncated > 0 {
		parts = append(parts, fmt.Sprintf("truncated=%d", r.Truncated))
	}
	if n := len(r.DuplicatesSkipped); n > 0 {
		parts = append(parts, fmt.Sprintf("deduped=%d", n))
	}
	if n := len(r.PlanDuplicatesSkipped); n > 0 {
		parts = append(parts, fmt.Sprintf("plan_deduped=%d", n))
	}
	if n := len(r.MergedWorkSkipped); n > 0 {
		parts = append(parts, fmt.Sprintf("merged_work_skipped=%d", n))
	}
	if n := len(r.RoutedPlanLane); n > 0 {
		parts = append(parts, fmt.Sprintf("plan_lane=%d", n))
	}
	if r.SlicesDropped > 0 || r.FictionalProposalsDropped > 0 {
		parts = append(parts, fmt.Sprintf("slices_dropped=%d", r.SlicesDropped))
	}
	if r.FictionalProposalsDropped > 0 {
		parts = append(parts, fmt.Sprintf("fictional_proposals=%d", r.FictionalProposalsDropped))
	}
	if r.SlicesFlagged > 0 {
		parts = append(parts, fmt.Sprintf("slices_flagged=%d", r.SlicesFlagged))
	}
	if r.SlicesSpeculative > 0 {
		parts = append(parts, fmt.Sprintf("slices_speculative=%d", r.SlicesSpeculative))
	}
	return strings.Join(parts, " ")
}
