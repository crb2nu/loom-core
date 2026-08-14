package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// PlanReader is the emitter's view of the Plan Store read surface, satisfied
// by *clients.PlanClient. Trimmed to the calls the emitter needs so tests can
// fake it without standing up the MCP hub.
type PlanReader interface {
	ListPlans(ctx context.Context, project, namespace, phase string) ([]clients.PlanSummary, error)
	ListSlices(ctx context.Context, planID string) ([]clients.PlanSliceSummary, error)
	// GetSlice recovers a slice's full detail (incl. the `files` array the
	// tabular LIST view omits) so the emitter can stamp a real scope.
	GetSlice(ctx context.Context, sliceID string) (clients.PlanSliceSummary, error)
}

const (
	planSliceCreatedBy    = "mills:plan-slice-emitter"
	planSliceDefaultLabel = "mills-from-plan-slice"
	// planSliceFabricatedLabel marks an emitted item whose slice declared ONLY
	// files absent from the target repo at emit time — the fabricated-slice
	// signature (the plan cites files as if they exist; none do). The slice
	// itself carries the machine-readable stamp (store.Slice.Fabricated +
	// MissingFiles + GroundingRevision, consumed by the fabricated_slice
	// gate); the label makes the suspect visible in the HUD and backlog
	// queries without deserializing slices.
	planSliceFabricatedLabel = "mills-fabricated-suspect"
	planSliceDefaultPhase    = "pending"
	planSliceDefaultPriority = store.P2
	planSliceDefaultPoll     = 5 * time.Minute
	planSliceDefaultTimeout  = 2 * time.Minute
	// planSliceIDPrefix namespaces emitter-produced BacklogItem ids so they
	// can't collide with the gitlab importer ("gl-") or canaries. The full
	// shape is "psl-<sanitized slice_id>".
	planSliceIDPrefix = "psl-"
)

// emitterReadyPlanPhases are the plan lifecycle phases whose pending slices
// are eligible to emit. Draft plans are still being shaped; merged/done are
// finished. Only planned + in_progress represent "ready to implement". Keep
// this order stable so a malformed duplicate plan is resolved deterministically.
var emitterReadyPlanPhases = []string{"planned", "in_progress"}

func isEmitterReadyPlanPhase(phase string) bool {
	for _, readyPhase := range emitterReadyPlanPhases {
		if strings.EqualFold(strings.TrimSpace(phase), readyPhase) {
			return true
		}
	}
	return false
}

// PlanSliceEmitterConfig captures the operator-tunable knobs. Defaults apply
// when fields are zero.
type PlanSliceEmitterConfig struct {
	Project   string
	Namespace string
	// DemandProjects is the allowlist of NON-home repos to ALSO source demand
	// from (S6 multi-repo). Items emitted for these projects carry the project
	// path as their TargetProject so the pipeline routes the run cross-repo.
	// Empty = home-only (pre-S6 behavior). The operator only populates this
	// when cross_repo execution is enabled (Policy.CrossRepoDemandProjects),
	// and the reconciler's fail-closed gate still guards execution.
	DemandProjects []string
	// DynamicDemandProjects, when non-nil, is resolved on EVERY tick and
	// unioned (deduped) with DemandProjects — the runtime half of demand
	// sourcing, backed by the bootstrapped_projects registry. The operator
	// wires it through a closure that fails closed on policy
	// (cross_repo.enabled AND cross_repo.allow_bootstrapped), so hot policy
	// reloads take effect without re-constructing the emitter. Errors are the
	// provider's to log; it returns nil when disabled or unavailable.
	DynamicDemandProjects func(ctx context.Context) []string
	ReadyPhase            string
	Label                 string
	Priority              store.Priority
	PollInterval          time.Duration
	// TickTimeout bounds one complete home + dynamic-demand scan. Without it,
	// a stalled MCP hub read wedges the emitter before its ticker can advance.
	TickTimeout time.Duration
}

