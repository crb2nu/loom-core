package codebase

import (
	"context"
	"sync"

	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/codebase/index"
	"github.com/crb2nu/loom/pkg/codebase/qdrant"
	"github.com/crb2nu/loom/pkg/codebase/schema"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/flexinfer"
	"github.com/crb2nu/loom/pkg/httpclient"
)

// Embedding provider defaults. These are used as fallbacks when the
// configured provider differs from the base config's default values.
//
// The FlexInfer and retired-Morph values are aliases into pkg/flexinfer,
// which is the single source of truth for them. They were literals here
// until the Morph retirement showed the cost: the same two strings lived in
// pkg/pm, pkg/agentcontext, and here, and two rounds of fixes each caught
// only some of the copies.
const (
	defaultFlexInferProvider = flexinfer.DefaultEmbedProvider
	defaultFlexInferBaseURL  = flexinfer.DefaultEmbedBaseURL
	defaultFlexInferURL      = flexinfer.DefaultProxyURL
	defaultFlexInferModel    = flexinfer.DefaultEmbedModel
	defaultOllamaURL         = "http://localhost:11434"
	defaultOllamaModel       = "nomic-embed-text"
	defaultMorphBaseURL      = flexinfer.RetiredMorphBaseURL
	defaultMorphModel        = flexinfer.RetiredMorphModel
)

type Service struct {
	cfg Config

	qdrant *qdrant.Client
	embed  embed.Embedder

	indexers *index.Registry

	jobsMu sync.RWMutex
	jobs   map[string]*indexJob

	watchMu   sync.RWMutex
	watchJobs map[string]*watchJob

	// watchStore persists running-watch descriptors so they survive a process
	// restart and can be re-launched by ResumeWatches.
	watchStore *watchStore

	// baseCtx is the long-lived parent for watch goroutines. It is set to the
	// server context by ResumeWatches at startup so watches outlive the
	// per-request handler context (which dies on every reconnect) yet still
	// stop cleanly on process shutdown. Defaults to context.Background().
	baseCtx context.Context

	// watchJanitorOnce guards the single idle-watch sweep goroutine.
	watchJanitorOnce sync.Once
}

type indexJob struct {
	id string

	cancel context.CancelFunc

	status string
	err    string

	stats schema.IndexStats
}

func NewServiceFromEnv() (*Service, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}

	hc := httpclient.NewDefault()

	// Select embedder based on configuration
	var embedder embed.Embedder
	if cfg.DisableEmbeddingsDefault {
		// Use dummy embedder when embeddings are disabled
		embedder = embed.NewDummyEmbedder(1)
	} else {
		switch cfg.EmbedProvider {
		case "flexinfer":
			// FlexInfer TEI backend (OpenAI-compatible)
			baseURL := cfg.EmbedBaseURL
			if baseURL == "" || baseURL == defaultMorphBaseURL {
				baseURL = env.String("FLEXINFER_URL", defaultFlexInferURL) + "/v1"
			}
			model := cfg.EmbedModel
			if model == "" || model == defaultMorphModel {
				model = defaultFlexInferModel
			}
			embedder = embed.NewFlexInferClient(hc, baseURL, cfg.EmbedAPIKey, model)
		case "ollama":
			// Ollama local embeddings
			baseURL := cfg.EmbedBaseURL
			if baseURL == "" || baseURL == defaultMorphBaseURL {
				baseURL = env.String("OLLAMA_BASE_URL", defaultOllamaURL)
			}
			model := cfg.EmbedModel
			if model == "" || model == defaultMorphModel {
				model = defaultOllamaModel
			}
			embedder = embed.NewOllamaClient(hc, baseURL, model)
		case "dummy", "none":
			// Explicit dummy mode
			embedder = embed.NewDummyEmbedder(1)
		default:
			// Default to Morph/OpenAI-compatible API
			embedder = embed.NewMorphClient(hc, cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel)
		}
	}

	svc := &Service{
		cfg:        cfg,
		qdrant:     qdrant.NewClient(hc, cfg.QdrantURL, cfg.QdrantAPIKey, cfg.QdrantCollection, cfg.QdrantDistance),
		embed:      embedder,
		jobs:       make(map[string]*indexJob),
		watchJobs:  make(map[string]*watchJob),
		watchStore: newWatchStore(cfg.WatchStateDir),
		baseCtx:    context.Background(),
		indexers: index.NewRegistry(
			cfg.MaxFileBytes,
		),
	}

	return svc, nil
}

// NewServiceWithEmbedder creates a service with a custom embedder.
func NewServiceWithEmbedder(cfg Config, embedder embed.Embedder) (*Service, error) {
	hc := httpclient.NewDefault()

	svc := &Service{
		cfg:        cfg,
		qdrant:     qdrant.NewClient(hc, cfg.QdrantURL, cfg.QdrantAPIKey, cfg.QdrantCollection, cfg.QdrantDistance),
		embed:      embedder,
		jobs:       make(map[string]*indexJob),
		watchJobs:  make(map[string]*watchJob),
		watchStore: newWatchStore(cfg.WatchStateDir),
		baseCtx:    context.Background(),
		indexers: index.NewRegistry(
			cfg.MaxFileBytes,
		),
	}

	return svc, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
