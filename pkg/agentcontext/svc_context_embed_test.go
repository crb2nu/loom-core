package agentcontext

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// capturedUpsert decodes the vectors from a qdrant points upsert request body.
type capturedUpsert struct {
	Points []struct {
		Vector []float64 `json:"vector"`
	} `json:"points"`
}

// newEmbedOutageQdrant returns an httptest server that emulates a Qdrant in
// which the "context" collection does not yet exist: GET 404s, PUT creates and
// upserts succeed. Every points upsert body is appended to *captured so a test
// can assert the persisted (fallback) vectors.
func newEmbedOutageQdrant(t *testing.T, captured *[]capturedUpsert) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/context":
			// Collection does not exist yet → drives the default-size fallback.
			http.NotFound(w, r)
		case r.Method == http.MethodPut && r.URL.Path == "/collections/context/points":
			body, _ := io.ReadAll(r.Body)
			var cu capturedUpsert
			if err := json.Unmarshal(body, &cu); err != nil {
				t.Fatalf("decode upsert body: %v", err)
			}
			*captured = append(*captured, cu)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","result":{"status":"acknowledged"}}`))
		case r.Method == http.MethodPut:
			// Collection create + registered payload indexes — idempotent ack.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","result":true}`))
		default:
			t.Fatalf("unexpected qdrant request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func assertNonZeroFallback(t *testing.T, vec []float64) {
	t.Helper()
	if len(vec) != defaultEmbedVectorSize {
		t.Fatalf("fallback vector dim = %d, want %d", len(vec), defaultEmbedVectorSize)
	}
	for _, c := range vec {
		if c != 0 {
			return
		}
	}
	t.Error("fallback vector is all-zero; Qdrant rejects that under cosine distance")
}

// TestStoreContextEntries_EmbedFailureStillPersists is the regression guard for
// the agent_context_add write path: a context entry MUST still persist (with a
// deterministic fallback vector) when the embedder is down. Without the
// best-effort decoupling, an embedder outage (Morph 522 / gte-qwen2-1.5b HTTP
// 500 / circuit breaker open) hard-fails agent_context_add and agents lose the
// ability to record any decision or finding.
func TestStoreContextEntries_EmbedFailureStillPersists(t *testing.T) {
	var captured []capturedUpsert
	server := newEmbedOutageQdrant(t, &captured)
	t.Cleanup(server.Close)

	cfg := Config{
		QdrantURL:         server.URL,
		QdrantDistance:    "Cosine",
		ContextCollection: "context",
	}
	vectorSize := 0
	metrics := NewMetrics()
	cs := &ContextSvc{
		qdrant:     NewQdrantRegistry(httpclient.NewDefault(), cfg),
		embed:      failingEmbedder{},
		vectorSize: &vectorSize,
		cfg:        cfg,
		metrics:    metrics,
		logger:     slog.Default(),
	}

	entries := []ContextEntry{{
		ID:        "ctx-1",
		AgentID:   "claude-code-test",
		SessionID: "s1",
		Namespace: "services/loom-core/fix/x",
		Title:     "Chose best-effort embed",
		Content:   "Persist context even when the embedder is down.",
	}}

	ids, err := cs.storeContextEntries(context.Background(), entries, []string{"Chose best-effort embed"})
	if err != nil {
		t.Fatalf("storeContextEntries returned error despite best-effort embed: %v", err)
	}
	if len(ids) != 1 || ids[0] != "ctx-1" {
		t.Fatalf("ids = %v, want [ctx-1]", ids)
	}
	if len(captured) != 1 || len(captured[0].Points) != 1 {
		t.Fatalf("captured upserts = %+v, want 1 point persisted", captured)
	}
	assertNonZeroFallback(t, captured[0].Points[0].Vector)

	if got := metrics.EmbeddingErrors.Load(); got != 1 {
		t.Errorf("EmbeddingErrors = %d, want 1", got)
	}
}

// TestAnnotationAdd_EmbedFailureStillPersists is the regression guard for the
// annotation write path (agent_code_annotate / annotations): an annotation MUST
// still persist with a fallback vector when the embedder is down.
func TestAnnotationAdd_EmbedFailureStillPersists(t *testing.T) {
	var captured []capturedUpsert
	server := newEmbedOutageQdrant(t, &captured)
	t.Cleanup(server.Close)

	cfg := Config{
		QdrantURL:         server.URL,
		QdrantDistance:    "Cosine",
		ContextCollection: "context",
	}
	vectorSize := 0
	cs := &ContextSvc{
		qdrant:     NewQdrantRegistry(httpclient.NewDefault(), cfg),
		embed:      failingEmbedder{},
		vectorSize: &vectorSize,
		cfg:        cfg,
		metrics:    NewMetrics(),
		logger:     slog.Default(),
		getSession: func(_ context.Context, sessionID string) (*Session, error) {
			return &Session{ID: sessionID, AgentID: "claude-code-test", Namespace: "services/loom-core/fix/x"}, nil
		},
	}

	res, err := cs.AnnotationAdd(context.Background(), map[string]any{
		"session_id": "s1",
		"file_path":  "pkg/agentcontext/svc_context_add.go",
		"line_start": 191,
		"content":    "best-effort embed decouples persistence from the embedder",
	})
	if err != nil {
		t.Fatalf("AnnotationAdd returned transport error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("AnnotationAdd returned a tool error despite best-effort embed: %+v", res.Content)
	}
	if len(captured) != 1 || len(captured[0].Points) != 1 {
		t.Fatalf("captured upserts = %+v, want 1 annotation persisted", captured)
	}
	assertNonZeroFallback(t, captured[0].Points[0].Vector)
}
