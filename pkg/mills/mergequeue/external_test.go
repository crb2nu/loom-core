package mergequeue

import (
	"context"
	"testing"

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
