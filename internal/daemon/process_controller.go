package daemon

import (
	"context"
	"fmt"
	"sync"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/process"
	"github.com/crb2nu/loom/pkg/registry"
)

// processControllerConfig is an immutable launch-configuration revision. A
// shard captures one value and uses its registry for both TargetSpec lookup and
// template expansion, preventing reload from composing a command out of two
// registry revisions.
type processControllerConfig struct {
	registry *registry.Registry
	target   string
	repoRoot string
	revision uint64
}

func (c processControllerConfig) expand(value string) string {
	return expandVarsWithRegistry(value, c.repoRoot, c.registry)
}

type processShardFactory func(serverName string, config processControllerConfig) (localProcessController, error)

// processControllerShard serializes lifecycle operations for exactly one
// server key. The dependency Manager still takes its own lock across Dial and
// Stop, but each shard contains only one key, so a slow server cannot pin the
// rest of the fleet.
type processControllerShard struct {
	controller localProcessController
	gate       chan struct{}
	retired    bool // protected by gate
}

func newProcessControllerShard(controller localProcessController) *processControllerShard {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &processControllerShard{controller: controller, gate: gate}
}

func (s *processControllerShard) acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.gate:
		return nil
	}
}

func (s *processControllerShard) release() {
	s.gate <- struct{}{}
}

// shardedProcessController is loomd's production local-process boundary. Its
// mutex protects only the fleet index and current immutable config pointer; it
// is never held while a dependency Manager performs Dial or Stop.
type shardedProcessController struct {
	mu      sync.Mutex
	config  processControllerConfig
	shards  map[string]*processControllerShard
	factory processShardFactory
}

func newShardedProcessController(reg *registry.Registry, target, repoRoot string) *shardedProcessController {
	return newShardedProcessControllerWithFactory(reg, target, repoRoot, newManagerProcessShard)
}

func newShardedProcessControllerWithFactory(
	reg *registry.Registry,
	target string,
	repoRoot string,
	factory processShardFactory,
) *shardedProcessController {
	return &shardedProcessController{
		config: processControllerConfig{
			registry: reg,
			target:   target,
			repoRoot: repoRoot,
			revision: 1,
		},
		shards:  make(map[string]*processControllerShard),
		factory: factory,
	}
}

func newManagerProcessShard(_ string, config processControllerConfig) (localProcessController, error) {
	if config.registry == nil {
		return nil, fmt.Errorf("process controller registry revision %d is unavailable", config.revision)
	}
	manager := process.NewManager(config.registry, config.target)
	// Capture config by value. In particular, do not consult Daemon's current
	// registry here: Manager.Start reads TargetSpec later while holding its shard
	// lock, and expansion must observe that exact same immutable registry.
	manager.SetExpandFunc(config.expand)
	return manager, nil
}

// UpdateRegistry publishes the config used only by future shards. Existing
// shards retain their captured revision until Stop completes and removes them.
// The update never waits for a shard Dial or Stop.
func (c *shardedProcessController) UpdateRegistry(reg *registry.Registry) uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	c.config.registry = reg
	c.config.revision++
	revision := c.config.revision
	c.mu.Unlock()
	return revision
}

func (c *shardedProcessController) Dial(ctx context.Context, serverName string) (mcp.Transport, error) {
	if c == nil {
		return nil, fmt.Errorf("process controller unavailable")
	}
	for {
		shard, err := c.shard(serverName)
		if err != nil {
			return nil, err
		}
		if err := shard.acquire(ctx); err != nil {
			return nil, err
		}
		if shard.retired {
			shard.release()
			continue
		}
		transport, dialErr := shard.controller.Dial(ctx, serverName)
		if dialErr != nil {
			// No generation resource exists to call Stop after a failed Dial. Drop
			// this shard so a retry (especially after reload) captures the latest
			// registry revision. fi-mcp-kit cleans partial starts before returning.
			shard.retired = true
			c.mu.Lock()
			if c.shards[serverName] == shard {
				delete(c.shards, serverName)
			}
			c.mu.Unlock()
		}
		shard.release()
		return transport, dialErr
	}
}

func (c *shardedProcessController) Stop(serverName string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	shard := c.shards[serverName]
	c.mu.Unlock()
	if shard == nil {
		return nil
	}

	// Stop has no context in fi-mcp-kit's Manager API. Waiting here blocks only
	// this key; other shard operations and config updates remain independent.
	_ = shard.acquire(context.Background())
	if shard.retired {
		shard.release()
		return nil
	}
	stopErr := shard.controller.Stop(serverName)
	shard.retired = true

	c.mu.Lock()
	if c.shards[serverName] == shard {
		delete(c.shards, serverName)
	}
	c.mu.Unlock()
	shard.release()
	return stopErr
}

func (c *shardedProcessController) shard(serverName string) (*processControllerShard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if shard := c.shards[serverName]; shard != nil {
		return shard, nil
	}
	if c.factory == nil {
		return nil, fmt.Errorf("process controller shard factory unavailable")
	}
	controller, err := c.factory(serverName, c.config)
	if err != nil {
		return nil, err
	}
	if controller == nil {
		return nil, fmt.Errorf("process controller shard factory returned nil for %s", serverName)
	}
	shard := newProcessControllerShard(controller)
	c.shards[serverName] = shard
	return shard, nil
}