func (c *PlanSliceEmitterConfig) applyDefaults() {
	if c.ReadyPhase == "" {
		c.ReadyPhase = planSliceDefaultPhase
	}
	if c.Label == "" {
		c.Label = planSliceDefaultLabel
	}
	if c.Priority == "" {
		c.Priority = planSliceDefaultPriority
	}
	if c.PollInterval <= 0 {
		c.PollInterval = planSliceDefaultPoll
	}
	if c.TickTimeout <= 0 {
		c.TickTimeout = planSliceDefaultTimeout
	}
}

// PlanSliceEmitter polls the Plan Store for ready slices and emits one
// plan-linked BacklogItem per slice. It is idempotent on re-run: emits are
// keyed by a deterministic id derived from the slice_id and skipped if the
// backlog already has that id, so the reconciler's state transitions are
// never clobbered (mirrors the GitLab importer's read-then-insert dedup).
type PlanSliceEmitter struct {
	plans     PlanReader
	store     BacklogStore
	cfg       PlanSliceEmitterConfig
	logger    *slog.Logger
	protected func([]string) []string
	grounder  func(ctx context.Context, project string, files []string) (missing []string, revision string, ok bool)
	// Enabled is the live global admission barrier. Nil preserves existing
	// standalone/test behavior.
	Enabled func() bool
	active  atomic.Int64
}

// NewPlanSliceEmitter wires a reader + store + config. Defaults are applied
// to zero fields.
func NewPlanSliceEmitter(plans PlanReader, st BacklogStore, cfg PlanSliceEmitterConfig, logger *slog.Logger) *PlanSliceEmitter {
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &PlanSliceEmitter{plans: plans, store: st, cfg: cfg, logger: logger}
}

// SetProtectedPathHitter installs the hook that pre-declares a slice's
// protected-path touches on the emitted item. hit returns the subset of the
// given repo-relative paths matching the active policy's protected_paths globs
// (e.g. func(p) { return pm.Current().ProtectedPathsHit(p) }); the emitter
// records them in item.Policy.ProtectedPathsTouched so the post-implement
// path_policy gate treats the plan-declared protected touch as intended rather
// than escalating it. UNDECLARED protected touches — a path the implement stage
// hit that the slice never declared — are not pre-declared, so the gate still
// fires on them. nil-safe: without a hitter the emitter behaves as before.
func (e *PlanSliceEmitter) SetProtectedPathHitter(hit func([]string) []string) {
	if e != nil {
		e.protected = hit
	}
}

// SetSliceGrounder installs the hook that verifies a slice's declared files
// against the target repo's tree at emit time (e.g. a
// clients.RepoTreeGrounder bound to the operator-local clone). ground reports
// the subset of the given repo-relative paths ABSENT at the revision it
// consulted; ok=false means the check could not run (foreign project, no
// clone) and the slice emits ungrounded. The emitter stamps the result on the
// emitted item's slice — a missing file is a legitimate planned new file, but
// a slice whose EVERY concrete declared file is missing carries the
// fabricated-slice signature and is flagged Fabricated for the
// fabricated_slice gate to escalate terminally on an all-new implement diff.
// nil-safe: without a grounder the emitter behaves as before.
func (e *PlanSliceEmitter) SetSliceGrounder(ground func(ctx context.Context, project string, files []string) ([]string, string, bool)) {
	if e != nil {
		e.grounder = ground
	}
}

// Run drives Tick on the configured PollInterval until ctx is done. A single
// tick's error is logged and the loop continues; a hard abort only happens
// when ctx is canceled.
func (e *PlanSliceEmitter) Run(ctx context.Context) error {
	e.logger.Info("plan-slice emitter started",
		"project", e.cfg.Project, "namespace", e.cfg.Namespace,
		"demand_projects", e.cfg.DemandProjects,
		"ready_phase", e.cfg.ReadyPhase, "poll_interval", e.cfg.PollInterval,
		"tick_timeout", e.cfg.TickTimeout)
	// Tick once immediately so a freshly-deployed operator drains any
	// already-ready slices without waiting one interval.
	_, _ = e.runTick(ctx, "initial")
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _ = e.runTick(ctx, "periodic")
		}
	}
}

