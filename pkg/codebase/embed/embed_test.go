package embed

import (
	"context"
	"math"
	"testing"
)

func TestDummyEmbedder(t *testing.T) {
	ctx := context.Background()

	t.Run("default dimension", func(t *testing.T) {
		e := NewDummyEmbedder(0)
		if e.Dimension() != 1 {
			t.Errorf("expected dimension 1, got %d", e.Dimension())
		}
	})

	t.Run("custom dimension", func(t *testing.T) {
		e := NewDummyEmbedder(384)
		if e.Dimension() != 384 {
			t.Errorf("expected dimension 384, got %d", e.Dimension())
		}
	})

	t.Run("embed query", func(t *testing.T) {
		e := NewDummyEmbedder(128)
		vec, err := e.EmbedQuery(ctx, "test query")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vec) != 128 {
			t.Errorf("expected vector length 128, got %d", len(vec))
		}
		if vec[0] != 1 {
			t.Errorf("expected first element to be 1, got %f", vec[0])
		}
	})

	t.Run("embed documents", func(t *testing.T) {
		e := NewDummyEmbedder(64)
		texts := []string{"doc 1", "doc 2", "doc 3"}
		vecs, err := e.EmbedDocuments(ctx, texts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vecs) != 3 {
			t.Errorf("expected 3 vectors, got %d", len(vecs))
		}
		for i, vec := range vecs {
			if len(vec) != 64 {
				t.Errorf("vector %d: expected length 64, got %d", i, len(vec))
			}
		}
	})

	t.Run("name and model", func(t *testing.T) {
		e := NewDummyEmbedder(128)
		if e.Name() != "dummy" {
			t.Errorf("expected name 'dummy', got %q", e.Name())
		}
		if e.Model() != "none" {
			t.Errorf("expected model 'none', got %q", e.Model())
		}
	})

	t.Run("empty documents", func(t *testing.T) {
		e := NewDummyEmbedder(128)
		vecs, err := e.EmbedDocuments(ctx, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vecs) != 0 {
			t.Errorf("expected 0 vectors, got %d", len(vecs))
		}
	})
}

func TestEmbedderInterface(t *testing.T) {
	var _ Embedder = (*DummyEmbedder)(nil)
	var _ Embedder = (*MorphClient)(nil)
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
		ok   bool
	}{
		{name: "identical", a: []float64{1, 0}, b: []float64{1, 0}, want: 1, ok: true},
		{name: "orthogonal", a: []float64{1, 0}, b: []float64{0, 1}, want: 0, ok: true},
		{name: "opposite", a: []float64{1, 0}, b: []float64{-1, 0}, want: -1, ok: true},
		{name: "large finite", a: []float64{math.MaxFloat64}, b: []float64{math.MaxFloat64}, want: 1, ok: true},
		{name: "small finite", a: []float64{math.SmallestNonzeroFloat64}, b: []float64{math.SmallestNonzeroFloat64}, want: 1, ok: true},
		{name: "empty", ok: false},
		{name: "dimension mismatch", a: []float64{1}, b: []float64{1, 0}, ok: false},
		{name: "zero vector", a: []float64{0, 0}, b: []float64{1, 0}, ok: false},
		{name: "nan", a: []float64{math.NaN()}, b: []float64{1}, ok: false},
		{name: "positive infinity", a: []float64{math.Inf(1)}, b: []float64{1}, ok: false},
		{name: "negative infinity", a: []float64{1}, b: []float64{math.Inf(-1)}, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CosineSimilarity(tc.a, tc.b)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("CosineSimilarity() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
