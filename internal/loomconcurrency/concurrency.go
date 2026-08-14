package loomconcurrency

import "context"

// Concurrency bounds concurrent admissions. Values outside the supported
// range are clamped so an omitted or malformed zero value can never create a
// zero-capacity semaphore and deadlock all work.
type Concurrency struct {
	limit int
	slots chan struct{}
}

// NewConcurrency constructs an admission limiter with a safe effective limit.
func NewConcurrency(limit int) *Concurrency {
	limit = clamp(limit)
	return &Concurrency{limit: limit, slots: make(chan struct{}, limit)}
}

// Limit returns the effective, clamped admission limit.
func (c *Concurrency) Limit() int {
	if c == nil {
		return MinLimit
	}
	return c.limit
}

// Acquire waits for an admission slot or returns the context error.
func (c *Concurrency) Acquire(ctx context.Context) error {
	if c == nil {
		return nil
	}
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns one admission slot.
func (c *Concurrency) Release() {
	if c != nil {
		<-c.slots
	}
}