// runTick gives every production emit pass a hard deadline and records enough
// telemetry to distinguish an idle namespace (ok, emitted=0) from a failed or
// wedged MCP dependency. Direct Tick calls remain useful for focused tests.
func (e *PlanSliceEmitter) runTick(ctx context.Context, kind string) (int, error) {
	if e.Enabled != nil && !e.Enabled() {
		return 0, nil
	}
	tickCtx, cancel := context.WithTimeout(ctx, e.cfg.TickTimeout)
	defer cancel()

	started := time.Now()
	emitted, err := e.Tick(tickCtx)
	elapsed := time.Since(started)
	mills.PlanSliceEmitterTickDurationSeconds.Observe(elapsed.Seconds())
	mills.PlanSliceEmitterItemsEmittedTotal.Add(float64(emitted))

	outcome := "ok"
	switch {
	case tickCtx.Err() == context.DeadlineExceeded:
		outcome = "timeout"
		e.logger.Warn("plan-slice emitter tick timed out",
			"kind", kind, "timeout", e.cfg.TickTimeout,
			"elapsed", elapsed.Round(time.Millisecond), "emitted", emitted, "err", err)
	case ctx.Err() != nil:
		outcome = "cancelled"
		e.logger.Info("plan-slice emitter tick cancelled",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond))
	case err != nil:
		outcome = "error"
		e.logger.Warn("plan-slice emitter tick failed",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond), "emitted", emitted, "err", err)
	default:
		e.logger.Info("plan-slice emitter tick complete",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond), "emitted", emitted)
	}
	mills.PlanSliceEmitterTicksTotal.WithLabelValues(outcome).Inc()
	return emitted, err
}

// Tick performs one emit pass and returns the number of newly-created backlog
// items. Safe to call from tests; the Run loop just drives this on a ticker.
func (e *PlanSliceEmitter) Tick(ctx context.Context) (int, error) {
	e.active.Add(1)
	defer e.active.Add(-1)
	if e.Enabled != nil && !e.Enabled() {
		return 0, nil
	}
	// FAIL-CLOSED: never scan the whole project without an explicit namespace
	// gate — that would scoop up arbitrary planning-scaffold slices.
	if strings.TrimSpace(e.cfg.Namespace) == "" {
		return 0, nil
	}
	// Home repo: emitted items carry no TargetProject, so the pipeline runs
	// against the operator's home repo (the pre-S6 path). A home list error is
	// hard — it's the critical demand source.
	emitted, err := e.emitForProject(ctx, e.cfg.Project, "")
	if err != nil {
		return emitted, err
	}
	// S6 multi-repo demand: source each allowlisted foreign repo and stamp its
	// path as the emitted item's TargetProject so the reconciler routes the run
	// cross-repo (still gated by CrossRepoPolicy.Enabled). A foreign project
	// that resolves to the home repo is skipped to avoid double-emitting home
	// slices with a spurious target. A per-repo error is logged and the loop
	// continues so one bad repo can't stall the rest.
	seen := make(map[string]bool)
	demand := append([]string(nil), e.cfg.DemandProjects...)
	// Runtime-bootstrapped repos join the same demand pass, resolved per tick
	// so a project minted minutes ago dispatches without an operator restart.
	// The provider fails closed on policy, so nil here means disabled or none.
	if e.cfg.DynamicDemandProjects != nil {
		demand = append(demand, e.cfg.DynamicDemandProjects(ctx)...)
	}
	for _, proj := range demand {
		proj = strings.TrimSpace(proj)
		if proj == "" || seen[proj] || store.SameRepo(proj, e.cfg.Project) {
			continue
		}
		seen[proj] = true
		n, ferr := e.emitForProject(ctx, proj, proj)
		emitted += n
		if ferr != nil {
			e.logger.Warn("plan-slice emitter cross-repo demand tick failed",
				"target_project", proj, "err", ferr)
		}
	}
	return emitted, nil
}

// ActiveOperations reports plan-emission passes currently executing.
func (e *PlanSliceEmitter) ActiveOperations() int64 {
	if e == nil {
		return 0
	}
	return e.active.Load()
}

