package takeup

import (
	"context"
	"sync"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/intake"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestNew_AppliesTickTimeoutDefault pins the per-tick deadline default so a
// Config that omits it still bounds each pass (the wedge guard is never off).
func TestNew_AppliesTickTimeoutDefault(t *testing.T) {
	r := New(newFakePlanStore(), &fakeMRs{}, newFakeBacklog(), Config{Namespace: "x"}, nil)
	if r.cfg.TickTimeout != defaultTickTimeout {
		t.Fatalf("TickTimeout = %v, want default %v", r.cfg.TickTimeout, defaultTickTimeout)
	}
}

// blockingPlanStore models the production wedge: ListPlans hangs on a stalled
// hub/GitLab dependency until its context is done. With an unbounded initial
// tick (the pre-fix Run) this blocks the goroutine before the ticker starts,
// so ListPlans is called exactly once, forever. With the per-tick deadline the
// initial pass times out and the loop keeps ticking.
type blockingPlanStore struct {
	mu    sync.Mutex
	calls int
}

func (b *blockingPlanStore) ListPlans(ctx context.Context, _, _, _ string) ([]clients.PlanSummary, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-ctx.Done() // released by the per-tick deadline (or parent cancel)
	return nil, ctx.Err()
}
func (b *blockingPlanStore) ListSlices(context.Context, string) ([]clients.PlanSliceSummary, error) {
	return nil, nil
}
func (b *blockingPlanStore) GetSlice(context.Context, string) (clients.PlanSliceSummary, error) {
	return clients.PlanSliceSummary{}, nil
}
func (b *blockingPlanStore) UpdateSlicePhase(context.Context, string, string) error    { return nil }
func (b *blockingPlanStore) AppendSliceDecision(context.Context, string, string) error { return nil }
func (b *blockingPlanStore) AdvancePlan(context.Context, string, string, string) error { return nil }
func (b *blockingPlanStore) listCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestTakeup_StalledTickDoesNotWedgeRun is the regression test for the
// 2026-07-03 incident: a stalled dependency in the FIRST tick must not wedge
// Run. A wedged reconciler calls ListPlans once and never again; the fix keeps
// the loop ticking, so we observe multiple calls and a prompt shutdown.
func TestTakeup_StalledTickDoesNotWedgeRun(t *testing.T) {
	ps := &blockingPlanStore{}
	r := New(ps, &fakeMRs{}, newFakeBacklog(), Config{
		Namespace:    "mills/x",
		PollInterval: 40 * time.Millisecond,
		TickTimeout:  20 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let the initial pass plus several periodic passes fire; each blocks then
	// times out at 20ms.
	time.Sleep(220 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel — reconciler wedged")
	}

	if n := ps.listCalls(); n < 2 {
		t.Fatalf("ListPlans called %d times; a wedged initial tick allows only 1 — the loop never advanced past the stall", n)
	}
}

// fakeHub is a HubCaller that returns canned per-tool bodies. Unlike the
// clients-package fakeTransport (keyed on JSON-RPC method, always "tools/call"),
// this dispatches on the tool NAME so a multi-call reconcile pass can decode
// distinct list/write responses in one tick.
type fakeHub struct {
	mu        sync.Mutex
	responses map[string]string // toolName -> raw body (TOON or JSON)
	planLists map[string]string // phase -> raw agent_plan_list body
	calls     []string
}

func (h *fakeHub) CallTool(_ context.Context, _, tool string, args map[string]any) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, tool)
	if tool == "agent_plan_list" && h.planLists != nil {
		phase, _ := args["phase"].(string)
		if body, ok := h.planLists[phase]; ok {
			return body, nil
		}
	}
	if body, ok := h.responses[tool]; ok {
		return body, nil
	}
	// Unstubbed writes default to an ok envelope so the reconciler's write
	// path succeeds; unstubbed reads would decode to an empty list.
	return "ok: true", nil
}

func (h *fakeHub) called(tool string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == tool {
			n++
		}
	}
	return n
}

