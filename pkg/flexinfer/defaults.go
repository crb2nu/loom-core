package flexinfer

// In-cluster defaults for the FlexInfer OpenAI-compatible proxy.
//
// These are the single source of truth for "where do embeddings go when
// nothing is configured". They exist as one package because the same two
// values used to be copy-pasted into pkg/pm, pkg/agentcontext (twice),
// and pkg/codebase; the 2026-08-13 Morph-retirement incident needed two
// rounds of fixes (!1584, !1589) precisely because each round only found
// some of the copies. DefaultsGuard (defaults_guard_test.go) fails the
// build if a new copy appears.
//
// Consuming these constants does NOT change precedence: every call site
// keeps its own env chain and passes the constant only as the terminal
// fallback.
//
// Note the deliberate non-consumers. pkg/mills/clients and
// internal/hud/coordinator reference the proxy URL in documentation only:
// both read it from env with an *empty* default, and that emptiness is
// load-bearing — it disables the LLM-judged Mills gates and makes
// coordinator.Config.Enabled() report false. Defaulting them to the
// in-cluster URL would silently arm those subsystems off-cluster.
const (
	// DefaultProxyURL is the in-cluster service URL of the FlexInfer
	// proxy, with no path suffix and no trailing slash. Callers that
	// need the OpenAI-compatible API root want DefaultEmbedBaseURL.
	DefaultProxyURL = "http://flexinfer-proxy.flexinfer-system.svc.cluster.local"

	// DefaultEmbedBaseURL is the OpenAI-compatible API root on the proxy
	// (DefaultProxyURL + "/v1"). This is the value that belongs in an
	// EmbedBaseURL / CODEBASE_EMBED_BASE_URL style setting.
	DefaultEmbedBaseURL = DefaultProxyURL + "/v1"

	// DefaultEmbedModel is the proxy-side alias for the 1536-dimension
	// embedding model. The dimension is part of the contract: Qdrant
	// collections written by agent-context, pm, and codebase are all
	// sized 1536, so changing this alias to a differently-sized model
	// produces the dimension-mismatch storm documented in the
	// Morph-retirement incident rather than a clean failure.
	DefaultEmbedModel = "embeddings-1536"

	// DefaultEmbedProvider is the provider key that selects the FlexInfer
	// embedding backend in the codebase/agent-context/pm provider switch.
	DefaultEmbedProvider = "flexinfer"
)

// Retired Morph identifiers. Configuration surviving from before the
// Morph retirement still carries these, so the normalize paths in
// pkg/agentcontext and pkg/codebase rewrite them onto the FlexInfer
// defaults above rather than dialing a decommissioned endpoint.
const (
	// RetiredMorphBaseURL is the decommissioned Morph API root.
	RetiredMorphBaseURL = "https://api.morphllm.com/v1"

	// RetiredMorphModel is the decommissioned Morph embedding model.
	RetiredMorphModel = "morph-embedding-v3"

	// RetiredMorphHost matches RetiredMorphBaseURL by substring, for
	// normalize checks against arbitrary user-supplied base URLs.
	RetiredMorphHost = "api.morphllm.com"

	// RetiredMorphModelPrefix matches any Morph embedding model
	// generation (morph-embedding-v2, -v3, ...).
	RetiredMorphModelPrefix = "morph-embedding-"
)
