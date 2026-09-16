package textsim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func semanticScoreCounterValue(t *testing.T, available bool) float64 {
	t.Helper()
	return testutil.ToFloat64(MergedWorkSemanticScoresTotal.WithLabelValues(fmt.Sprint(available)))
}

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
	before := semanticScoreCounterValue(t, true)
	backend := &fakeDocumentEmbedder{vectors: [][]float64{{1, 0}, {0, 1}}}
	scorer := NewSemanticScorer(backend)
	got := scorer.Score(context.Background(), "feat(mills): Add semantic grounding", "Add lexical grounding")

	wantLexical := WorkTitleJaccard("feat(mills): Add semantic grounding", "Add lexical grounding")
	wantCombined := math.Max(wantLexical, 0.5)
	if got.Lexical != wantLexical || got.Semantic != 0.5 || got.Combined != wantCombined || !got.SemanticAvailable || got.Fallback {
		t.Fatalf("Score() = %+v, want lexical=%v semantic=.5 combined=%v", got, wantLexical, wantCombined)
	}
	if len(backend.texts) != 2 || backend.texts[0] != "Add semantic grounding" {
		t.Fatalf("embedded texts = %q, want normalized titles", backend.texts)
	}
	if backend.calls != 1 {
		t.Fatalf("EmbedDocuments calls = %d, want exactly one batch", backend.calls)
	}
	if delta := semanticScoreCounterValue(t, true) - before; delta != 1 {
		t.Fatalf("available=true counter delta = %v, want 1", delta)
	}
}

func TestSemanticScorerUsesMaximumSignal(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		vectors [][]float64
		want    float64
	}{
		{name: "lexical wins", a: "same work", b: "same work", vectors: [][]float64{{1, 0}, {-1, 0}}, want: 1},
		{name: "semantic wins", a: "alpha", b: "omega", vectors: [][]float64{{1, 0}, {1, 0}}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewSemanticScorer(&fakeDocumentEmbedder{vectors: tc.vectors}).Score(context.Background(), tc.a, tc.b)
			if got.Combined != tc.want {
				t.Fatalf("Combined = %v, want %v (lexical=%v semantic=%v)", got.Combined, tc.want, got.Lexical, got.Semantic)
			}
		})
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
		{name: "infinite vector", scorer: NewSemanticScorer(&fakeDocumentEmbedder{vectors: [][]float64{{math.Inf(1)}, {1}}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := semanticScoreCounterValue(t, false)
			got := tc.scorer.Score(context.Background(), "Add semantic grounding", "Add lexical grounding")
			want := WorkTitleJaccard("Add semantic grounding", "Add lexical grounding")
			if got.Combined != want || got.Lexical != want || got.Semantic != 0 || got.SemanticAvailable || !got.Fallback {
				t.Fatalf("fallback = %+v, want lexical-only %v", got, want)
			}
			if delta := semanticScoreCounterValue(t, false) - before; delta != 1 {
				t.Fatalf("available=false counter delta = %v, want 1", delta)
			}
		})
	}
}

func TestSemanticScorerBoundsBackendCall(t *testing.T) {
	before := semanticScoreCounterValue(t, false)
	backend := &fakeDocumentEmbedder{waitForContext: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	got := NewSemanticScorer(backend).Score(ctx, "Add semantic grounding", "Add lexical grounding")
	want := WorkTitleJaccard("Add semantic grounding", "Add lexical grounding")
	if got.Combined != want || got.SemanticAvailable || !got.Fallback {
		t.Fatalf("timeout fallback = %+v, want lexical-only %v", got, want)
	}
	if backend.calls != 1 {
		t.Fatalf("EmbedDocuments calls = %d, want one", backend.calls)
	}
	if delta := semanticScoreCounterValue(t, false) - before; delta != 1 {
		t.Fatalf("available=false counter delta = %v, want 1", delta)
	}
}

func TestSemanticScorerEmptyTitleDoesNotCallBackend(t *testing.T) {
	before := semanticScoreCounterValue(t, false)
	backend := &fakeDocumentEmbedder{vectors: [][]float64{{1}, {1}}}
	got := NewSemanticScorer(backend).Score(context.Background(), "", "Add grounding")
	if got.Combined != 0 || got.SemanticAvailable || !got.Fallback || backend.texts != nil {
		t.Fatalf("Score() = %+v, embedded=%q; want zero lexical fallback without backend call", got, backend.texts)
	}
	if delta := semanticScoreCounterValue(t, false) - before; delta != 1 {
		t.Fatalf("available=false counter delta = %v, want 1", delta)
	}
}

var _ Scorer = (*SemanticScorer)(nil)

type fixedScorer Similarity

func (f fixedScorer) Score(context.Context, string, string) Similarity { return Similarity(f) }

func TestCompareShadowNeverChangesLexicalGate(t *testing.T) {
	tests := []struct {
		name             string
		a, b             string
		similarity       Similarity
		wantLexicalBand  Band
		wantCombinedBand Band
		wantGate         bool
		wantDisagrees    bool
	}{
		{
			name: "agreement", a: "Add semantic title grounding", b: "Add semantic title grounding",
			similarity:      Similarity{Semantic: 1, Combined: 1, SemanticAvailable: true},
			wantLexicalBand: BandHard, wantCombinedBand: BandHard, wantGate: true,
		},
		{
			name: "semantic higher", a: "Add semantic title grounding", b: "Repair webhook retries",
			similarity:      Similarity{Semantic: 1, Combined: .9, SemanticAvailable: true},
			wantLexicalBand: BandNone, wantCombinedBand: BandHard, wantGate: false, wantDisagrees: true,
		},
		{
			name: "semantic lower", a: "Add semantic title grounding", b: "Add semantic title grounding",
			similarity:      Similarity{Semantic: 0, Combined: .1, SemanticAvailable: true},
			wantLexicalBand: BandHard, wantCombinedBand: BandNone, wantGate: true, wantDisagrees: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareShadow(context.Background(), fixedScorer(tc.similarity), tc.a, tc.b, .7, true)
			if got.LexicalBand != tc.wantLexicalBand || got.CombinedBand != tc.wantCombinedBand || got.Gate != tc.wantGate || got.Disagrees != tc.wantDisagrees {
				t.Fatalf("CompareShadow() = %+v", got)
			}
			wantGate := WorkTitleJaccard(tc.a, tc.b) >= .7
			if got.Gate != wantGate {
				t.Fatalf("semantic changed lexical gate: got %v want %v", got.Gate, wantGate)
			}
		})
	}
}

func TestCompareShadowUnavailableFailsOpen(t *testing.T) {
	got := CompareShadow(context.Background(), fixedScorer(Similarity{}), "same title", "same title", .7, true)
	if !got.Gate || got.Combined != got.Lexical || got.CombinedBand != BandUnavailable || got.Disagrees {
		t.Fatalf("CompareShadow() = %+v, want unchanged lexical gate", got)
	}
}
