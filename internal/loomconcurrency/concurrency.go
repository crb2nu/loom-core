package loomconcurrency

import (
	"context"
	"sync"
)

// Concurrency bounds concurrent admissions. Values outside the supported
// range are clamped so an omitted or malformed zero value can never create a
// zero-capacity semaphore and deadlock all work.
type Concurrency struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	changed chan struct{}
}

// NewConcurrency constructs an admission limiter with a safe effective limit.
func NewConcurrency(limit int) *Concurrency {
	limit = clamp(limit)
	return &Concurrency{limit: limit, changed: make(chan struct{})}
}

// Limit returns the effective, clamped admission limit.
func (c *Concurrency) Limit() int {
	if c == nil {
		return MinLimit
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

// Acquire waits for an admission slot or returns the context error.
func (c *Concurrency) Acquire(ctx context.Context) error {
	if c == nil {
		return nil
	}
	for {
		c.mu.Lock()
		if c.inUse < c.limit {
			c.inUse++
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release returns one admission slot.
func (c *Concurrency) Release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.inUse > 0 {
		c.inUse--
		c.notifyLocked()
	}
	c.mu.Unlock()
}

func (c *Concurrency) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}
