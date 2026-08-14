package pm

import "testing"

func TestLoadConfigFromEnvEmbedDefaults(t *testing.T) {
	for _, key := range []string{
		"PM_EMBED_PROVIDER", "AGENT_CONTEXT_EMBED_PROVIDER", "CODEBASE_EMBED_PROVIDER",
		"PM_EMBED_BASE_URL", "AGENT_CONTEXT_EMBED_BASE_URL", "CODEBASE_EMBED_BASE_URL",
		"MORPH_BASE_URL", "FLEXINFER_URL", "OPENAI_BASE_URL",
		"PM_EMBED_MODEL", "AGENT_CONTEXT_EMBED_MODEL", "CODEBASE_EMBED_MODEL",
		"MORPH_EMBED_MODEL", "FLEXINFER_EMBED_MODEL",
	} {
		t.Setenv(key, "")
	}

	cfg := LoadConfigFromEnv()
	if cfg.EmbedBaseURL != "http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1" {
		t.Fatalf("EmbedBaseURL = %q, want implicit-port-80 FlexInfer URL", cfg.EmbedBaseURL)
	}
	if cfg.EmbedModel != "embeddings-1536" {
		t.Fatalf("EmbedModel = %q, want embeddings-1536", cfg.EmbedModel)
	}
}

func TestNormalizeRetiredEmbedConfigUsesFlexInferDefaults(t *testing.T) {
	cfg := Config{
		EmbedProvider: "morph",
		EmbedBaseURL:  "https://api.morphllm.com/v1",
		EmbedModel:    "morph-embedding-v4",
	}

	normalizeRetiredEmbedConfig(&cfg)

	if cfg.EmbedBaseURL != "http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1" {
		t.Fatalf("EmbedBaseURL = %q, want implicit-port-80 FlexInfer URL", cfg.EmbedBaseURL)
	}
	if cfg.EmbedModel != "embeddings-1536" {
		t.Fatalf("EmbedModel = %q, want embeddings-1536", cfg.EmbedModel)
	}
}
