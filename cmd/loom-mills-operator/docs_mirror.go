package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/finishing"
)

const docsMirrorInterval = time.Hour

type docsMirrorCache struct {
	mu       sync.RWMutex
	value    finishing.DocsMirrorDrift
	check    func(context.Context, time.Time) finishing.DocsMirrorDrift
	interval time.Duration
}

func newDocsMirrorCache() *docsMirrorCache {
	return &docsMirrorCache{value: finishing.DocsMirrorDrift{State: finishing.StateUnknown, Reason: "not yet computed"}, interval: docsMirrorInterval}
}

func (c *docsMirrorCache) Get() finishing.DocsMirrorDrift {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}
func (c *docsMirrorCache) refresh(ctx context.Context, now time.Time) {
	if c.check == nil {
		return
	}
	v := c.check(ctx, now)
	c.mu.Lock()
	c.value = v
	c.mu.Unlock()
	if v.State == finishing.StateUnknown {
		return
	}
	mills.FinishingDocsMirrorDriftFiles.Set(float64(v.Missing + v.Stale))
	if v.MirrorCommitAt != nil {
		age := now.Sub(*v.MirrorCommitAt).Seconds()
		if age < 0 {
			age = 0
		}
		mills.FinishingDocsMirrorAgeSeconds.Set(age)
	}
}
func (c *docsMirrorCache) Run(ctx context.Context) error {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			c.refresh(ctx, now.UTC())
		}
	}
}
func (o *operator) handleDocsMirror(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, o.docsMirror.Get())
}
