package mergequeue

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/policy"
)

func TestMainRedExternalHoldLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	rec := pipeline.DefaultBranchCIRecorder{Store: st, Policy: policy.MainRedExternalHoldPolicy{HoldMinutes: 30}}
	check := func(id, class string, at time.Time) *store.MainRedExternalHold {
		t.Helper()
		h, e := rec.Record(ctx, "services/loom-core", "main", id, class, at)
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	if h := check("1", store.ExternalDependencyIncident, start); h != nil && h.Active(start) {
		t.Fatal("one incident activated hold")
	}
	if h := check("2", "internal_failure", start.Add(time.Minute)); h != nil && h.Active(start.Add(time.Minute)) {
		t.Fatal("internal failure activated hold")
	}
	check("3", store.ExternalDependencyIncident, start.Add(2*time.Minute))
	h := check("4", store.ExternalDependencyIncident, start.Add(3*time.Minute))
	if h == nil || !h.Active(start.Add(3*time.Minute)) {
		t.Fatalf("hold=%+v", h)
	}
	now := h.ExpiresAt.Add(-time.Nanosecond)
	calls := 0
	c := &MainRedExternalController{Store: st, Now: func() time.Time { return now }, Escalate: func(context.Context, store.MainRedExternalHold) error { calls++; return nil }}
	if reason, err := c.Reconcile(ctx, "services/loom-core", "main"); err != nil || reason != MainRedExternalReason {
		t.Fatalf("before expiry reason=%q err=%v", reason, err)
	}
	now = h.ExpiresAt
	if reason, err := c.Reconcile(ctx, "services/loom-core", "main"); err != nil || reason != "" {
		t.Fatalf("at expiry reason=%q err=%v", reason, err)
	}
	if _, err := c.Reconcile(ctx, "services/loom-core", "main"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("escalations=%d", calls)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c.Store = st
	if _, err := c.Reconcile(ctx, "services/loom-core", "main"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("restart escalations=%d", calls)
	}
	check = func(id, class string, at time.Time) *store.MainRedExternalHold {
		h, e := st.RecordDefaultBranchPipeline(ctx, "services/loom-core", "main", id, class, at, 30*time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	h = check("5", "success", now.Add(time.Minute))
	if h == nil || h.ClearedAt == nil {
		t.Fatalf("recovery hold=%+v", h)
	}
}

func TestMainRedExternalPolicyClamps(t *testing.T) {
	if got := (policy.MainRedExternalHoldPolicy{}).Duration(); got != policy.DefaultMainRedExternalHold {
		t.Fatalf("default=%v", got)
	}
	if got := (policy.MainRedExternalHoldPolicy{HoldMinutes: int(^uint(0) >> 1)}).Duration(); got != policy.MaxMainRedExternalHold {
		t.Fatalf("max=%v", got)
	}
}

func TestMainRedExternalNegativeAndReplay(t *testing.T) {
	ctx := context.Background()
	st := newQueueStore(t)
	start := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	for _, class := range []string{"", "success", "internal_failure"} {
		project := "p-" + class
		for i, c := range []string{store.ExternalDependencyIncident, class, store.ExternalDependencyIncident} {
			h, err := st.RecordDefaultBranchPipeline(ctx, project, "main", fmt.Sprint(i+1), c, start.Add(time.Duration(i)*time.Minute), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if h != nil && h.Active(start.Add(2*time.Minute)) {
				t.Fatalf("mixed %q activated hold: %+v", class, h)
			}
		}
	}
	rec := pipeline.DefaultBranchCIRecorder{Store: st, PolicyFn: func() *mills.Policy {
		return &mills.Policy{MergeQueue: mills.MergeQueuePolicy{MainRedExternalHoldMinutes: 3}}
	}}
	for _, id := range []string{"9", "10"} {
		if _, err := rec.Record(ctx, "p", "main", id, store.ExternalDependencyIncident, start); err != nil {
			t.Fatal(err)
		}
	}
	h, err := st.MainRedExternalHold(ctx, "p", "main")
	if err != nil || h == nil || !h.ExpiresAt.Equal(start.Add(3*time.Minute)) {
		t.Fatalf("policy not applied: %+v %v", h, err)
	}
	if _, err := rec.Record(ctx, "p", "main", "11", "success", start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Re-delivering the older pipeline must not reorder it above recovery.
	h, err = rec.Record(ctx, "p", "main", "10", store.ExternalDependencyIncident, start.Add(2*time.Minute))
	if err != nil || h == nil || h.ClearedAt == nil {
		t.Fatalf("replay reactivated hold: %+v %v", h, err)
	}
}

func TestProcessorMainRedExternalDefersAndRecovers(t *testing.T) {
	ctx := context.Background()
	st := newQueueStore(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	e := enqueue(t, st, seedRun(t, st, "hold"), "sha")
	f := &fakeForge{snapshot: MRSnapshot{SHA: "sha", State: "opened", BaseSHA: "base"}, tip: "base"}
	p := newProcessor(st, f)
	p.Now = func() time.Time { return now }
	for _, id := range []string{"1", "2"} {
		if _, err := st.RecordDefaultBranchPipeline(ctx, e.Project, e.TargetBranch, id, store.ExternalDependencyIncident, now, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	e = drive(t, p, st, e.PipelineRunID)
	if len(f.calls) != 0 || e.State != store.MergeQueueQueued || e.Detail["defer_reason"] != MainRedExternalReason {
		t.Fatalf("not deferred: %+v calls=%v", e, f.calls)
	}
	if _, err := st.RecordDefaultBranchPipeline(ctx, e.Project, e.TargetBranch, "3", "success", now.Add(time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	e = drive(t, p, st, e.PipelineRunID)
	if len(f.calls) == 0 || e.Detail["defer_reason"] != nil {
		t.Fatalf("did not recover: %+v calls=%v", e, f.calls)
	}
}

func TestMainRedExternalEscalationPerEpisode(t *testing.T) {
	ctx := context.Background()
	st := newQueueStore(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	c := &MainRedExternalController{Store: st, Now: func() time.Time { return now }}
	for episode := 0; episode < 2; episode++ {
		for i := 0; i < 2; i++ {
			if _, err := st.RecordDefaultBranchPipeline(ctx, "p", "main", fmt.Sprint(episode*3+i), store.ExternalDependencyIncident, now, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(time.Minute + time.Nanosecond)
		for i := 0; i < 3; i++ {
			if _, err := c.Reconcile(ctx, "p", "main"); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='mergequeue.main_red_external.expired'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != episode+1 {
			t.Fatalf("episode %d escalations=%d", episode, count)
		}
		if _, err := st.RecordDefaultBranchPipeline(ctx, "p", "main", fmt.Sprint(episode*3+2), "success", now, time.Minute); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
}
