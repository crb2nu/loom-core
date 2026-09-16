package agentcontext

import (
	"testing"

	"github.com/crb2nu/loom/pkg/flexinfer"
)

// TestNormalizeRetiredEmbedConfig pins the 2026-09-05 regression: a stale
// MORPH_EMBED_MODEL / MORPH_BASE_URL in a deployment sits ahead of the
// FlexInfer defaults in the env chain, so every embed asked flexinfer-proxy
// for a model it does not serve. Any retired marker must collapse the whole
// embed target to the FlexInfer defaults.
func TestNormalizeRetiredEmbedConfig(t *testing.T) {
	cases := []struct {
		name string
		in   Config
	}{
		{"retired model", Config{EmbedProvider: "flexinfer", EmbedBaseURL: flexinfer.DefaultEmbedBaseURL, EmbedModel: "morph-embedding-v3"}},
		{"retired model v4 mixed case", Config{EmbedProvider: "flexinfer", EmbedBaseURL: flexinfer.DefaultEmbedBaseURL, EmbedModel: " Morph-Embedding-V4 "}},
		{"retired host", Config{EmbedProvider: "flexinfer", EmbedBaseURL: "https://api.morphllm.com/v1", EmbedModel: "embeddings-1536"}},
		{"retired provider", Config{EmbedProvider: "morph", EmbedBaseURL: "https://example.invalid/v1", EmbedModel: "anything"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.in
			normalizeRetiredEmbedConfig(&cfg)
			if cfg.EmbedProvider != flexinfer.DefaultEmbedProvider || cfg.EmbedBaseURL != flexinfer.DefaultEmbedBaseURL || cfg.EmbedModel != flexinfer.DefaultEmbedModel {
				t.Fatalf("got provider=%q url=%q model=%q, want FlexInfer defaults", cfg.EmbedProvider, cfg.EmbedBaseURL, cfg.EmbedModel)
			}
		})
	}
}

func TestNormalizeRetiredEmbedConfigLeavesLiveConfigAlone(t *testing.T) {
	cfg := Config{EmbedProvider: "flexinfer", EmbedBaseURL: "http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1", EmbedModel: "gte-qwen2-1p5b-radeonvii"}
	normalizeRetiredEmbedConfig(&cfg)
	if cfg.EmbedProvider != "flexinfer" || cfg.EmbedBaseURL != "http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1" || cfg.EmbedModel != "gte-qwen2-1p5b-radeonvii" {
		t.Fatalf("live config was rewritten: provider=%q url=%q model=%q", cfg.EmbedProvider, cfg.EmbedBaseURL, cfg.EmbedModel)
	}
}

func TestLoadConfigFromEnvIgnoresRetiredMorphEnv(t *testing.T) {
	t.Setenv("MORPH_BASE_URL", "https://api.morphllm.com/v1")
	t.Setenv("MORPH_EMBED_MODEL", "morph-embedding-v3")
	t.Setenv("AGENT_CONTEXT_EMBED_MODEL", "")
	t.Setenv("CODEBASE_EMBED_MODEL", "")
	t.Setenv("AGENT_CONTEXT_EMBED_BASE_URL", "")
	t.Setenv("CODEBASE_EMBED_BASE_URL", "")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbedModel != flexinfer.DefaultEmbedModel || cfg.EmbedBaseURL != flexinfer.DefaultEmbedBaseURL {
		t.Fatalf("retired MORPH_* env leaked through: model=%q url=%q", cfg.EmbedModel, cfg.EmbedBaseURL)
	}
}