// emitForProject scans one project's ready plans in the configured namespace
// and emits a queued, plan-linked BacklogItem per ready slice. targetProject is
// stamped on every emitted item's TargetProject: empty for the operator's home
// repo (single-repo behavior), or the foreign project path for S6 cross-repo
// demand. Returns the number of newly-created items.
func (e *PlanSliceEmitter) emitForProject(ctx context.Context, project, targetProject string) (int, error) {
	plans, err := e.listReadyPlans(ctx, project)
	if err != nil {
		return 0, err
	}

	emitted := 0
	for _, pl := range plans {
		slices, serr := e.plans.ListSlices(ctx, pl.ID)
		if serr != nil {
			e.logger.Warn("plan-slice emitter list slices failed", "plan_id", pl.ID, "err", serr)
			continue
		}
		for _, sl := range slices {
			if !strings.EqualFold(strings.TrimSpace(sl.Phase), e.cfg.ReadyPhase) {
				continue
			}
			// Existence check FIRST, on the deterministic id alone: if the
			// reconciler/council has already touched this item, leave its
			// state alone (read-then-insert dedup) and skip the detail fetch +
			// grounding work below — they only feed item creation, and running
			// grounding for already-emitted slices every tick would double-
			// count its verdict metric.
			existing, getErr := e.store.Get(ctx, planSliceBacklogID(sl.ID))
			if getErr == nil && existing != nil {
				// Beam resync: plan priority is live steering. If the plan's
				// priority changed after emission and the item is still
				// queued, propagate the new bucket so the dispatcher's
				// priority-ordered pickup reflects the reorder. Any other
				// state means the item is in flight or terminal; priority is
				// inert there and the item is left untouched.
				if p := itemPriority(pl, e.cfg); existing.State == store.BacklogQueued && existing.Priority != p {
					prev := existing.Priority
					existing.Priority = p
					if perr := e.store.Put(ctx, existing); perr != nil {
						e.logger.Warn("plan-slice emitter priority resync failed",
							"id", existing.ID, "err", perr)
					} else {
						e.logger.Info("plan-slice emitter resynced priority",
							"id", existing.ID, "from", prev, "to", existing.Priority)
					}
				}
				continue
			}
			if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
				e.logger.Warn("plan-slice emitter get failed", "id", planSliceBacklogID(sl.ID), "err", getErr)
				continue
			}
			// The LIST view's TOON tabular form omits the `files` array, so a
			// ready slice always arrives here with empty Files. Recover the
			// declared file scope from the detail view; without it the emitted
			// item is slice-less and reaches the `scope` gate with nothing to
			// enforce ("no slices; no scope to enforce"). Best-effort: on error
			// or a genuinely file-less slice, fall through — the pipeline's
			// post-plan_slice hydration (pipeline.Runner.SliceHydrator) gets a
			// second chance to recover scope, and the scope gate records an
			// advisory skip if nothing materializes (escalation #332 was this
			// exact shape back when the gate still failed terminally).
			if len(sl.Files) == 0 {
				if full, gerr := e.plans.GetSlice(ctx, sl.ID); gerr != nil {
					e.logger.Warn("plan-slice emitter slice detail fetch failed; item may fail scope",
						"slice_id", sl.ID, "err", gerr)
				} else if len(full.Files) > 0 {
					sl.Files = full.Files
				}
			}
			// Pre-declare the protected paths the slice's declared files touch
			// (e.g. **/*auth*.go) so the path_policy gate treats the planned
			// touch as intended; an UNDECLARED protected touch the implement
			// stage introduces still fails the gate. nil hitter => no
			// pre-declaration (prior behavior).
			var protectedTouched []string
			if e.protected != nil && len(sl.Files) > 0 {
				protectedTouched = e.protected(sl.Files)
			}
			// Ground the declared files against the target repo's tree so a
			// fabricated slice (every declared file absent) is flagged BEFORE
			// an implement run spends a spawn inventing files to satisfy it.
			grounded := e.groundSliceFiles(ctx, project, sl.Files)
			item := sliceToBacklog(pl, sl, e.cfg, protectedTouched, grounded)
			// S6: route the run to the demand repo. Empty targetProject leaves
			// TargetProject unset (home repo); a foreign path routes cross-repo.
			if targetProject != "" {
				item.TargetProject = targetProject
			}
			if err := e.store.Put(ctx, item); err != nil {
				e.logger.Warn("plan-slice emitter put failed",
					"id", item.ID, "slice_id", sl.ID, "err", err)
				continue
			}
			emitted++
			if grounded != nil && grounded.Fabricated {
				e.logger.Warn("plan-slice emitter flagged fabricated slice: every declared file is absent from the repo",
					"id", item.ID, "plan_id", pl.ID, "slice_id", sl.ID,
					"grounding_revision", grounded.Revision, "missing_files", grounded.Missing)
			}
			e.logger.Info("plan-slice emitter created backlog item",
				"id", item.ID, "plan_id", pl.ID, "slice_id", sl.ID,
				"title", item.Title, "priority", item.Priority)
		}
	}
	return emitted, nil
}

