package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/budget"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	tastePlanGrades           = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_taste_plan_grades", Help: "Merged backlog taste grades by plan and grade."}, []string{"plan_id", "grade"})
	tastePlanRegretRate       = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_taste_plan_regret_rate", Help: "Regret share among graded merged backlog items by plan."}, []string{"plan_id"})
	tastePlanGradeCoverage    = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_taste_plan_grade_coverage", Help: "Graded share of merged backlog items by plan."}, []string{"plan_id"})
	tasteOverallGradeCoverage = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_taste_overall_grade_coverage", Help: "Overall graded share of merged backlog items by rolling window."}, []string{"window"})
)

func (o *operator) handleTasteAggregates(w http.ResponseWriter, r *http.Request) {
	agg, err := o.store.Backlog.TasteAggregates(r.Context(), time.Now().UTC(), 14*24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setTasteAggregateMetrics(agg)
	writeJSON(w, http.StatusOK, agg)
}

func setTasteAggregateMetrics(agg store.TasteAggregates) {
	tastePlanGrades.Reset()
	tastePlanRegretRate.Reset()
	tastePlanGradeCoverage.Reset()
	for _, plan := range agg.Plans {
		tastePlanGrades.WithLabelValues(plan.PlanID, "keep").Set(float64(plan.Keep))
		tastePlanGrades.WithLabelValues(plan.PlanID, "meh").Set(float64(plan.Meh))
		tastePlanGrades.WithLabelValues(plan.PlanID, "regret").Set(float64(plan.Regret))
		tastePlanRegretRate.WithLabelValues(plan.PlanID).Set(plan.RegretRate)
		tastePlanGradeCoverage.WithLabelValues(plan.PlanID).Set(plan.GradeCoverage)
	}
	tasteOverallGradeCoverage.WithLabelValues("14d").Set(agg.OverallCoverage14d)
}

// backlogPlanReader is the subset of the plan client the backlog intake needs
// to materialize a plan-linked item's slice scope at enqueue time.
type backlogPlanReader interface {
	ListSlices(ctx context.Context, planID string) ([]clients.PlanSliceSummary, error)
	GetSlice(ctx context.Context, sliceID string) (clients.PlanSliceSummary, error)
}

// canaryDedupeLabel is the label that marks a backlog item as a Mills
// canary heartbeat. Items carrying this label are subject to the
// 24h dedupe window enforced by handleBacklogCreate.
const canaryDedupeLabel = "mills-canary"

// canaryDedupeWindow is the lookback used to detect a still-in-flight
// canary. Kept conservative (24h) because canaries should reach merge
// inside a few hours; anything older is a sign of an outright wedge,
// not a duplicate enqueue.
const canaryDedupeWindow = 24 * time.Hour

// handleBacklogList returns every backlog item, newest-first by updated_at.
// Pagination is intentionally absent for v1 — operator humans will browse
// O(100) items, not O(10k); when scale demands it we add ?limit/?offset
// without breaking the existing shape.
func (o *operator) handleBacklogList(w http.ResponseWriter, r *http.Request) {
	items, err := o.store.Backlog.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Encode an empty backlog (and every inner array) as `[]`, not `null`.
	// BacklogItem's array fields are untagged (PascalCase keys, no omitempty)
	// so a nil slice serializes as `null` — which forces every client to
	// special-case it and crashed the HUD warp view on an empty beam. Coerce
	// server-side at the encode boundary; the DAO scans a fresh item per
	// request so in-place mutation is safe.
	writeJSON(w, http.StatusOK, coerceBacklogList(items))
}

// coerceBacklogList rewrites a nil top-level list and each item's nil inner
// arrays to empty slices so the JSON encoder never emits `null` for the
// backlog wire contract. Returns the (same, possibly reallocated) slice for
// call-site clarity.
func coerceBacklogList(items []*store.BacklogItem) []*store.BacklogItem {
	if items == nil {
		items = []*store.BacklogItem{}
	}
	for _, it := range items {
		coerceBacklogArrays(it)
	}
	return items
}

// coerceBacklogArrays sets a backlog item's nil non-omitempty array fields to
// empty slices in place. Only the fields that would otherwise serialize as
// `null` are touched: BacklogItem.Labels/Dependencies/Slices and, per slice,
// Files/Tests. Fields tagged `omitempty` (Slice.ParallelWith,
// SuccessCriteria.*, ItemPolicy.ProtectedPathsTouched) are intentionally left
// alone — they are omitted, never `null`, so coercing them would only add
// noise to the wire shape.
func coerceBacklogArrays(item *store.BacklogItem) {
	if item == nil {
		return
	}
	if item.Labels == nil {
		item.Labels = []string{}
	}
	if item.Dependencies == nil {
		item.Dependencies = []string{}
	}
	if item.Slices == nil {
		item.Slices = []store.Slice{}
	}
	for i := range item.Slices {
		if item.Slices[i].Files == nil {
			item.Slices[i].Files = []string{}
		}
		if item.Slices[i].Tests == nil {
			item.Slices[i].Tests = []string{}
		}
	}
}

// handleBacklogCreate accepts a JSON BacklogItem body and inserts or
// compare-and-swap updates it in the canonical store. Updates must echo the
// Revision returned by a prior read; a stale revision returns 409 instead of
// overwriting a newer lifecycle or admission decision. Required fields: id,
// title. Defaults applied when unset: state=queued, priority=P3,
// created_by="api". Always returns the persisted item (so callers can see
// normalized timestamps and the next revision).
//
// Until slice 3.x lands the council-driven backlog mutator + GitLab sync,
// this endpoint is the only mutation path — used by smoke tests, manual
// queue insertions, and any external automation that wants to feed the
// mills without going through the GitLab issue → sync flow.
func (o *operator) handleBacklogCreate(w http.ResponseWriter, r *http.Request) {
	var item store.BacklogItem
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&item); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(item.Title) == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if item.State == "" {
		item.State = store.BacklogQueued
	}
	if item.Priority == "" {
		item.Priority = store.P3
	}
	if item.CreatedBy == "" {
		item.CreatedBy = "api"
	}

	// Canary dedupe: refuse a new mills-canary enqueue when an earlier
	// one is still in flight within the 24h window. External automation
	// (e.g. cron-driven heartbeats) was creating dozens of escalated
	// duplicates per day; this guard turns those into idempotent retries.
	// The `force=1` query bypass exists for operators who genuinely
	// want a second canary in the same window — typically because they
	// just unwedged the previous one.
	if itemHasLabel(&item, canaryDedupeLabel) && !backlogForceRequested(r) {
		existing, derr := findRecentCanary(r.Context(), o.store, canaryDedupeWindow, item.ID)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusInternalServerError)
			return
		}
		if existing != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":          "canary-deduped",
				"existing_id":    existing.ID,
				"existing_state": string(existing.State),
				"window":         canaryDedupeWindow.String(),
			})
			return
		}
	}

	// Plan-linked items should arrive with an enforceable slice scope: the
	// post-implement scope gate reads item.Slices directly and has nothing
	// to enforce on a slice-less (or file-less) item — it records an
	// advisory skip and the diff runs unscoped. Intakes that cross the MCP hub
	// (the HUD pattern-stamp projection) lose slice `files` in transit — the
	// hub's TOON tabular encoding drops array columns (same gotcha the
	// plan-slice emitter works around via GetSlice) — so the live 2026-07-01
	// widget/gadget stamp runs escalated with "no slices; no scope to
	// enforce" despite a clean implement. Hydrate from the plan store here,
	// at the single choke point every REST enqueue passes through.
	o.hydratePlanSliceScope(r.Context(), &item)

	if err := o.store.Backlog.Put(r.Context(), &item); err != nil {
		if errors.Is(err, store.ErrStaleWrite) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":   "stale-backlog-write",
				"message": err.Error(),
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	persisted, err := o.store.Backlog.Get(r.Context(), item.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	coerceBacklogArrays(persisted)
	writeJSON(w, http.StatusCreated, persisted)
}

// handleBacklogGet returns one backlog item.
func (o *operator) handleBacklogGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	item, err := o.store.Backlog.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "backlog item not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	coerceBacklogArrays(item)
	writeJSON(w, http.StatusOK, item)
}

