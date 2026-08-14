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

// stubRecorder records agent-context writes; seq is shared with stubCreator so
// ordering between the entry and the handoff can be asserted.
type stubRecorder struct {
	entries []stubEntry
	err     error
	seq     *int
}

type stubEntry struct {
	sessionID string
	entryType string
	title     string
	content   string
	tags      []string
	seq       int
}

func (s *stubRecorder) AddContextEntry(_ context.Context, sessionID, entryType, title, content string, tags []string) error {
	n := 0
	if s.seq != nil {
		*s.seq++
		n = *s.seq
	}
	s.entries = append(s.entries, stubEntry{
		sessionID: sessionID, entryType: entryType, title: title,
		content: content, tags: tags, seq: n,
	})
	return s.err
}

// orderedCreator stamps the shared seq counter when the handoff is created.
type orderedCreator struct {
	stubCreator
	seq  *int
	seen int
}

func (o *orderedCreator) CreateHandoff(ctx context.Context, req pipeline.HandoffRequest) (pipeline.HandoffResponse, error) {
	if o.seq != nil {
		*o.seq++
		o.seen = *o.seq
	}
	return o.stubCreator.CreateHandoff(ctx, req)
}

// TestHandoffHook_RecordsMergeFindingBeforeHandoff proves the merge outcome
// lands in the operator's agent-context session before the handoff that
// packages that session is created.
func TestHandoffHook_RecordsMergeFindingBeforeHandoff(t *testing.T) {
	seq := 0
	creator := &orderedCreator{seq: &seq}
	recorder := &stubRecorder{seq: &seq}
	h := NewHandoffHook(creator, "", "https://gitlab.flexinfer.ai/services/loom-core", nil)
	h.SetContextRecorder(recorder)

	run := &store.PipelineRun{ID: "PIPE-9", MRIID: mrPtr(77), CostUSD: 1.25, Attempts: 2}
	item := &store.BacklogItem{ID: "MILLS-M", Title: "Ship the thing"}
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}

	if len(recorder.entries) != 1 {
		t.Fatalf("context entries = %d, want 1", len(recorder.entries))
	}
	got := recorder.entries[0]
	if got.seq >= creator.seen {
		t.Errorf("entry at seq %d, handoff at %d; the entry must land first", got.seq, creator.seen)
	}
	if got.entryType != "finding" {
		t.Errorf("entry_type = %q, want finding", got.entryType)
	}
	if got.sessionID != "" {
		t.Errorf("session_id = %q, want empty (the recorder resolves the operator session)", got.sessionID)
	}
	if !strings.HasPrefix(got.title, "merged MILLS-M") {
		t.Errorf("title = %q, want a 'merged <item>' prefix", got.title)
	}
	for _, want := range []string{
		"MILLS-M", "PIPE-9", "!77",
		"https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/77",
		"$1.25", "2 attempt(s)",
	} {
		if !strings.Contains(got.content, want) {
			t.Errorf("content missing %q; full=\n%s", want, got.content)
		}
	}
}

// TestHandoffHook_RecorderFailureDoesNotBlockHandoff keeps the recorder
// advisory: a hub outage must not drop the merge notification.
func TestHandoffHook_RecorderFailureDoesNotBlockHandoff(t *testing.T) {
	creator := &stubCreator{}
	h := NewHandoffHook(creator, "", "", nil)
	h.SetContextRecorder(&stubRecorder{err: errors.New("hub down")})

	run := &store.PipelineRun{ID: "PIPE-10"}
	item := &store.BacklogItem{ID: "MILLS-N", Title: "n"}
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	if len(creator.calls) != 1 {
		t.Fatalf("CreateHandoff calls = %d, want 1 despite the recorder failure", len(creator.calls))
	}
}

// TestHandoffHook_NoRecorderIsANoOp keeps the pre-recorder wiring valid.
func TestHandoffHook_NoRecorderIsANoOp(t *testing.T) {
	creator := &stubCreator{}
	h := NewHandoffHook(creator, "", "", nil)
	run := &store.PipelineRun{ID: "PIPE-11"}
	item := &store.BacklogItem{ID: "MILLS-O", Title: "o"}
	if err := h.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	if len(creator.calls) != 1 {
		t.Fatalf("CreateHandoff calls = %d, want 1", len(creator.calls))
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
