package pm

import (
	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/httpclient"
)

// buildEmbedder constructs an Embedder for the configured provider, mirroring
// the agent-context provider selection (morph default, flexinfer, ollama,
// dummy). Embedding is best-effort at the service layer, so no circuit breaker
// is layered here.
func buildEmbedder(hc *httpclient.Client, cfg Config) embed.Embedder {
	provider := cfg.EmbedProvider
	baseURL := cfg.EmbedBaseURL
	model := cfg.EmbedModel

	switch provider {
	case "flexinfer":
		if baseURL == "" || baseURL == "https://api.morphllm.com/v1" {
			baseURL = env.StringChain([]string{"FLEXINFER_URL"}, "http://localhost:8080") + "/v1"
		}
		if model == "" || model == "morph-embedding-v3" {
			model = "BAAI/bge-large-en-v1.5"
		}
		return embed.NewFlexInferClient(hc, baseURL, cfg.EmbedAPIKey, model)
	case "ollama":
		if baseURL == "" || baseURL == "https://api.morphllm.com/v1" {
			baseURL = "http://localhost:11434"
		}
		if model == "" || model == "morph-embedding-v3" {
			model = "nomic-embed-text"
		}
		return embed.NewOllamaClient(hc, baseURL, model)
	case "dummy", "none":
		return embed.NewDummyEmbedder(VectorSize)
	default:
		return embed.NewMorphClient(hc, baseURL, cfg.EmbedAPIKey, model)
	}
}

// fallbackVector returns a deterministic, non-zero unit vector of dimension
// VectorSize. Used when embedding fails so the point still persists and stays
// filterable by payload. Qdrant rejects all-zero vectors under cosine
// distance, so the first component is set to 1.
func fallbackVector() []float64 {
	v := make([]float64, VectorSize)
	v[0] = 1
	return v
}
