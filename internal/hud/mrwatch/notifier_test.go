package mrwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSender records every inbox message and can be primed to fail.
type fakeSender struct {
	mu   sync.Mutex
	msgs []InboxMessage
	err  error
}

func (f *fakeSender) SendInbox(_ context.Context, msg InboxMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msg)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func (f *fakeSender) last() (InboxMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) == 0 {
		return InboxMessage{}, false
	}
	return f.msgs[len(f.msgs)-1], true
}

// snapWith builds a one-MR snapshot in the given state.
func snapWith(mr MergeRequest) Snapshot {
	return Snapshot{MergeRequests: []MergeRequest{mr}}
}

func mrIn(state State, branch string) MergeRequest {
	return MergeRequest{
		Repo:         "services/loom-core",
		IID:          42,
		SourceBranch: branch,
		State:        state,
		Reason:       "reason_" + string(state),
		WebURL:       "https://gitlab.example/mr/42",
	}
}

func newTestNotifier(sender MessageSender, resolver OwnerResolver) *Notifier {
	return NewNotifier(sender, resolver, NotifierOptions{
		Enabled: true,
		Now:     func() time.Time { return time.Unix(0, 0) },
	})
}

// ownerAlways resolves every branch to the same owner.
func ownerAlways(id, session string) OwnerResolver {
	return func(string) (Owner, bool) {
		return Owner{AgentID: id, SessionID: session}, true
	}
}

// ownerNever resolves nothing (unknown branch).
func ownerNever() OwnerResolver {
	return func(string) (Owner, bool) { return Owner{}, false }
}

func TestNotifier_TransitionDedup(t *testing.T) {
	tests := []struct {
		name      string
		states    []State // one poll per state, in order
		wantSends int     // total inbox messages after all polls
	}{
		{"enter once", []State{StateConflict}, 1},
		{"enter then hold (dedup)", []State{StateConflict, StateConflict, StateConflict}, 1},
		{"enter, leave, re-enter", []State{StateConflict, StateOK, StateConflict}, 2},
		{"change between notify states", []State{StateCIFailedFlaky, StateConflict}, 2},
		{"leave to non-notify then back", []State{StateAutomergeUnarmed, StateCIRunning, StateAutomergeUnarmed}, 2},
		{"non-notify never sends", []State{StateAwaitingPipeline, StateCIRunning, StateDraftIdle, StateOK}, 0},
		{"retained merged never sends", []State{StateMerged, StateMerged}, 0},
		{"stall then merge notifies once, not again", []State{StateConflict, StateMerged, StateMerged}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{}
			n := newTestNotifier(sender, ownerAlways("claude-code", "sess1"))
			for _, st := range tc.states {
				n.Notify(context.Background(), snapWith(mrIn(st, "feat/x")))
			}
			if got := sender.count(); got != tc.wantSends {
				t.Fatalf("inbox sends = %d, want %d (states=%v)", got, tc.wantSends, tc.states)
			}
		})
	}
}

func TestNotifier_OwningAgentJoinHit(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(sender, ownerAlways("codex", "sess-abc"))
	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "feat/join")))

	msg, ok := sender.last()
	if !ok {
		t.Fatal("expected an inbox message for an owned branch")
	}
	if msg.ToAgentID != "codex" {
		t.Errorf("to_agent = %q, want codex", msg.ToAgentID)
	}
	if msg.SessionRef != "sess-abc" {
		t.Errorf("session_ref = %q, want sess-abc", msg.SessionRef)
	}
	wantBody := "MR !42 (feat/join) entered conflict: reason_conflict — https://gitlab.example/mr/42"
	if msg.Body != wantBody {
		t.Errorf("body = %q, want %q", msg.Body, wantBody)
	}
}

func TestNotifier_OwningAgentJoinMiss(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(sender, ownerNever())
	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "feat/orphan")))

	if got := sender.count(); got != 0 {
		t.Fatalf("unknown branch must send no inbox message, got %d", got)
	}
	// ... but the attention lane still fires.
	lanes := n.Attention()
	if len(lanes) != 1 {
		t.Fatalf("attention lanes = %d, want 1 (attention fires even without an owner)", len(lanes))
	}
	if lanes[0].Lane != "conflict" {
		t.Errorf("lane = %q, want conflict", lanes[0].Lane)
	}
}

func TestNotifier_InboxFailureTolerated(t *testing.T) {
	sender := &fakeSender{err: errors.New("bridge down")}
	n := newTestNotifier(sender, ownerAlways("claude-code", ""))
	// Must not panic; the send error is swallowed.
	n.Notify(context.Background(), snapWith(mrIn(StateCIFailedDeterministic, "feat/y")))

	// Attention lane still recorded despite the failed send.
	if got := len(n.Attention()); got != 1 {
		t.Fatalf("attention lanes = %d, want 1 after a failed send", got)
	}
}

