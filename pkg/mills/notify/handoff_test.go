package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubCreator records the handoff requests it receives and optionally fails.
type stubCreator struct {
	calls []pipeline.HandoffRequest
	err   error
}

func (s *stubCreator) CreateHandoff(_ context.Context, req pipeline.HandoffRequest) (pipeline.HandoffResponse, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return pipeline.HandoffResponse{}, s.err
	}
	return pipeline.HandoffResponse{HandoffID: "ho-1"}, nil
}

func mrPtr(v int64) *int64 { return &v }

func TestHandoffHook_OnMerged_PostsToInbox(t *testing.T) {
	st := &stubCreator{}
	h := NewHandoffHook(st, "", "https://gitlab.flexinfer.ai/services/loom-core", nil)
	if !h.Enabled() {
		t.Fatal("hook should be enabled with a non-nil creator")
	}
	if h.Target() != "mills-merges" {
		t.Fatalf("default target = %q, want mills-merges", h.Target())
	}

	run := &store.PipelineRun{ID: "PIPE-1", MRIID: mrPtr(42), CostUSD: 0.12, Attempts: 1}
	item := &store.BacklogItem{ID: "MILLS-X", Title: "Add a thing"}
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged err = %v", err)
	}

	if len(st.calls) != 1 {
		t.Fatalf("CreateHandoff calls = %d, want 1", len(st.calls))
	}
	req := st.calls[0]
	if req.To != "mills-merges" {
		t.Errorf("To = %q, want mills-merges", req.To)
	}
	if req.From != "loom-mills-operator" {
		t.Errorf("From = %q", req.From)
	}
	if req.BacklogID != "MILLS-X" || req.PipelineRun != "PIPE-1" {
		t.Errorf("BacklogID/PipelineRun = %q/%q", req.BacklogID, req.PipelineRun)
	}
	if !strings.Contains(req.Reason, "MILLS-X") || !strings.Contains(req.Reason, "merge_requests/42") {
		t.Errorf("Reason = %q, want it to mention the backlog id + MR link", req.Reason)
	}
	if req.Context["event"] != "mills.merged" {
		t.Errorf("Context[event] = %v, want mills.merged", req.Context["event"])
	}
	if req.Context["mr_iid"] != int64(42) {
		t.Errorf("Context[mr_iid] = %v, want 42", req.Context["mr_iid"])
	}
}

func TestHandoffHook_OnMerged_SwallowsError(t *testing.T) {
	st := &stubCreator{err: errors.New("hub down")}
	h := NewHandoffHook(st, "custom-inbox", "", nil)
	if got := h.Target(); got != "custom-inbox" {
		t.Fatalf("target = %q, want custom-inbox", got)
	}
	run := &store.PipelineRun{ID: "PIPE-2"}
	item := &store.BacklogItem{ID: "MILLS-Y", Title: "y"}
	// A creator failure must be swallowed so the OnMerged chain continues.
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged must swallow creator errors, got %v", err)
	}
	if len(st.calls) != 1 {
		t.Fatalf("CreateHandoff calls = %d, want 1 (attempted despite failure)", len(st.calls))
	}
}

func TestHandoffHook_Disabled_NilCreator(t *testing.T) {
	h := NewHandoffHook(nil, "", "", nil)
	if h.Enabled() {
		t.Fatal("a nil creator must yield a disabled hook")
	}
	// Disabled OnMerged is a no-op and never errors.
	run := &store.PipelineRun{ID: "PIPE-3"}
	item := &store.BacklogItem{ID: "Z", Title: "z"}
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("disabled OnMerged err = %v", err)
	}
}

// A nil *HandoffHook must be safe to call so the operator's late-bound closure
// can invoke it before the hook is assigned.
func TestHandoffHook_NilReceiver_Safe(t *testing.T) {
	var h *HandoffHook
	if h.Enabled() {
		t.Fatal("nil hook must report disabled")
	}
	if err := h.OnMerged(context.Background(), &store.PipelineRun{ID: "x"}, &store.BacklogItem{ID: "y"}); err != nil {
		t.Fatalf("nil-receiver OnMerged err = %v", err)
	}
}