// listReadyPlans fetches every eligible lifecycle bucket before any backlog
// writes. This preserves the emitter's fail-closed read behavior while keeping
// terminal plan bodies out of the MCP response that production must decode.
func (e *PlanSliceEmitter) listReadyPlans(ctx context.Context, project string) ([]clients.PlanSummary, error) {
	seenPlanIDs := make(map[string]struct{})
	var ready []clients.PlanSummary
	for _, phase := range emitterReadyPlanPhases {
		plans, err := e.plans.ListPlans(ctx, project, e.cfg.Namespace, phase)
		if err != nil {
			return nil, fmt.Errorf("list %s plans (%s): %w", phase, project, err)
		}
		for _, pl := range plans {
			// Keep a local guard in case a reader ignores its phase filter.
			if !isEmitterReadyPlanPhase(pl.Phase) {
				continue
			}
			planID := strings.TrimSpace(pl.ID)
			if planID != "" {
				if _, seen := seenPlanIDs[planID]; seen {
					continue
				}
				seenPlanIDs[planID] = struct{}{}
			}
			ready = append(ready, pl)
		}
	}
	return ready, nil
}

// sliceGrounding is the emit-time existence verdict for a slice's declared
// files, stamped onto the emitted item's slice for the fabricated_slice gate.
type sliceGrounding struct {
	// Missing is the subset of the slice's concrete declared paths absent
	// from the target repo's tree at Revision.
	Missing []string
	// Revision is the git revision the check ran against.
	Revision string
	// Fabricated is true when EVERY concrete declared path was missing — the
	// fabricated-slice signature (the plan cites files as if they exist).
	Fabricated bool
}

// groundSliceFiles runs the grounding hook over the slice's concrete declared
// paths. Returns nil — the slice emits ungrounded, prior behavior — when no
// grounder is installed, the slice declares no concrete paths (globs ground
// per-directory at the scope gate, not here), or the hook cannot answer for
// this project (foreign repo, clone unavailable).
func (e *PlanSliceEmitter) groundSliceFiles(ctx context.Context, project string, files []string) *sliceGrounding {
	if e.grounder == nil {
		return nil
	}
	concrete := make([]string, 0, len(files))
	for _, f := range files {
		clean := strings.TrimPrefix(strings.TrimSpace(f), "./")
		if clean == "" || strings.ContainsAny(clean, "*?[") {
			continue
		}
		concrete = append(concrete, clean)
	}
	if len(concrete) == 0 {
		return nil
	}
	missing, revision, ok := e.grounder(ctx, project, concrete)
	if !ok {
		mills.PlanSliceEmitterSliceGroundingTotal.WithLabelValues("ungroundable").Inc()
		return nil
	}
	gr := &sliceGrounding{Missing: missing, Revision: revision}
	switch {
	case len(missing) == 0:
		mills.PlanSliceEmitterSliceGroundingTotal.WithLabelValues("grounded").Inc()
	case len(missing) >= len(concrete):
		gr.Fabricated = true
		mills.PlanSliceEmitterSliceGroundingTotal.WithLabelValues("fabricated").Inc()
	default:
		mills.PlanSliceEmitterSliceGroundingTotal.WithLabelValues("partial").Inc()
	}
	return gr
}