func TestNotifier_AttentionLaneClassifyByType(t *testing.T) {
	sender := &fakeSender{}

	cases := []struct {
		state    State
		wantLane string
		wantSev  string
	}{
		{StateConflict, "conflict", "critical"},
		{StateCIFailedDeterministic, "merge", "critical"},
		{StateCIFailedFlaky, "merge", "warning"},
		{StatePipelineSkipped, "merge", "warning"},
		{StateAutomergeUnarmed, "merge", "warning"},
		{StateStaleBranch, "merge", "info"},
	}
	allowed := map[string]bool{"merge": true, "conflict": true}
	for _, tc := range cases {
		n2 := newTestNotifier(sender, ownerAlways("a", ""))
		n2.Notify(context.Background(), snapWith(mrIn(tc.state, "b")))
		lanes := n2.Attention()
		if len(lanes) != 1 {
			t.Fatalf("state %s: attention lanes = %d, want 1", tc.state, len(lanes))
		}
		got := lanes[0]
		if !allowed[got.Lane] {
			t.Errorf("state %s: lane %q outside closed contract {merge, conflict}", tc.state, got.Lane)
		}
		if got.Lane != tc.wantLane {
			t.Errorf("state %s: lane = %q, want %q", tc.state, got.Lane, tc.wantLane)
		}
		if got.Severity != tc.wantSev {
			t.Errorf("state %s: severity = %q, want %q", tc.state, got.Severity, tc.wantSev)
		}
	}
}

func TestNotifier_AttentionClearsWhenHealthy(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(sender, ownerAlways("a", ""))

	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "b")))
	if got := len(n.Attention()); got != 1 {
		t.Fatalf("attention after conflict = %d, want 1", got)
	}
	// MR recovers to ok: attention lane must clear.
	n.Notify(context.Background(), snapWith(mrIn(StateOK, "b")))
	if got := len(n.Attention()); got != 0 {
		t.Fatalf("attention after recovery = %d, want 0", got)
	}
	// MR disappears entirely (retention expired): still empty, no leak.
	n.Notify(context.Background(), Snapshot{MergeRequests: []MergeRequest{}})
	if got := len(n.Attention()); got != 0 {
		t.Fatalf("attention after MR gone = %d, want 0", got)
	}
}

// TestNotifier_MergedClearsAttention: a retained merged MR is not a stall, so it
// must clear the lane rather than hold (or raise) one. Before merged MRs were
// retained the entry simply vanished and prune() did this; the retained entry
// must reach the same end state.
func TestNotifier_MergedClearsAttention(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(sender, ownerAlways("a", ""))

	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "b")))
	if got := len(n.Attention()); got != 1 {
		t.Fatalf("attention after conflict = %d, want 1", got)
	}
	n.Notify(context.Background(), snapWith(mrIn(StateMerged, "b")))
	if got := len(n.Attention()); got != 0 {
		t.Errorf("attention while the merged MR is retained = %d, want 0", got)
	}
	if got := sender.count(); got != 1 {
		t.Errorf("inbox sends = %d, want 1 (the conflict only)", got)
	}
}

func TestNotifier_DisabledIsNoop(t *testing.T) {
	sender := &fakeSender{}
	n := NewNotifier(sender, ownerAlways("a", ""), NotifierOptions{Enabled: false})
	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "b")))
	if sender.count() != 0 {
		t.Error("disabled notifier must not send inbox messages")
	}
	if len(n.Attention()) != 0 {
		t.Error("disabled notifier must not record attention lanes")
	}
}

func TestNotifier_NilAndPruneSafety(t *testing.T) {
	var n *Notifier
	// nil receiver: no panic, empty attention.
	n.Notify(context.Background(), snapWith(mrIn(StateConflict, "b")))
	if got := len(n.Attention()); got != 0 {
		t.Fatalf("nil notifier Attention = %d, want 0", got)
	}

	// nil sender + nil resolver: attention-only, no panic.
	live := newTestNotifier(nil, nil)
	live.Notify(context.Background(), snapWith(mrIn(StateConflict, "b")))
	if got := len(live.Attention()); got != 1 {
		t.Fatalf("attention with nil sender/resolver = %d, want 1", got)
	}
}

func TestNotifyEnabledFromEnv(t *testing.T) {
	cases := map[string]bool{
		"":         true, // default ON
		"on":       true,
		"1":        true,
		"true":     true,
		"whatever": true,
		"off":      false,
		"0":        false,
		"false":    false,
		"no":       false,
		"disabled": false,
	}
	for val, want := range cases {
		t.Setenv(EnvNotify, val)
		if got := NotifyEnabledFromEnv(); got != want {
			t.Errorf("NotifyEnabledFromEnv(%q) = %v, want %v", val, got, want)
		}
	}
}