// toonBody encodes v through the REAL mcp result formatter, forced to TOON —
// the exact tabular bytes the agent-context server ships on the wire. Feeding
// these back through clients.PlanClient exercises the genuine DecodeTOONToJSON
// path, not a hand-reconstructed fixture (cf. the "capture real bytes before
// decoder fix" lesson).
func toonBody(t *testing.T, v any) string {
	t.Helper()
	res, err := mcp.JSONResult(v)
	if err != nil {
		t.Fatalf("encode TOON fixture: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Text == "" {
		t.Fatalf("TOON fixture produced no text content: %+v", res)
	}
	return res.Content[0].Text
}

// TestTakeup_RealHubTOONDecodePathEndToEnd is the integration test the pre-fix
// suite lacked: it drives the reconciler through a REAL *clients.PlanClient
// whose hub returns production-shaped TOON (from mcp.JSONResult), so the whole
// decode path — agent_plan_list + agent_plan_slice_list tabular TOON, the
// mr_ref/phase projections, and the ok-envelope on writes — runs end to end.
// A merged MR must advance the slice to merged, close its emitted backlog item,
// and roll the single-slice plan forward to merged.
func TestTakeup_RealHubTOONDecodePathEndToEnd(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "toon") // force the tabular producer

	const (
		planID  = "plan-takeup-killtest-20260703"
		sliceID = "plan-takeup-killtest-20260703#1"
	)

	planList := toonBody(t, map[string]any{
		"ok": true,
		"plans": []map[string]any{{
			"id":       planID,
			"project":  "services/loom-core",
			"phase":    "planned",
			"title":    "Take-up kill test",
			"priority": "P2",
		}},
	})
	sliceList := toonBody(t, map[string]any{
		"ok": true,
		// The tabular LIST view omits array columns (files, decisions) — mirror
		// that so the fixture matches what the live agent_plan_slice_list emits.
		"slices": []map[string]any{{
			"id":                  sliceID,
			"plan_id":             planID,
			"name":                "reconcile the beam",
			"phase":               "implementing",
			"goal":                "true plan state to MR reality",
			"acceptance_criteria": "slice advances to merged",
			"mr_ref":              "!913",
		}},
	})
	okBody := toonBody(t, map[string]any{"ok": true})
	emptyPlanList := toonBody(t, map[string]any{"ok": true, "plans": []map[string]any{}})

	hub := &fakeHub{responses: map[string]string{
		"agent_plan_slice_list":        sliceList,
		"agent_plan_slice_update":      okBody,
		"agent_plan_lifecycle_advance": okBody,
	}, planLists: map[string]string{
		"":            planList,
		"planned":     planList,
		"in_progress": emptyPlanList,
		"in_review":   emptyPlanList,
		"merging":     emptyPlanList,
	}}

	// Real PlanClient over the fake hub — ServerName empty resolves to the
	// agent-context default. This is the seam the interface field enables.
	pc := &clients.PlanClient{Hub: hub, AgentID: "mills:test"}

	// Sanity-pin the decode itself before driving the reconciler, so a broken
	// TOON round-trip fails here with a clear message rather than as a silent
	// no-op downstream (the exact live verification from the incident report).
	plans, err := pc.ListPlans(context.Background(), "services/loom-core", "mills/demand-sourcing", "")
	if err != nil {
		t.Fatalf("ListPlans decode: %v", err)
	}
	if len(plans) != 1 || plans[0].Phase != "planned" {
		t.Fatalf("decoded plans = %+v, want one planned plan", plans)
	}
	slices, err := pc.ListSlices(context.Background(), planID)
	if err != nil {
		t.Fatalf("ListSlices decode: %v", err)
	}
	if len(slices) != 1 || slices[0].MRRef != "!913" || slices[0].Phase != "implementing" {
		t.Fatalf("decoded slice = %+v, want mr_ref=!913 phase=implementing", slices)
	}

	// Seed the emitted backlog item so the close path is exercised too.
	bs := newFakeBacklog()
	itemID := intake.BacklogIDForSlice(sliceID)
	bs.items[itemID] = &store.BacklogItem{ID: itemID, State: store.BacklogQueued}

	mrs := &fakeMRs{states: map[int64]string{913: "merged"}}
	r := New(pc, mrs, bs, Config{Project: "services/loom-core", Namespace: "mills/demand-sourcing"}, nil)

	stats, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stats.SlicesMerged != 1 {
		t.Errorf("SlicesMerged = %d, want 1", stats.SlicesMerged)
	}
	if stats.PlansMerged != 1 {
		t.Errorf("PlansMerged = %d, want 1 (single merged slice rolls the plan forward)", stats.PlansMerged)
	}
	if stats.ItemsClosed != 1 {
		t.Errorf("ItemsClosed = %d, want 1", stats.ItemsClosed)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
	if bs.items[itemID].State != store.BacklogMerged {
		t.Errorf("backlog item state = %q, want merged", bs.items[itemID].State)
	}
	// planned → in_progress → in_review → merging → merged = 4 lifecycle writes.
	if got := hub.called("agent_plan_lifecycle_advance"); got != 4 {
		t.Errorf("agent_plan_lifecycle_advance calls = %d, want 4 hops to merged", got)
	}
	if got := hub.called("agent_plan_slice_update"); got != 1 {
		t.Errorf("agent_plan_slice_update calls = %d, want 1 (slice→merged)", got)
	}
}
