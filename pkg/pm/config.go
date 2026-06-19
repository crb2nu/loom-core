package pm

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/env"
)

// RisksCollection is the fixed Qdrant collection name for the risks domain.
// Vector size 1536, distance Cosine — matching sibling agent-context
// collections so the same Qdrant instance/embedder can be reused.
const RisksCollection = "pm_risks"

// VectorSize is the embedding dimension stored in pm_risks.
const VectorSize = 1536

// Config holds the runtime configuration for the pm service. Env var names are
// shared with agent-context (QDRANT_URL, embed provider chain) so mcp-pm points
// at the same Qdrant/embedder without separate wiring.
type Config struct {
	// Qdrant
	QdrantURL      string
	QdrantAPIKey   string
	QdrantDistance string
	Collection     string

	// Embeddings (best-effort; reuses the agent-context env chain).
	EmbedProvider string
	EmbedAPIKey   string
	EmbedBaseURL  string
	EmbedModel    string

	// EmbedTimeout caps each best-effort embed call so a stalled provider never
	// blocks a write — on timeout the fallback vector is used instead.
	EmbedTimeout time.Duration
}

// LoadConfigFromEnv builds Config from the environment, reusing the
// agent-context variable names so a single set of env vars configures both
// servers.
func LoadConfigFromEnv() Config {
	return Config{
		QdrantURL: strings.TrimRight(env.StringChain(
			[]string{"PM_QDRANT_URL", "AGENT_CONTEXT_QDRANT_URL", "QDRANT_URL"},
			"http://localhost:6333",
		), "/"),
		QdrantAPIKey:   env.StringChain([]string{"PM_QDRANT_API_KEY", "AGENT_CONTEXT_QDRANT_API_KEY", "QDRANT_API_KEY"}, ""),
		QdrantDistance: env.StringChain([]string{"PM_QDRANT_DISTANCE", "AGENT_CONTEXT_QDRANT_DISTANCE"}, "Cosine"),
		Collection:     env.StringChain([]string{"PM_RISKS_COLLECTION"}, RisksCollection),

		EmbedProvider: strings.ToLower(env.StringChain(
			[]string{"PM_EMBED_PROVIDER", "AGENT_CONTEXT_EMBED_PROVIDER", "CODEBASE_EMBED_PROVIDER"},
			"morph",
		)),
		EmbedAPIKey: env.StringChain(
			[]string{"PM_EMBED_API_KEY", "AGENT_CONTEXT_EMBED_API_KEY", "CODEBASE_EMBED_API_KEY", "MORPH_API_KEY", "FLEXINFER_API_KEY", "OPENAI_API_KEY"},
			"",
		),
		EmbedBaseURL: strings.TrimRight(env.StringChain(
			[]string{"PM_EMBED_BASE_URL", "AGENT_CONTEXT_EMBED_BASE_URL", "CODEBASE_EMBED_BASE_URL", "MORPH_BASE_URL", "FLEXINFER_URL", "OPENAI_BASE_URL"},
			"https://api.morphllm.com/v1",
		), "/"),
		EmbedModel: env.StringChain(
			[]string{"PM_EMBED_MODEL", "AGENT_CONTEXT_EMBED_MODEL", "CODEBASE_EMBED_MODEL", "MORPH_EMBED_MODEL", "FLEXINFER_EMBED_MODEL"},
			"morph-embedding-v3",
		),
		EmbedTimeout: env.Duration("PM_EMBED_TIMEOUT", 5*time.Second),
	}
}
