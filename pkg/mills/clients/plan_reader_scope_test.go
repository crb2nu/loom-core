package clients

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeSliceHub is a HubCaller stub for the slice-scope reader: canned bodies
// per tool, keyed by slice_id for the detail view.
type fakeSliceHub struct {
	listBody string
	listErr  error
	getBody  map[string]string
	getErr   map[string]error
	calls    []string
}

func (f *fakeSliceHub) CallTool(_ context.Context, _, toolName string, args map[string]any) (string, error) {
	f.calls = append(f.calls, toolName)
	switch toolName {
	case "agent_plan_slice_list":
		return f.listBody, f.listErr
	case "agent_plan_slice_get":
		id, _ := args["slice_id"].(string)
		if err := f.getErr[id]; err != nil {
			return "", err
		}
		return f.getBody[id], nil
	}
	return "", fmt.Errorf("unexpected tool %s", toolName)
}

// TestPlanClient_SliceScopeForPlan_RecoversFilesFromDetailView pins the
// hydration contract behind Runner.SliceHydrator: the LIST view omits the
// `files` array (TOON tabular), so a file-less slice is re-fetched via the
// detail view, and a slice that STILL declares no files is dropped rather
// than stamped — a file-less slice would make the item look scoped while the
// scope gate's allowlist stayed empty (the #332 shape).
func TestPlanClient_SliceScopeForPlan_RecoversFilesFromDetailView(t *testing.T) {
	hub := &fakeSliceHub{
		listBody: `{"ok":true,"slices":[
			{"id":"p#1","plan_id":"p","name":"inline","files":["pkg/a/a.go"]},
			{"id":"p#2","plan_id":"p","name":"detail-recovered"},
			{"id":"p#3","plan_id":"p","name":"genuinely-file-less"}
		]}`,
		getBody: map[string]string{
			"p#2": `{"ok":true,"slice":{"id":"p#2","plan_id":"p","name":"detail-recovered","files":["pkg/b/b.go","pkg/b/b_test.go"]}}`,
			"p#3": `{"ok":true,"slice":{"id":"p#3","plan_id":"p","name":"genuinely-file-less"}}`,
		},
	}
	pc := &PlanClient{Hub: hub}
	slices, files, err := pc.SliceScopeForPlan(context.Background(), "p")
	if err != nil {
		t.Fatalf("SliceScopeForPlan: %v", err)
	}
	if len(slices) != 2 {
		t.Fatalf("slices = %+v, want 2 (file-less slice dropped)", slices)
	}
	if slices[0].Name != "inline" || len(slices[0].Files) != 1 {
		t.Errorf("slice[0] = %+v, want inline with 1 file", slices[0])
	}
	if slices[1].Name != "detail-recovered" || len(slices[1].Files) != 2 {
		t.Errorf("slice[1] = %+v, want detail-recovered with 2 files", slices[1])
	}
	if len(files) != 3 {
		t.Errorf("flat files = %v, want 3 aggregated paths", files)
	}
}

// TestPlanClient_SliceScopeForPlan_ListErrorBubbles: a list failure returns
// the error so the runner can log the hydration miss (the scope gate then
// skips downstream — hydration is best-effort, never run-fatal).
func TestPlanClient_SliceScopeForPlan_ListErrorBubbles(t *testing.T) {
	hub := &fakeSliceHub{listErr: errors.New("hub down")}
	pc := &PlanClient{Hub: hub}
	if _, _, err := pc.SliceScopeForPlan(context.Background(), "p"); err == nil {
		t.Fatal("want error when the slice list fails, got nil")
	}
}

// TestPlanClient_SliceScopeForPlan_DetailErrorSkipsOnlyThatSlice: a detail
// fetch failure drops the affected slice but keeps the rest — mirroring the
// plan-slice emitter's per-slice resilience.
func TestPlanClient_SliceScopeForPlan_DetailErrorSkipsOnlyThatSlice(t *testing.T) {
	hub := &fakeSliceHub{
		listBody: `{"ok":true,"slices":[
			{"id":"p#1","plan_id":"p","name":"broken"},
			{"id":"p#2","plan_id":"p","name":"ok","files":["pkg/c/c.go"]}
		]}`,
		getErr: map[string]error{"p#1": errors.New("detail fetch failed")},
	}
	pc := &PlanClient{Hub: hub}
	slices, files, err := pc.SliceScopeForPlan(context.Background(), "p")
	if err != nil {
		t.Fatalf("SliceScopeForPlan: %v", err)
	}
	if len(slices) != 1 || slices[0].Name != "ok" {
		t.Fatalf("slices = %+v, want only the intact slice", slices)
	}
	if len(files) != 1 || files[0] != "pkg/c/c.go" {
		t.Errorf("files = %v, want [pkg/c/c.go]", files)
	}
}