// sliceToBacklog maps a ready plan slice into a queued, plan-linked
// BacklogItem. PlanID carries the convergence link so the spawned agent
// resolves the live plan; the deterministic id keys the dedup.
func sliceToBacklog(pl clients.PlanSummary, sl clients.PlanSliceSummary, cfg PlanSliceEmitterConfig, protectedTouched []string, grounded *sliceGrounding) *store.BacklogItem {
	title := strings.TrimSpace(sl.Name)
	if t := strings.TrimSpace(pl.Title); t != "" {
		if title != "" {
			title = t + " — " + title
		} else {
			title = t
		}
	}
	planID := strings.TrimSpace(sl.PlanID)
	if planID == "" {
		planID = strings.TrimSpace(pl.ID)
	}
	// Stamp the slice's declared files as a single-slice scope so the emitted
	// item carries scope the pipeline's `scope` gate can enforce. An emitted
	// item with no slices fails the gate ("backlog item has no slices; no
	// scope to enforce") on every implement attempt and escalates with an
	// empty diff — the same cascade sliceless council items hit. Empty Files
	// (the slice declared none) leaves Slices nil; the gate still fails closed
	// then, which is the correct signal that the slice was under-specified.
	var slices []store.Slice
	if name := strings.TrimSpace(sl.Name); name != "" && len(sl.Files) > 0 {
		s := store.Slice{
			Name:  name,
			Files: append([]string(nil), sl.Files...),
		}
		// Emit-time grounding stamp: which declared files were absent, at
		// which revision, and whether ALL of them were (the fabricated-slice
		// signature). The fabricated_slice gate reads this to make an all-new
		// implement diff a terminal escalation instead of a retry burn.
		if grounded != nil {
			s.MissingFiles = append([]string(nil), grounded.Missing...)
			s.GroundingRevision = grounded.Revision
			s.Fabricated = grounded.Fabricated
		}
		slices = []store.Slice{s}
	}
	labels := []string{cfg.Label}
	if grounded != nil && grounded.Fabricated {
		labels = append(labels, planSliceFabricatedLabel)
	}
	item := &store.BacklogItem{
		ID:        planSliceBacklogID(sl.ID),
		Title:     title,
		Labels:    labels,
		State:     store.BacklogQueued,
		Priority:  itemPriority(pl, cfg),
		SpecDoc:   buildSliceSpec(sl),
		Slices:    slices,
		PlanID:    planID,
		CreatedBy: planSliceCreatedBy,
	}
	if len(protectedTouched) > 0 {
		item.Policy.ProtectedPathsTouched = append([]string(nil), protectedTouched...)
	}
	return item
}

// itemPriority resolves an emitted item's priority bucket: the plan's own
// warp-beam priority wins (that's the steering junction — the dispatcher
// orders queued items priority ASC, so a P0 plan's slices dispatch first),
// falling back to the emitter's configured default when the plan doesn't
// declare one or declares an unknown bucket.
func itemPriority(pl clients.PlanSummary, cfg PlanSliceEmitterConfig) store.Priority {
	switch p := store.Priority(strings.ToUpper(strings.TrimSpace(pl.Priority))); p {
	case store.P0, store.P1, store.P2, store.P3:
		return p
	}
	return cfg.Priority
}

// BacklogIDForSlice exposes the deterministic slice→backlog id derivation so
// downstream consumers (the take-up reconciler) resolve the emitted item for
// a slice without re-implementing the sanitization.
func BacklogIDForSlice(sliceID string) string { return planSliceBacklogID(sliceID) }

// planSliceBacklogID derives a deterministic, store-safe backlog id from a
// slice_id so a re-emit upserts against the same id rather than duplicating.
func planSliceBacklogID(sliceID string) string {
	slug := strings.ToLower(strings.TrimSpace(sliceID))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "slice"
	}
	return planSliceIDPrefix + slug
}

// buildSliceSpec composes the slice's goal + acceptance criteria into an
// actionable SpecDoc for the pipeline's implement stage. The live plan +
// full slice detail (files, etc.) remain resolvable via the item's PlanID.
func buildSliceSpec(sl clients.PlanSliceSummary) string {
	var b strings.Builder
	if g := strings.TrimSpace(sl.Goal); g != "" {
		b.WriteString(g)
	}
	if ac := strings.TrimSpace(sl.AcceptanceCriteria); ac != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## Acceptance criteria\n")
		b.WriteString(ac)
	}
	return b.String()
}