// handleBacklogSync triggers the canonical-store ↔ GitLab sync. Implementation
// lands in slice 3.x (council backlog mutator); the auth gate is locked
// in here.
func (o *operator) handleBacklogSync(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "3.x backlog mutator + GitLab sync")
}

// handleCostPreview returns a Phase 7 slice 7.3 cost preview for one
// backlog item. Read-only: no admin token required. Required query
// param: ?backlog_id=. Responses:
//   - 200 + CostEstimate JSON on the happy path
//   - 400 when backlog_id is missing
//   - 404 when the backlog id is unknown
//   - 503 when the policy manager isn't configured (operator boot race)
//
// The estimator is constructed per-request because it's just two pointer
// wires; profiling didn't justify caching it on the operator struct.
func (o *operator) handleCostPreview(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("backlog_id"))
	if id == "" {
		http.Error(w, "backlog_id is required", http.StatusBadRequest)
		return
	}
	if o.policy == nil {
		http.Error(w, "policy manager not ready", http.StatusServiceUnavailable)
		return
	}
	est := &budget.Estimator{
		Store:      o.store,
		PolicyFunc: o.policy.Current,
	}
	preview, err := est.Preview(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "backlog item not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Echo a small marker on the body so HUD callers can distinguish
	// preview vs. live spend in their own logging without re-parsing.
	type previewEnvelope struct {
		*budget.CostEstimate
		Source string `json:"source"`
	}
	writeJSON(w, http.StatusOK, previewEnvelope{
		CostEstimate: preview,
		Source:       "estimator/v1",
	})
}

