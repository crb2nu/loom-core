package clients

import (
	"context"
	"testing"
	"time"
)

type countingPlanHub struct {
	calls     map[string]int
	failWrite bool
}

func (h *countingPlanHub) CallTool(_ context.Context, _, tool string, _ map[string]any) (string, error) {
	if h.calls == nil {
		h.calls = map[string]int{}
	}
	h.calls[tool]++
	if tool == "agent_plan_slice_list" {
		return `{"ok":true,"slices":[{"id":"p#1","plan_id":"p","name":"one","phase":"pending"}]}`, nil
	}
	if h.failWrite {
		return `{"ok":false}`, nil
	}
	return `{"ok":true}`, nil
}

func TestPlanClientListSlicesIfChangedCacheExpiryAndStamp(t *testing.T) {
	hub := &countingPlanHub{}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	c := &PlanClient{Hub: hub, now: func() time.Time { return now }}
	plan := PlanSummary{ID: "p", UpdatedAt: "2026-09-12T11:00:00Z"}
	for range 2 {
		if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 1 {
		t.Fatalf("unchanged calls = %d, want 1", got)
	}

	plan.UpdatedAt = "2026-09-12T11:01:00Z"
	if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 2 {
		t.Fatalf("moved stamp calls = %d, want 2", got)
	}

	now = now.Add(time.Hour)
	if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 3 {
		t.Fatalf("expired calls = %d, want 3", got)
	}
}

func TestPlanClientListSlicesIfChangedInvalidStampAndWriteInvalidation(t *testing.T) {
	hub := &countingPlanHub{}
	c := &PlanClient{Hub: hub}
	for _, stamp := range []string{"", "not-a-time"} {
		if _, err := c.ListSlicesIfChanged(context.Background(), PlanSummary{ID: "p", UpdatedAt: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 2 {
		t.Fatalf("invalid stamp calls = %d, want 2", got)
	}

	plan := PlanSummary{ID: "p", UpdatedAt: "2026-09-12T11:00:00Z"}
	if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateSlicePhase(context.Background(), "p#1", "implementing"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 4 {
		t.Fatalf("post-write calls = %d, want 4", got)
	}

	hub.failWrite = true
	if err := c.UpdateSlicePhase(context.Background(), "p#1", "merged"); err == nil {
		t.Fatal("expected failed write")
	}
	if _, err := c.ListSlicesIfChanged(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if got := hub.calls["agent_plan_slice_list"]; got != 4 {
		t.Fatalf("failed write invalidated cache: calls = %d", got)
	}
}

func TestPlanSummaryUpdatedAtDecodesFromCapturedListPayload(t *testing.T) {
	const captured = `{"ok":true,"plans":[{"id":"p","project":"services/loom-core","phase":"planned","title":"Cache slices","priority":"P1","updated_at":"2026-09-12T11:22:33.123Z"}]}`
	var env planListEnvelope
	if err := decodeListBody(captured, &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Plans[0].UpdatedAt; got != "2026-09-12T11:22:33.123Z" {
		t.Fatalf("updated_at = %q", got)
	}
	const capturedTOON = "ok: true\nplans[1]{id,project,phase,title,priority,updated_at}:\n  p,services/loom-core,planned,Cache slices,P1,2026-09-12T11:22:33.123Z"
	var toon planListEnvelope
	if err := decodeListBody(capturedTOON, &toon); err != nil {
		t.Fatal(err)
	}
	if got := toon.Plans[0].UpdatedAt; got != "2026-09-12T11:22:33.123Z" {
		t.Fatalf("TOON updated_at = %q", got)
	}
}
