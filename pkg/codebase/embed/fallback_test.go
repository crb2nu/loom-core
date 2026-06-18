package embed

import (
	"context"
	"errors"
	"testing"
)

func TestFallbackEmbedder_EmbedDocuments_PrimarySucceeds(t *testing.T) {
	primary := &fakeEmbedder{}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)

	if _, err := f.EmbedDocuments(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secondary.callCount() != 0 {
		t.Fatalf("secondary must not be called when primary succeeds, got %d", secondary.callCount())
	}
}

func TestFallbackEmbedder_EmbedDocuments_FallsBackOnPrimaryError(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary 522")}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)

	vecs, err := f.EmbedDocuments(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors from secondary, got %d", len(vecs))
	}
	if secondary.callCount() != 1 {
		t.Fatalf("expected secondary to be called once, got %d", secondary.callCount())
	}
}

func TestFallbackEmbedder_EmbedQuery_NeverFallsBack(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("primary down")}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)

	// Query path must surface the primary error and never touch the secondary,
	// so the caller degrades to keyword search instead of cross-space vectors.
	if _, err := f.EmbedQuery(context.Background(), "q"); err == nil {
		t.Fatal("expected primary error to propagate on EmbedQuery")
	}
	if secondary.callCount() != 0 {
		t.Fatalf("secondary must never serve queries, got %d calls", secondary.callCount())
	}
}

func TestFallbackEmbedder_NilSecondary_PropagatesPrimaryError(t *testing.T) {
	primary := &fakeEmbedder{err: errors.New("down")}
	f := NewFallbackEmbedder(primary, nil)

	if _, err := f.EmbedDocuments(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected primary error with nil secondary")
	}
}

func TestFallbackEmbedder_NameModelFromPrimary(t *testing.T) {
	primary := &fakeEmbedder{}
	secondary := &fakeEmbedder{}
	f := NewFallbackEmbedder(primary, secondary)
	if f.Name() != "fake" || f.Model() != "fake-model" {
		t.Fatalf("expected primary Name/Model, got %s/%s", f.Name(), f.Model())
	}
}
