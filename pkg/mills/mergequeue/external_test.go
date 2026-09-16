package mergequeue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestExternalEnqueuer_DurableIdempotencyAndProvenance(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/mills.db"
	st, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	c := ExternalCandidate{Producer: "mcp_gitlab", IdempotencyKey: "ship-1", Project: "services/loom", MRIID: 42, SourceBranch: "feat/x", TargetBranch: "main", ObservedSHA: "abc"}
	e := &ExternalEnqueuer{Store: st, Enabled: func() bool { return true }, MaxDepth: func() int { return 10 }}
	first, err := e.Enqueue(ctx, c)
	if err != nil || first.Outcome != "enqueued" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if first.Entry.Detail["producer"] != "mcp_gitlab" {
		t.Fatalf("provenance = %#v", first.Entry.Detail)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e.Store = st
	second, err := e.Enqueue(ctx, c)
	if err != nil || second.Outcome != "duplicate" {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

// TestExternalEnqueuer_RefusesHeadEvictedForRebaseConflict pins the admission
// fence: once the queue has evicted an MR head for a rebase conflict, offering
// the same head again (same or a fresh idempotency key) answers `conflicted`
// and enqueues nothing; a new head, or a prior eviction for any other reason,
// still admits the candidate.
func TestExternalEnqueuer_RefusesHeadEvictedForRebaseConflict(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/mills.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &ExternalEnqueuer{Store: st, Enabled: func() bool { return true }, MaxDepth: func() int { return 10 }}
	c := ExternalCandidate{Producer: "mrwatch_shepherd", IdempotencyKey: "loom!1789:aead08f4", Project: "services/loom-core", MRIID: 1789, SourceBranch: "feat/canary", TargetBranch: "main", ObservedSHA: "aead08f4"}

	first, err := e.Enqueue(ctx, c)
	if err != nil || first.Outcome != "enqueued" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if _, err := st.MergeQueue.MarkEvicted(ctx, first.Entry.ID, store.MergeQueueEvictRebaseConflict, map[string]any{"detail": "rebase failed"}); err != nil {
		t.Fatalf("evict: %v", err)
	}

	// Same key after the eviction used to re-enqueue (created=true); now fenced.
	again, err := e.Enqueue(ctx, c)
	if err != nil || again.Outcome != "conflicted" {
		t.Fatalf("same head after rebase_conflict = %+v, %v; want outcome=conflicted", again, err)
	}
	if again.Entry == nil || again.Entry.ID != first.Entry.ID || again.State != store.MergeQueueEvicted {
		t.Fatalf("conflicted result should carry the prior eviction: %+v", again)
	}
	c.IdempotencyKey = "loom!1789:aead08f4:retry"
	fresh, err := e.Enqueue(ctx, c)
	if err != nil || fresh.Outcome != "conflicted" {
		t.Fatalf("fresh key, same head = %+v, %v; want outcome=conflicted", fresh, err)
	}
	active, err := st.MergeQueue.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("fenced enqueues must not create active entries, got %d", len(active))
	}

	// A new head is a new verdict.
	c.IdempotencyKey = "loom!1789:b0b0b0b0"
	c.ObservedSHA = "b0b0b0b0"
	moved, err := e.Enqueue(ctx, c)
	if err != nil || moved.Outcome != "enqueued" {
		t.Fatalf("new head = %+v, %v; want outcome=enqueued", moved, err)
	}

	// An eviction for any other reason (here ci_red) does not fence the head:
	// CI can be retried without the head changing.
	if _, err := st.MergeQueue.MarkEvicted(ctx, moved.Entry.ID, store.MergeQueueEvictCIRed, nil); err != nil {
		t.Fatalf("evict ci_red: %v", err)
	}
	c.IdempotencyKey = "loom!1789:b0b0b0b0:again"
	readmitted, err := e.Enqueue(ctx, c)
	if err != nil || readmitted.Outcome != "enqueued" {
		t.Fatalf("same head after ci_red = %+v, %v; want outcome=enqueued", readmitted, err)
	}
}

func TestExternalEnqueuer_Outcomes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/mills.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := ExternalCandidate{Producer: "mrwatch_shepherd", IdempotencyKey: "one", Project: "p", MRIID: 1, SourceBranch: "x", TargetBranch: "main", ObservedSHA: "sha"}
	disabled, err := (&ExternalEnqueuer{Store: st, Enabled: func() bool { return false }}).Enqueue(ctx, c)
	if err != nil || disabled.Outcome != "disabled" {
		t.Fatalf("disabled = %+v, %v", disabled, err)
	}
	e := &ExternalEnqueuer{Store: st, Enabled: func() bool { return true }, MaxDepth: func() int { return 1 }}
	if _, err := e.Enqueue(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.IdempotencyKey = "two"
	c.MRIID = 2
	full, err := e.Enqueue(ctx, c)
	if err != nil || full.Outcome != "full" {
		t.Fatalf("full = %+v, %v", full, err)
	}
}

func TestExternalEnqueuer_ValidatesCandidate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/mills.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = (&ExternalEnqueuer{Store: st, Enabled: func() bool { return true }}).Enqueue(ctx, ExternalCandidate{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

// The queue already holding an MR (its own run or an earlier external
// candidate) refuses a second candidate for it instead of creating a parallel
// row that would be driven — and speculated — as a separate candidate.
func TestExternalEnqueuer_RefusesSecondCandidateForActiveMR(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/mills.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &ExternalEnqueuer{Store: st, Enabled: func() bool { return true }, MaxDepth: func() int { return 10 }}
	c := ExternalCandidate{Producer: "mrwatch_shepherd", IdempotencyKey: "loom!1918:a73991d7", Project: "services/loom-core", MRIID: 1918, SourceBranch: "feat/canary", TargetBranch: "main", ObservedSHA: "a73991d7"}
	first, err := e.Enqueue(ctx, c)
	if err != nil || first.Outcome != "enqueued" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	c.IdempotencyKey = "loom!1918:a73991d7:again"
	again, err := e.Enqueue(ctx, c)
	if err != nil || again.Outcome != "active" || again.Entry == nil || again.Entry.ID != first.Entry.ID {
		t.Fatalf("second candidate for an active MR = %+v, %v; want outcome=active pointing at the live entry", again, err)
	}
	if active, _ := st.MergeQueue.ListActive(ctx); len(active) != 1 {
		t.Fatalf("active entries = %d, want 1", len(active))
	}
	// Settled rows do not block a fresh candidate.
	if _, err := st.MergeQueue.MarkEvicted(ctx, first.Entry.ID, store.MergeQueueEvictCITimeout, map[string]any{"detail": "timeout"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := e.Enqueue(ctx, c)
	if err != nil || fresh.Outcome != "enqueued" {
		t.Fatalf("after settle = %+v, %v; want enqueued", fresh, err)
	}
}

func TestExternalAdoptionBranchOwner(t *testing.T) {
	for _, tc := range []struct {
		name, project, branch string
		state                 store.BacklogState
		want                  bool
	}{
		{"match", "services/flexinfer", "feat/BL-branch/slice", store.BacklogEscalated, true},
		{"restart", "services/flexinfer", "feat/BL-branch/slice", store.BacklogEscalated, true},
		{"fix prefix", "services/flexinfer", "fix/BL-branch/slice", store.BacklogEscalated, true},
		{"stale", "services/flexinfer", "feat/BL-branch/slice", store.BacklogEscalated, false},
		{"different MR", "services/flexinfer", "feat/BL-branch/slice", store.BacklogEscalated, false},
		{"done run", "services/flexinfer", "feat/BL-branch/slice", store.BacklogEscalated, false},
		{"foreign", "services/other", "feat/BL-branch/slice", store.BacklogEscalated, false},
		{"unknown", "", "feat/BL-branch/slice", store.BacklogEscalated, false},
		{"near branch", "services/flexinfer", "feat/BL-branch/slice-extra", store.BacklogEscalated, false},
		{"queued", "services/flexinfer", "feat/BL-branch/slice", store.BacklogQueued, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newQueueStore(t)
			item := &store.BacklogItem{ID: "BL-branch", Title: "branch owner", State: store.BacklogEscalated, Priority: store.P2}
			if err := st.Backlog.Put(ctx, item); err != nil {
				t.Fatal(err)
			}
			run := &store.PipelineRun{ID: "PIPE-branch", BacklogID: item.ID, Template: "mills-default-pipeline", State: store.PipelineEscalated, StartedAt: time.Now().Add(-time.Hour)}
			run.MRIID = nil
			if tc.name == "stale" {
				run.StartedAt = time.Now().Add(time.Hour)
			}
			if tc.name == "different MR" {
				iid := int64(999)
				run.MRIID = &iid
			}
			if tc.name == "done run" {
				run.State = store.PipelineDone
			}
			if err := st.Pipeline.PutRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if tc.project != "" {
				success := store.StageOutcomeSuccess
				if err := st.Pipeline.PutStage(ctx, &store.StageResult{PipelineRunID: run.ID, Stage: "ci_watch", Attempt: 1, StartedAt: run.StartedAt, Outcome: &success, Artifacts: map[string]any{"ci_project": tc.project}}); err != nil {
					t.Fatal(err)
				}
			}
			item.Slices = []store.Slice{{Name: "slice"}}
			if tc.name == "fix prefix" {
				item.Labels = []string{"type/fix"}
			}
			if err := st.Backlog.Put(ctx, item); err != nil {
				t.Fatal(err)
			}
			if tc.state != item.State {
				if _, err := st.Backlog.TransitionState(ctx, item.ID, item.ClaimVersion, item.State, tc.state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.Backlog.DeferEscalationRecheck(ctx, item.ID, 0, time.Now()); err != nil {
				t.Fatal(err)
			}
			result, err := (&ExternalEnqueuer{Store: st}).Enqueue(ctx, ExternalCandidate{Producer: "mrwatch_shepherd", IdempotencyKey: "branch", Project: "services/flexinfer", MRIID: 1004, SourceBranch: tc.branch, TargetBranch: "main", ObservedSHA: "head"})
			if err != nil {
				t.Fatal(err)
			}
			p := newProcessor(st, &fakeForge{})
			if tc.name == "restart" {
				if _, err := st.MergeQueue.MarkMerged(ctx, result.Entry.ID, store.MergeQueueQueued, "merged-sha"); err != nil {
					t.Fatal(err)
				}
			} else if err := p.settleMerged(ctx, result.Entry, store.MergeQueueQueued, "merged-sha"); err != nil {
				t.Fatal(err)
			}
			newProcessor(st, &fakeForge{}).settleRecentExternalAdoptions(ctx)
			p.settleRecentExternalAdoptions(ctx)
			got, err := st.Backlog.Get(ctx, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (got.State == store.BacklogMerged) != tc.want {
				t.Fatalf("state=%s want merged=%v", got.State, tc.want)
			}
			events, err := st.Events.ListBySubject(ctx, "backlog", item.ID, 50)
			if err != nil {
				t.Fatal(err)
			}
			closures := 0
			for _, event := range events {
				if event.Kind == "backlog.settled" {
					closures++
					if event.Payload["merged_sha"] != "merged-sha" || event.Payload["project"] != "services/flexinfer" || event.Payload["mr_iid"] != float64(1004) {
						t.Fatalf("evidence=%v", event.Payload)
					}
				}
			}
			if tc.want {
				if closures != 1 {
					t.Fatalf("closures=%d", closures)
				}
				if _, err := st.Backlog.EscalationRecheck(ctx, item.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("cooldown not cleared: %v", err)
				}
			} else if closures != 0 {
				t.Fatalf("unexpected closures=%d", closures)
			}
		})
	}
}
