package daemon

import (
	"context"
	"sync"

	"github.com/crb2nu/loom/pkg/weaver"
)

// WeaverPreflight is the daemon-facing alias for the shared preflight
// result. The implementation lives in pkg/weaver so the standalone
// cmd/mcp-weaver server surfaces the same degraded signal as the
// daemon-embedded router.
//
// See .loom/111-product-spec-weaver-qwen3-integration-2026-05-08.md
// (PRE-001..PRE-003).
type WeaverPreflight = weaver.Preflight

// runWeaverPreflight queries FlexInfer for its model catalog and
// compares it against the configured router/subagent/domain models.
// Never returns an error; the caller treats CatalogError as the
// degraded signal.
func runWeaverPreflight(ctx context.Context, client weaver.ModelLister, cfg weaver.Config, registry *weaver.DomainRegistry) WeaverPreflight {
	return weaver.RunPreflight(ctx, client, cfg, registry)
}

// preflightStore is a tiny mutex-guarded holder used by the daemon to
// publish the latest preflight result without exposing the field
// directly. Keeps reads safe across the JSON-RPC dispatch goroutines.
type preflightStore struct {
	mu      sync.RWMutex
	current WeaverPreflight
	set     bool
}

func (s *preflightStore) Set(p WeaverPreflight) {
	s.mu.Lock()
	s.current = p
	s.set = true
	s.mu.Unlock()
}

func (s *preflightStore) Get() (WeaverPreflight, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current, s.set
}