// itemHasLabel returns true if the backlog item carries the given label.
func itemHasLabel(item *store.BacklogItem, label string) bool {
	if item == nil {
		return false
	}
	for _, l := range item.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// backlogForceRequested returns true if the caller passed ?force=1 (or
// any truthy synonym). Used to bypass the canary dedupe guard.
func backlogForceRequested(r *http.Request) bool {
	return truthyQuery(r, "force")
}

// truthyQuery reports whether the named query param carries a truthy value
// (1/true/yes/on, case-insensitive).
func truthyQuery(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// findRecentCanary scans the backlog for a non-merged mills-canary item
// that should block a new canary enqueue. Returns the oldest match (i.e.
// the item still wedging the queue) or nil if none. `selfID` excludes
// the item being upserted right now, which matters because Put is an
// upsert — re-posting a canary by ID must not be treated as a duplicate
// of itself.
//
// Escalated canaries are special-cased: they bypass the `window` and
// always block, because escalation means "human must act before the
// next canary makes sense." Without this carve-out, a stuck escalation
// from >window ago lets a new canary slip through every cycle, which
// is exactly how the backlog accumulated 30+ identical
// MILLS-CANARY-* / escalated rows visible in the HUD Backlog tab.
// Other in-flight states (queued, running, paused) continue to use
// the time window — those represent transient progress, not a wedge.
func findRecentCanary(ctx context.Context, st *store.Store, window time.Duration, selfID string) (*store.BacklogItem, error) {
	if st == nil {
		return nil, nil
	}
	items, err := st.Backlog.List(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().Add(-window)
	var oldest *store.BacklogItem
	for _, it := range items {
		if it == nil || it.ID == selfID {
			continue
		}
		if !itemHasLabel(it, canaryDedupeLabel) {
			continue
		}
		if it.State == store.BacklogMerged {
			continue
		}
		// Escalated canaries block forever — see comment above. Other
		// non-merged states use the configured window.
		if it.State != store.BacklogEscalated && it.CreatedAt.Before(cutoff) {
			continue
		}
		if oldest == nil || it.CreatedAt.Before(oldest.CreatedAt) {
			oldest = it
		}
	}
	return oldest, nil
}

// hydratePlanSliceScope materializes slice scope (names + files) onto a
// plan-linked backlog item that arrived without one, reading the canonical
// slice detail from the plan store. Mirrors the plan-slice emitter's recovery
// path: the slice LIST view's TOON tabular form omits the `files` array, so
// each file-less slice is re-fetched via GetSlice. Also pre-declares any
// protected paths the hydrated files touch so the path_policy gate treats a
// plan-declared touch as intended (undeclared touches introduced by the
// implement stage still fail the gate).
//
// Best-effort by design: on a nil reader, a fetch error, or a plan whose
// slices genuinely declare no files, the item is left as-is — the pipeline's
// post-plan_slice hydration (pipeline.Runner.SliceHydrator) re-attempts scope
// recovery after the plan_slice stage, and the scope gate records an advisory
// skip if nothing materializes.
func (o *operator) hydratePlanSliceScope(ctx context.Context, item *store.BacklogItem) {
	if o.planReader == nil || item == nil || strings.TrimSpace(item.PlanID) == "" {
		return
	}
	for _, s := range item.Slices {
		if len(s.Files) > 0 {
			return // scope already materialized by the intake
		}
	}
	slices, err := o.planReader.ListSlices(ctx, item.PlanID)
	if err != nil {
		o.logger.Warn("backlog intake: plan slice hydration failed; item may fail the scope gate",
			"id", item.ID, "plan_id", item.PlanID, "err", err)
		return
	}
	var (
		out      []store.Slice
		allFiles []string
	)
	for _, sl := range slices {
		files := sl.Files
		if len(files) == 0 {
			full, gerr := o.planReader.GetSlice(ctx, sl.ID)
			if gerr != nil {
				o.logger.Warn("backlog intake: plan slice detail fetch failed",
					"slice_id", sl.ID, "err", gerr)
				continue
			}
			files = full.Files
		}
		name := strings.TrimSpace(sl.Name)
		if name == "" || len(files) == 0 {
			continue
		}
		out = append(out, store.Slice{Name: name, Files: append([]string(nil), files...)})
		allFiles = append(allFiles, files...)
	}
	if len(out) == 0 {
		return
	}
	item.Slices = out
	if o.policy != nil && len(item.Policy.ProtectedPathsTouched) == 0 {
		if hit := o.policy.Current().ProtectedPathsHit(allFiles); len(hit) > 0 {
			item.Policy.ProtectedPathsTouched = append([]string(nil), hit...)
		}
	}
	o.logger.Info("backlog intake: hydrated plan-slice scope",
		"id", item.ID, "plan_id", item.PlanID, "slices", len(out), "files", len(allFiles))
}
