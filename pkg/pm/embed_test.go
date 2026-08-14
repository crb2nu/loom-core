package pm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/flexinfer"
	"github.com/crb2nu/loom/pkg/httpclient"
)

func TestLoadConfigFromEnvEmbeddingResolution(t *testing.T) {
	tests := []struct {
		name                             string
		env                              map[string]string
		wantProvider, wantURL, wantModel string
	}{
		{name: "configless flexinfer", wantProvider: flexinfer.DefaultEmbedProvider, wantURL: flexinfer.DefaultEmbedBaseURL, wantModel: flexinfer.DefaultEmbedModel},
		{name: "specific vars win", env: map[string]string{"PM_EMBED_PROVIDER": "flexinfer", "PM_EMBED_BASE_URL": "http://custom/v1", "PM_EMBED_MODEL": "custom-1536", "MORPH_EMBED_MODEL": "morph-embedding-v3"}, wantProvider: "flexinfer", wantURL: "http://custom/v1", wantModel: "custom-1536"},
		{name: "retired legacy values normalize", env: map[string]string{"MORPH_BASE_URL": "https://api.morphllm.com/v1", "MORPH_EMBED_MODEL": "morph-embedding-v4"}, wantProvider: flexinfer.DefaultEmbedProvider, wantURL: flexinfer.DefaultEmbedBaseURL, wantModel: flexinfer.DefaultEmbedModel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"PM_EMBED_PROVIDER", "PM_EMBED_BASE_URL", "PM_EMBED_MODEL", "AGENT_CONTEXT_EMBED_PROVIDER", "AGENT_CONTEXT_EMBED_BASE_URL", "AGENT_CONTEXT_EMBED_MODEL", "CODEBASE_EMBED_PROVIDER", "CODEBASE_EMBED_BASE_URL", "CODEBASE_EMBED_MODEL", "MORPH_BASE_URL", "MORPH_EMBED_MODEL", "FLEXINFER_URL", "FLEXINFER_EMBED_MODEL"} {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			cfg := LoadConfigFromEnv()
			if cfg.EmbedProvider != tt.wantProvider || cfg.EmbedBaseURL != tt.wantURL || cfg.EmbedModel != tt.wantModel {
				t.Fatalf("embedding config = %q %q %q, want %q %q %q", cfg.EmbedProvider, cfg.EmbedBaseURL, cfg.EmbedModel, tt.wantProvider, tt.wantURL, tt.wantModel)
			}
		})
	}
}

func TestBuildEmbedderFlexInferRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != flexinfer.DefaultEmbedModel {
			t.Fatalf("model = %q", body.Model)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float64{1, 2}}}})
	}))
	defer server.Close()
	e := buildEmbedder(httpclient.NewDefault(), Config{EmbedProvider: "flexinfer", EmbedBaseURL: server.URL + "/v1", EmbedModel: flexinfer.DefaultEmbedModel})
	if _, err := e.EmbedDocuments(context.Background(), []string{"hello"}); err != nil {
		t.Fatal(err)
	}
}
