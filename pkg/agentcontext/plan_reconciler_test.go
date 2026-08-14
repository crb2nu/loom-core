package agentcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// fakeHUD serves GET /api/agent/mr-status from a branch -> merge_requests JSON
// map. A branch absent from the map answers with an empty registry (count 0),
// which is exactly what the real HUD returns once an MR merges.
type fakeHUD struct {
	mu       sync.Mutex
	byBranch map[string]string
	stale    bool
	queries  []string
}

func newFakeHUD(t *testing.T, byBranch map[string]string) (*fakeHUD, *httptest.Server) {
	t.Helper()
	h := &fakeHUD{byBranch: byBranch}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/mr-status" {
			http.NotFound(w, r)
			return
		}
		branch := r.URL.Query().Get("branch")
		h.mu.Lock()
		h.queries = append(h.queries, branch)
		mrs := h.byBranch[branch]
		stale := h.stale
		h.mu.Unlock()
		if mrs == "" {
			mrs = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"branch":"` + branch + `","merge_requests":` + mrs +
			`,"stale":` + boolLiteral(stale) + `}`))
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func boolLiteral(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (h *fakeHUD) queriedBranches() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.queries...)
}

// newTestPlanReconciler wires a sweep over a backend-less PlanSvc (the plan
// package's cache-path convention) against a fake HUD.
func newTestPlanReconciler(ps *PlanSvc, hudURL string) *PlanReconciler {
	cfg := DefaultPlanReconcilerConfig()
	cfg.CheckInterval = time.Hour // the loop is never started in these tests
	return NewPlanReconciler(cfg, ps, hudURL, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedPlan writes a plan and its slices straight through the service cache so a
// test controls phases exactly.
func seedPlan(t *testing.T, ps *PlanSvc, plan *Plan, slices ...*PlanSlice) {
	t.Helper()
	ctx := context.Background()
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = time.Now().UTC()
		plan.UpdatedAt = plan.CreatedAt
	}
	ps.mu.Lock()
	ps.plans[plan.ID] = plan
	ps.mu.Unlock()
	for i, s := range slices {
		s.PlanID = plan.ID
		s.Order = i + 1
		if s.ID == "" {
			s.ID = fmt.Sprintf("%s#%d", plan.ID, i+1)
		}
		if err := ps.persistSlice(ctx, s); err != nil {
			t.Fatalf("seed slice %s: %v", s.ID, err)
		}
	}
}

func mergedMRJSON(repo string, iid int) string {
	return `[{"repo":"` + repo + `","iid":` + strconv.Itoa(iid) + `,"state":"merged"}]`
}

func openMRJSON(repo string, iid int) string {
	return `[{"repo":"` + repo + `","iid":` + strconv.Itoa(iid) + `,"state":"ci_running"}]`
}

func mustSlice(t *testing.T, ps *PlanSvc, id string) *PlanSlice {
	t.Helper()
	s, err := ps.fetchSlice(context.Background(), id)
	if err != nil || s == nil {
		t.Fatalf("slice %s not found: %v", id, err)
	}
	return s
}

func mustPlan(t *testing.T, ps *PlanSvc, id string) *Plan {
	t.Helper()
	p, err := ps.fetch(context.Background(), id)
	if err != nil || p == nil {
		t.Fatalf("plan %s not found: %v", id, err)
	}
	return p
}

// TestPlanSweep_MergedSliceAdvancesWithNote is the core case: a slice whose
// branch has a merged MR moves to merged and records why.
func TestPlanSweep_MergedSliceAdvancesWithNote(t *testing.T) {
	_, hud := newFakeHUD(t, map[string]string{
		"feat/slice-a": mergedMRJSON("services/loom-core", 1400),
	})
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-a", Project: "services/loom-core", Phase: PlanPhaseInProgress},
		&PlanSlice{ID: "plan-a#1", Name: "slice a", BranchName: "feat/slice-a", Phase: SlicePhaseImplemented},
	)

	r := newTestPlanReconciler(ps, hud.URL)
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats == nil || stats.SlicesAdvanced != 1 {
		t.Fatalf("expected 1 slice advanced, got %+v", stats)
	}

	s := mustSlice(t, ps, "plan-a#1")
	if s.Phase != SlicePhaseMerged {
		t.Fatalf("slice phase = %q, want %q", s.Phase, SlicePhaseMerged)
	}
	if len(s.Decisions) != 1 {
		t.Fatalf("expected exactly one decision note, got %v", s.Decisions)
	}
	note := s.Decisions[0]
	if !strings.Contains(note, "auto-advanced: MR merged (services/loom-core!1400)") ||
		!strings.Contains(note, "[plan-truth-sweep]") {
		t.Fatalf("unexpected decision note %q", note)
	}
	if r.metrics.PlanSweepSlicesAdvanced.Load() != 1 {
		t.Fatalf("slices-advanced counter = %d, want 1", r.metrics.PlanSweepSlicesAdvanced.Load())
	}
}

// TestPlanSweep_UnmergedSliceUntouched covers the three ways a slice must be
// left alone: its MR is still open, the HUD reports no MR at all (a merged MR
// simply leaves the open-MR registry — absence is never merged), and it has no
// branch/MR provenance to query with.
func TestPlanSweep_UnmergedSliceUntouched(t *testing.T) {
	hud, srv := newFakeHUD(t, map[string]string{
		"feat/open": openMRJSON("services/loom-core", 1401),
	})
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-b", Project: "services/loom-core", Phase: PlanPhaseInProgress},
		&PlanSlice{ID: "plan-b#1", BranchName: "feat/open", Phase: SlicePhaseInReview},
		&PlanSlice{ID: "plan-b#2", BranchName: "feat/vanished", Phase: SlicePhaseImplemented},
		&PlanSlice{ID: "plan-b#3", Phase: SlicePhaseImplementing},
		&PlanSlice{ID: "plan-b#4", MRRef: "services/loom-core!1402", Phase: SlicePhaseImplemented},
	)

	r := newTestPlanReconciler(ps, srv.URL)
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.SlicesAdvanced != 0 || stats.PlansAdvanced != 0 {
		t.Fatalf("expected no advances, got %+v", stats)
	}
	for _, id := range []string{"plan-b#1", "plan-b#2", "plan-b#3", "plan-b#4"} {
		s := mustSlice(t, ps, id)
		if s.Phase == SlicePhaseMerged {
			t.Fatalf("slice %s was advanced to merged", id)
		}
		if len(s.Decisions) != 0 {
			t.Fatalf("slice %s gained decisions %v", id, s.Decisions)
		}
	}
	if p := mustPlan(t, ps, "plan-b"); p.Phase != PlanPhaseInProgress {
		t.Fatalf("plan phase = %q, want unchanged in_progress", p.Phase)
	}
	// Slices without a resolvable branch must not cost a HUD round trip.
	for _, b := range hud.queriedBranches() {
		if b == "" || b == "services/loom-core!1402" {
			t.Fatalf("sweep queried the HUD with a non-branch key %q", b)
		}
	}
}

// TestPlanSweep_PlanAdvancesOnlyWhenAllSlicesMerged pins the plan-level rule and
// the stepwise walk: in_progress cannot reach merged in one legal hop, so the
// sweep must route through in_review and merging.
func TestPlanSweep_PlanAdvancesOnlyWhenAllSlicesMerged(t *testing.T) {
	hud, srv := newFakeHUD(t, map[string]string{
		"feat/one": mergedMRJSON("services/loom-core", 1410),
	})
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-c", Project: "services/loom-core", Phase: PlanPhaseInProgress},
		&PlanSlice{ID: "plan-c#1", BranchName: "feat/one", Phase: SlicePhaseImplemented},
		&PlanSlice{ID: "plan-c#2", BranchName: "feat/two", Phase: SlicePhaseImplementing},
	)

	r := newTestPlanReconciler(ps, srv.URL)
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if stats.SlicesAdvanced != 1 {
		t.Fatalf("first pass: expected 1 slice advanced, got %+v", stats)
	}
	if stats.PlansAdvanced != 0 {
		t.Fatalf("plan advanced with an unmerged slice outstanding: %+v", stats)
	}
	if p := mustPlan(t, ps, "plan-c"); p.Phase != PlanPhaseInProgress {
		t.Fatalf("plan phase = %q, want in_progress while a slice is unmerged", p.Phase)
	}

	// The second slice's MR lands; the next pass takes the plan to merged.
	hud.mu.Lock()
	hud.byBranch["feat/two"] = mergedMRJSON("services/loom-core", 1411)
	hud.mu.Unlock()

	stats, err = r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if stats.SlicesAdvanced != 1 || stats.PlansAdvanced != 1 {
		t.Fatalf("second pass: expected 1 slice + 1 plan advanced, got %+v", stats)
	}

	p := mustPlan(t, ps, "plan-c")
	if p.Phase != PlanPhaseMerged {
		t.Fatalf("plan phase = %q, want merged", p.Phase)
	}
	got := make([]string, 0, len(p.PhaseHistory))
	for _, h := range p.PhaseHistory {
		got = append(got, h.From+"->"+h.To)
		if h.Actor != planSweepActor {
			t.Fatalf("transition %s actor = %q, want %q", h.From+"->"+h.To, h.Actor, planSweepActor)
		}
		if h.Note != planSweepPlanNote {
			t.Fatalf("transition %s note = %q, want %q", h.From+"->"+h.To, h.Note, planSweepPlanNote)
		}
	}
	want := []string{"in_progress->in_review", "in_review->merging", "merging->merged"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase history = %v, want %v", got, want)
	}
	if r.metrics.PlanSweepPlansAdvanced.Load() != 1 {
		t.Fatalf("plans-advanced counter = %d, want 1", r.metrics.PlanSweepPlansAdvanced.Load())
	}

	// A third pass is a no-op: everything is already merged.
	stats, err = r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if stats.SlicesAdvanced != 0 || stats.PlansAdvanced != 0 || stats.Errors != 0 {
		t.Fatalf("third pass should be a no-op, got %+v", stats)
	}
}

// TestPlanSweep_DisabledWithoutHUD: with no HUD base URL there is no merged
// signal, so the sweep must not run at all — never advance on absence.
func TestPlanSweep_DisabledWithoutHUD(t *testing.T) {
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-d", Project: "services/loom-core", Phase: PlanPhaseMerging},
		&PlanSlice{ID: "plan-d#1", BranchName: "feat/d", Phase: SlicePhaseImplemented},
	)

	r := newTestPlanReconciler(ps, "")
	if r.Enabled() {
		t.Fatal("sweep reports enabled without a HUD base URL")
	}
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats != nil {
		t.Fatalf("expected no stats from a disabled sweep, got %+v", stats)
	}
	if s := mustSlice(t, ps, "plan-d#1"); s.Phase != SlicePhaseImplemented {
		t.Fatalf("disabled sweep advanced slice to %q", s.Phase)
	}
	if p := mustPlan(t, ps, "plan-d"); p.Phase != PlanPhaseMerging {
		t.Fatalf("disabled sweep advanced plan to %q", p.Phase)
	}

	// Start must be inert too (no goroutine, no state change).
	r.Start(context.Background())
	r.mu.RLock()
	running := r.running
	r.mu.RUnlock()
	if running {
		t.Fatal("disabled sweep started its loop")
	}
}

// TestPlanSweep_NeverRegressesPhase: plans at or past merged are outside the
// swept phase set, and the path function refuses any backwards or lateral walk.
func TestPlanSweep_NeverRegressesPhase(t *testing.T) {
	_, srv := newFakeHUD(t, map[string]string{
		"feat/done": mergedMRJSON("services/loom-core", 1420),
	})
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-e", Project: "services/loom-core", Phase: PlanPhaseDeployed},
		&PlanSlice{ID: "plan-e#1", BranchName: "feat/done", Phase: SlicePhaseMerged},
	)

	r := newTestPlanReconciler(ps, srv.URL)
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.PlansScanned != 0 || stats.PlansAdvanced != 0 {
		t.Fatalf("a deployed plan must not be swept, got %+v", stats)
	}
	if p := mustPlan(t, ps, "plan-e"); p.Phase != PlanPhaseDeployed {
		t.Fatalf("plan regressed to %q", p.Phase)
	}

	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		{PlanPhaseMerging, PlanPhaseMerged, []string{PlanPhaseMerged}},
		{PlanPhaseInReview, PlanPhaseMerged, []string{PlanPhaseMerging, PlanPhaseMerged}},
		{PlanPhasePlanned, PlanPhaseMerged, []string{PlanPhaseInProgress, PlanPhaseInReview, PlanPhaseMerging, PlanPhaseMerged}},
		{PlanPhaseMerged, PlanPhaseMerged, nil},   // no-op is not a hop
		{PlanPhaseDone, PlanPhaseMerged, nil},     // backwards
		{PlanPhaseDeployed, PlanPhaseMerged, nil}, // backwards
		{PlanPhaseAbandoned, PlanPhaseMerged, nil},
		{"bogus", PlanPhaseMerged, nil},
	} {
		got := planPhasePathTo(tc.from, tc.to)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("planPhasePathTo(%q,%q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
		// Every hop must strictly increase rank.
		prev := planPhaseRank(tc.from)
		for _, hop := range got {
			if planPhaseRank(hop) <= prev {
				t.Fatalf("path %v regresses at %q", got, hop)
			}
			prev = planPhaseRank(hop)
		}
	}
}

// TestPlanSweep_StaleSnapshotFailsOpen: a stale registry snapshot is not
// evidence of a merge, so nothing moves.
func TestPlanSweep_StaleSnapshotFailsOpen(t *testing.T) {
	hud, srv := newFakeHUD(t, map[string]string{
		"feat/stale": mergedMRJSON("services/loom-core", 1430),
	})
	hud.mu.Lock()
	hud.stale = true
	hud.mu.Unlock()

	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-f", Project: "services/loom-core", Phase: PlanPhaseMerging},
		&PlanSlice{ID: "plan-f#1", BranchName: "feat/stale", Phase: SlicePhaseImplemented},
	)

	r := newTestPlanReconciler(ps, srv.URL)
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.SlicesAdvanced != 0 || stats.PlansAdvanced != 0 {
		t.Fatalf("stale snapshot must not advance anything, got %+v", stats)
	}
}

// TestPlanSweep_HUDOutageIsNotAnAdvance: an unreachable HUD looks exactly like
// "no open MR"; the sweep must treat it as unknown, not merged.
func TestPlanSweep_HUDOutageIsNotAnAdvance(t *testing.T) {
	ps := newTestPlanSvc()
	seedPlan(t, ps,
		&Plan{ID: "plan-g", Project: "services/loom-core", Phase: PlanPhaseMerging},
		&PlanSlice{ID: "plan-g#1", BranchName: "feat/g", Phase: SlicePhaseImplemented},
	)

	r := newTestPlanReconciler(ps, "http://127.0.0.1:1")
	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile must not fail on an unreachable HUD: %v", err)
	}
	if stats.SlicesAdvanced != 0 || stats.PlansAdvanced != 0 {
		t.Fatalf("unreachable HUD advanced phases: %+v", stats)
	}
	if s := mustSlice(t, ps, "plan-g#1"); s.Phase != SlicePhaseImplemented {
		t.Fatalf("slice phase = %q, want unchanged", s.Phase)
	}
}

func TestSliceMergeBranch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slice PlanSlice
		want  string
	}{
		{"branch wins", PlanSlice{BranchName: "feat/x", MRRef: "!12"}, "feat/x"},
		{"branch-shaped mr_ref", PlanSlice{MRRef: "feat/y"}, "feat/y"},
		{"iid mr_ref", PlanSlice{MRRef: "!1234"}, ""},
		{"repo!iid mr_ref", PlanSlice{MRRef: "services/loom-core!1234"}, ""},
		{"url mr_ref", PlanSlice{MRRef: "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1"}, ""},
		{"bare word mr_ref", PlanSlice{MRRef: "1234"}, ""},
		{"nothing", PlanSlice{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sliceMergeBranch(&tc.slice); got != tc.want {
				t.Fatalf("sliceMergeBranch = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlanSweep_PersistsSliceThroughQdrant proves the advance reaches the store,
// not just the in-memory cache: the sweep's slice write must arrive as a points
// upsert carrying status="merged".
func TestPlanSweep_PersistsSliceThroughQdrant(t *testing.T) {
	_, hud := newFakeHUD(t, map[string]string{
		"feat/persisted": mergedMRJSON("services/loom-core", 1440),
	})

	var mu sync.Mutex
	var upserted []map[string]any
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/planSlices":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"config":{"params":{"vectors":{"size":4,"distance":"Cosine"}}}}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/planSlices/index":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"acknowledged"}}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/collections/planSlices/points"):
			var body struct {
				Points []struct {
					Payload map[string]any `json:"payload"`
				} `json:"points"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, p := range body.Points {
				upserted = append(upserted, p.Payload)
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"acknowledged"}}`))
		default:
			// Scrolls/gets fall through to the cache path; ack anything else.
			http.NotFound(w, r)
		}
	}))
	defer qdrant.Close()

	cfg := Config{QdrantURL: qdrant.URL, QdrantDistance: "Cosine", PlanSlicesCollection: "planSlices"}
	reg := NewQdrantRegistry(httpclient.NewDefault(), cfg)
	vectorSize := sessionsVectorSize
	ps := NewPlanSvc(nil, reg.Get(CollPlanSlices), nil, &vectorSize, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedPlan(t, ps,
		&Plan{ID: "plan-h", Project: "services/loom-core", Phase: PlanPhaseMerging},
		&PlanSlice{ID: "plan-h#1", BranchName: "feat/persisted", Phase: SlicePhaseImplemented},
	)

	mu.Lock()
	upserted = nil
	mu.Unlock()

	r := newTestPlanReconciler(ps, hud.URL)
	if _, err := r.TriggerReconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(upserted) != 1 {
		t.Fatalf("expected exactly one slice upsert, got %d", len(upserted))
	}
	if got := upserted[0]["status"]; got != SlicePhaseMerged {
		t.Fatalf("persisted status = %v, want %q", got, SlicePhaseMerged)
	}
	decisions, _ := upserted[0]["decisions"].([]any)
	if len(decisions) != 1 || !strings.Contains(decisions[0].(string), "[plan-truth-sweep]") {
		t.Fatalf("persisted decisions = %v, want one plan-truth-sweep note", decisions)
	}
}
