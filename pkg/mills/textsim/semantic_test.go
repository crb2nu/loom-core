package textsim

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type fakeDocumentEmbedder struct {
	vectors        [][]float64
	err            error
	texts          []string
	calls          int
	waitForContext bool
}

func (f *fakeDocumentEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	f.calls++
	f.texts = append([]string(nil), texts...)
	if f.waitForContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.vectors, f.err
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
		ok   bool
	}{
		{name: "identical", a: []float64{1, 0}, b: []float64{1, 0}, want: 1, ok: true},
		{name: "orthogonal", a: []float64{1, 0}, b: []float64{0, 1}, want: 0.5, ok: true},
		{name: "opposite", a: []float64{1, 0}, b: []float64{-1, 0}, want: 0, ok: true},
		{name: "large finite", a: []float64{math.MaxFloat64}, b: []float64{math.MaxFloat64}, want: 1, ok: true},
		{name: "small finite", a: []float64{math.SmallestNonzeroFloat64}, b: []float64{math.SmallestNonzeroFloat64}, want: 1, ok: true},
		{name: "empty", ok: false},
		{name: "dimension mismatch", a: []float64{1}, b: []float64{1, 0}, ok: false},
		{name: "zero norm", a: []float64{0, 0}, b: []float64{1, 0}, ok: false},
		{name: "nan", a: []float64{math.NaN()}, b: []float64{1}, ok: false},
		{name: "infinite", a: []float64{math.Inf(1)}, b: []float64{1}, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CosineSimilarity(tc.a, tc.b)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("CosineSimilarity() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSemanticScorerCombinesSignals(t *testing.T) {
	backend := &fakeDocumentEmbedder{vectors: [][]float64{{1, 0}, {0, 1}}}
	scorer := NewSemanticScorer(backend)
	got := scorer.Score(context.Background(), "feat(mills): Add semantic grounding", "Add lexical grounding")

	wantLexical := WorkTitleJaccard("feat(mills): Add semantic grounding", "Add lexical grounding")
	if got.Lexical != wantLexical || got.Semantic != 0.5 || got.Combined != (wantLexical+0.5)/2 || !got.SemanticAvailable {
		t.Fatalf("Score() = %+v, want lexical=%v semantic=.5 combined=%v", got, wantLexical, (wantLexical+0.5)/2)
	}
	if len(backend.texts) != 2 || backend.texts[0] != "Add semantic grounding" {
		t.Fatalf("embedded texts = %q, want normalized titles", backend.texts)
	}
	if backend.calls != 1 {
		t.Fatalf("EmbedDocuments calls = %d, want exactly one batch", backend.calls)
	}
}

func TestSemanticScorerFallsBackToLexical(t *testing.T) {
	tests := []struct {
		name   string
		scorer *SemanticScorer
	}{
		{name: "nil scorer"},
		{name: "nil backend", scorer: NewSemanticScorer(nil)},
		{name: "backend error", scorer: NewSemanticScorer(&fakeDocumentEmbedder{err: errors.New("unavailable")})},
		{name: "malformed response", scorer: NewSemanticScorer(&fakeDocumentEmbedder{vectors: [][]float64{{1, 0}}})},
		{name: "zero vector", scorer: NewSemanticScorer(&fakeDocumentEmbedder{vectors: [][]float64{{0, 0}, {1, 0}}})},
		{name: "dimension mismatch", scorer: NewSemanticScorer(&fakeDocumentEmbedder{vectors: [][]float64{{1}, {1, 0}}})},
		{name: "nan vector", scorer: NewSemanticScorer(&fakeDocumentEmbedder{vectors: [][]float64{{math.NaN()}, {1}}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scorer.Score(context.Background(), "Add semantic grounding", "Add lexical grounding")
			want := WorkTitleJaccard("Add semantic grounding", "Add lexical grounding")
			if got.Combined != want || got.Lexical != want || got.Semantic != 0 || got.SemanticAvailable {
				t.Fatalf("fallback = %+v, want lexical-only %v", got, want)
			}
		})
	}
}

func TestSemanticScorerBoundsBackendCall(t *testing.T) {
	backend := &fakeDocumentEmbedder{waitForContext: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	got := NewSemanticScorer(backend).Score(ctx, "Add semantic grounding", "Add lexical grounding")
	want := WorkTitleJaccard("Add semantic grounding", "Add lexical grounding")
	if got.Combined != want || got.SemanticAvailable {
		t.Fatalf("timeout fallback = %+v, want lexical-only %v", got, want)
	}
	if backend.calls != 1 {
		t.Fatalf("EmbedDocuments calls = %d, want one", backend.calls)
	}
}

func TestSemanticScorerEmptyTitleDoesNotCallBackend(t *testing.T) {
	backend := &fakeDocumentEmbedder{vectors: [][]float64{{1}, {1}}}
	got := NewSemanticScorer(backend).Score(context.Background(), "", "Add grounding")
	if got.Combined != 0 || got.SemanticAvailable || backend.texts != nil {
		t.Fatalf("Score() = %+v, embedded=%q; want zero lexical fallback without backend call", got, backend.texts)
	}
}

var _ Scorer = (*SemanticScorer)(nil)
