package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

func newRAGTestServer(t *testing.T, handler http.HandlerFunc) (*flexinferServer, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	f := newTestServer(nil)
	f.proxyURL = ts.URL
	f.httpClient = httpclient.NewDefault()
	return f, ts
}

func TestRAG_DefaultsToRetrieveOnly(t *testing.T) {
	var captured map[string]any
	f, _ := newRAGTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rag" {
			t.Errorf("expected /v1/rag, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answer": nil,
			"citations": []map[string]any{
				{"path": "pkg/a.go", "score": 0.9, "text": "chunk a"},
			},
			"context_tokens": 42,
		})
	})

	res, err := f.handleRAG(context.Background(), map[string]any{"query": "how does X work?"})
	if err != nil {
		t.Fatalf("handleRAG: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}

	if captured["query"] != "how does X work?" {
		t.Errorf("query not forwarded: %v", captured["query"])
	}
	if captured["retrieve_only"] != true {
		t.Errorf("retrieve_only must default to true, got %v", captured["retrieve_only"])
	}
	if _, present := captured["collection"]; present {
		t.Errorf("empty collection must be omitted so the service default applies")
	}
	if _, present := captured["max_per_path"]; present {
		t.Errorf("unset max_per_path must be omitted so the service default applies")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &decoded); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, res.Content[0].Text)
	}
	if decoded["ok"] != true {
		t.Errorf("expected ok:true, got %v", decoded["ok"])
	}
}

func TestRAG_ForwardsTuningParams(t *testing.T) {
	var captured map[string]any
	f, _ := newRAGTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{"answer": "cited answer"})
	})

	_, err := f.handleRAG(context.Background(), map[string]any{
		"query":         "q",
		"collection":    "codebase_memory_bge_flexinfer_v1",
		"retrieve_only": false,
		"top_k":         8,
		"top_n":         32,
		"max_per_path":  3,
	})
	if err != nil {
		t.Fatalf("handleRAG: %v", err)
	}

	if captured["collection"] != "codebase_memory_bge_flexinfer_v1" {
		t.Errorf("collection not forwarded: %v", captured["collection"])
	}
	if captured["retrieve_only"] != false {
		t.Errorf("retrieve_only=false not forwarded: %v", captured["retrieve_only"])
	}
	if captured["top_k"] != float64(8) || captured["top_n"] != float64(32) {
		t.Errorf("top_k/top_n not forwarded: %v / %v", captured["top_k"], captured["top_n"])
	}
	if captured["max_per_path"] != float64(3) {
		t.Errorf("max_per_path not forwarded: %v", captured["max_per_path"])
	}
}

func TestRAG_MissingQueryIsValidationError(t *testing.T) {
	f := newTestServer(nil)
	f.proxyURL = "http://proxy.test"
	f.httpClient = httpclient.NewDefault()

	res, err := f.handleRAG(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handleRAG: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error for missing query")
	}
}

func TestRAG_UnconfiguredProxyIsGraceful(t *testing.T) {
	f := newTestServer(nil)
	f.proxyURL = ""
	f.httpClient = httpclient.NewDefault()

	res, err := f.handleRAG(context.Background(), map[string]any{"query": "q"})
	if err != nil {
		t.Fatalf("handleRAG: %v", err)
	}
	if res.IsError {
		t.Fatalf("unconfigured proxy should be a friendly JSON result, not an error")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &decoded); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if decoded["ok"] != false {
		t.Errorf("expected ok:false for unconfigured proxy, got %v", decoded["ok"])
	}
}

func TestRAG_UpstreamErrorSurfacesStatus(t *testing.T) {
	f, _ := newRAGTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad collection"}`, http.StatusBadRequest)
	})

	res, err := f.handleRAG(context.Background(), map[string]any{"query": "q"})
	if err != nil {
		t.Fatalf("handleRAG: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result for upstream 400")
	}
}
