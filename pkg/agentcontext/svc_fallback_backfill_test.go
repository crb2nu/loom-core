package agentcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

type sequencedEmbedder struct {
	vectors [][]float64
	errors  []error
	calls   int
}

func (*sequencedEmbedder) Name() string  { return "sequenced-test" }
func (*sequencedEmbedder) Model() string { return "test-model" }

func (e *sequencedEmbedder) EmbedDocuments(_ context.Context, _ []string) ([][]float64, error) {
	i := e.calls
	e.calls++
	if i < len(e.errors) && e.errors[i] != nil {
		return nil, e.errors[i]
	}
	return [][]float64{e.vectors[i]}, nil
}

func (e *sequencedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	vectors, err := e.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func TestBackfillFallbackPoints_BoundedResumablePartialFailure(t *testing.T) {
	var scrollBody map[string]any
	var upserts []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/collections/patterns/points/scroll":
			if err := json.NewDecoder(r.Body).Decode(&scrollBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"p1","vector":[1,0,0],"payload":{"name":"one"}},{"id":"p2","vector":[1,0,0],"payload":{"name":"two","_embedding_fallback":true}}],"next_page_offset":"resume-2"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/patterns/points":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			upserts = append(upserts, body)
			_, _ = w.Write([]byte(`{"result":{"status":"acknowledged"},"status":"ok"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	q := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "patterns", "Cosine")
	embedder := &sequencedEmbedder{
		vectors: [][]float64{{1, 2, 3}, nil},
		errors:  []error{nil, errors.New("temporary embed failure")},
	}
	vectorSize := 3
	result, err := backfillFallbackPoints(context.Background(), q, embedder, &vectorSize, nil, 2, "resume-1", func(payload map[string]any) string {
		return toString(payload["name"])
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 2 || result.Corrected != 1 || result.Failed != 1 || result.Cursor != "resume-2" {
		t.Fatalf("result = %+v, want scanned=2 corrected=1 failed=1 cursor=resume-2", result)
	}
	if result.FailureReasons["embed_error"] != 1 {
		t.Fatalf("failure reasons = %#v, want one embed_error", result.FailureReasons)
	}
	if scrollBody["limit"] != float64(2) || scrollBody["offset"] != "resume-1" {
		t.Fatalf("scroll pagination = %#v", scrollBody)
	}
	if scrollBody["with_vector"] != true {
		t.Fatalf("scroll request = %#v, want vectors for legacy fallback detection", scrollBody)
	}
	if len(upserts) != 1 {
		t.Fatalf("upserts = %d, want only the successfully embedded row", len(upserts))
	}
	points := upserts[0]["points"].([]any)
	payload := points[0].(map[string]any)["payload"].(map[string]any)
	if payload[embeddingFallbackPayloadKey] != false {
		t.Fatalf("successful payload marker = %#v, want false", payload[embeddingFallbackPayloadKey])
	}
}

func TestBackfillFallbackPoints_RejectsDimensionMismatch(t *testing.T) {
	upserts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/collections/context/points/scroll" {
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"c1","vector":[1,0,0],"payload":{"title":"title","content":"body","_embedding_fallback":true}}]}}`))
			return
		}
		if r.Method == http.MethodPut {
			upserts++
		}
	}))
	t.Cleanup(server.Close)

	q := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "context", "Cosine")
	embedder := &sequencedEmbedder{vectors: [][]float64{{1, 2}}}
	vectorSize := 3
	result, err := backfillFallbackPoints(context.Background(), q, embedder, &vectorSize, nil, 1, nil, func(map[string]any) string { return "text" })
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 1 || result.Corrected != 0 || result.Failed != 1 || upserts != 0 {
		t.Fatalf("result = %+v, upserts=%d; want mismatch retained as failed", result, upserts)
	}
	if result.FailureReasons["dimension_mismatch"] != 1 {
		t.Fatalf("failure reasons = %#v, want one dimension_mismatch", result.FailureReasons)
	}
}

func TestBackfillFallbackPoints_EmptyInputHistogramAndRateCappedLogs(t *testing.T) {
	var points strings.Builder
	for i := 1; i <= 12; i++ {
		if i > 1 {
			points.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&points, `{"id":"p%d","vector":[1,0,0],"payload":{"content":"  "}}`, i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"result":{"points":[%s]}}`, points.String())
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	q := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "context", "Cosine")
	embedder := &sequencedEmbedder{}
	vectorSize := 3
	result, err := backfillFallbackPoints(context.Background(), q, embedder, &vectorSize, logger, 12, nil, func(payload map[string]any) string {
		return toString(payload["content"])
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 12 || result.FailureReasons["empty_input"] != 12 || embedder.calls != 0 {
		t.Fatalf("result = %+v, embed calls = %d; want 12 empty-input failures and no embed calls", result, embedder.calls)
	}
	output := logs.String()
	if !strings.Contains(output, "row_id=p1") || !strings.Contains(output, "row_id=p10") {
		t.Fatalf("logs missing row ids within cap: %s", output)
	}
	if strings.Contains(output, "row_id=p11") || strings.Contains(output, "row_id=p12") {
		t.Fatalf("logs exceeded per-row cap: %s", output)
	}
	if !strings.Contains(output, "suppressed=2") {
		t.Fatalf("logs missing capped summary: %s", output)
	}
}

func TestBackfillFallbackPoints_CategorizesInvalidVectorAndUpsertFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scroll"):
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"invalid","vector":[1,0,0],"payload":{"content":"one"}},{"id":"upsert","vector":[1,0,0],"payload":{"content":"two"}}]}}`))
		case r.Method == http.MethodPut:
			http.Error(w, "qdrant unavailable", http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	q := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "context", "Cosine")
	embedder := &sequencedEmbedder{vectors: [][]float64{nil, {1, 2, 3}}}
	vectorSize := 3
	result, err := backfillFallbackPoints(context.Background(), q, embedder, &vectorSize, nil, 2, nil, func(payload map[string]any) string {
		return toString(payload["content"])
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 2 || result.FailureReasons["invalid_vector"] != 1 || result.FailureReasons["upsert_error"] != 1 {
		t.Fatalf("result = %+v, want invalid_vector=1 and upsert_error=1", result)
	}
}
