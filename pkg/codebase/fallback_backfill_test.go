package codebase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/codebase/qdrant"
	"github.com/crb2nu/loom/pkg/httpclient"
)

func TestBackfillFallbackVectors_BoundsOversizedPage(t *testing.T) {
	t.Parallel()
	var scrollLimit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/scroll"):
			var body struct {
				Limit int `json:"limit"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode scroll request: %v", err)
			}
			scrollLimit = body.Limit
			_, _ = w.Write([]byte(`{"result":{"points":[]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/test"):
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":3,"distance":"Cosine"}}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	svc := &Service{qdrant: qdrant.NewClient(httpclient.NewDefault(), srv.URL, "", "test", "Cosine"), embed: backfillEmbedder{}}

	if _, err := svc.BackfillFallbackVectors(context.Background(), 10_000, nil); err != nil {
		t.Fatal(err)
	}
	if scrollLimit != 100 {
		t.Fatalf("scroll limit = %d, want bounded default 100", scrollLimit)
	}
}

type backfillEmbedder struct{}

func (backfillEmbedder) EmbedDocuments(context.Context, []string) ([][]float64, error) {
	return [][]float64{{0.2, 0.3, 0.4}}, nil
}
func (backfillEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return []float64{0.2, 0.3, 0.4}, nil
}
func (backfillEmbedder) Name() string  { return "test" }
func (backfillEmbedder) Model() string { return "test" }

func TestLegacyFallbackVector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		vector []float64
		want   bool
	}{
		{"legacy", []float64{1, 0, 0}, true},
		{"real", []float64{1, 0.1, 0}, false},
		{"zero", []float64{0, 0, 0}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := legacyFallbackVector(tc.vector); got != tc.want {
				t.Fatalf("legacyFallbackVector(%v)=%v want %v", tc.vector, got, tc.want)
			}
		})
	}
}

func TestBackfillFallbackVectors_RepairsLegacyPoint(t *testing.T) {
	t.Parallel()
	var upsertBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/scroll"):
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"stored-uuid","vector":[1,0,0],"payload":{"id":"chunk-1","content":"hello"}}],"next_page_offset":"next"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/test"):
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":3,"distance":"Cosine"}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			body, _ := io.ReadAll(r.Body)
			upsertBody = string(body)
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	svc := &Service{qdrant: qdrant.NewClient(httpclient.NewDefault(), srv.URL, "", "test", "Cosine"), embed: backfillEmbedder{}}
	result, err := svc.BackfillFallbackVectors(context.Background(), 1, "start")
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 1 || result.Corrected != 1 || result.Failed != 0 || result.Cursor != "next" {
		t.Fatalf("result=%+v", result)
	}
	if !strings.Contains(upsertBody, `"_embedding_fallback":false`) || !strings.Contains(upsertBody, `0.2`) {
		t.Fatalf("upsert body=%s", upsertBody)
	}
}
