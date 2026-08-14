package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// defaultTelemetryCacheTTL bounds how long a computed stage roll-up is reused.
// The panel polls every 30s and the endpoint recomputes 3 windowed aggregations
// plus sorts on every tick by design; a short TTL keeps sub-tick freshness while
// collapsing a burst of polls (multiple HUD tabs, a manual refresh) into one
// aggregation. Kept well under the panel poll interval so a live operator still
// sees fresh numbers each cycle.
const defaultTelemetryCacheTTL = 8 * time.Second

const (
	telemetryCacheTTLEnv        = "MILLS_TELEMETRY_CACHE_TTL_SECONDS"
	minTelemetryCacheTTLSeconds = 1
	maxTelemetryCacheTTLSeconds = 60
)

func telemetryCacheTTLFromEnv(logger *slog.Logger) time.Duration {
	raw, present := os.LookupEnv(telemetryCacheTTLEnv)
	return resolveTelemetryCacheTTL(raw, present, logger)
}

func resolveTelemetryCacheTTL(raw string, present bool, logger *slog.Logger) time.Duration {
	if !present || strings.TrimSpace(raw) == "" {
		return defaultTelemetryCacheTTL
	}
	trimmed := strings.TrimSpace(raw)
	seconds, err := strconv.Atoi(trimmed)
	if err != nil {
		if logger != nil {
			logger.Warn("ignoring invalid MILLS_TELEMETRY_CACHE_TTL_SECONDS", "value", raw)
		}
		return defaultTelemetryCacheTTL
	}
	if seconds < minTelemetryCacheTTLSeconds {
		if logger != nil {
			logger.Warn("clamping MILLS_TELEMETRY_CACHE_TTL_SECONDS below minimum", "value", raw, "min_seconds", minTelemetryCacheTTLSeconds)
		}
		seconds = minTelemetryCacheTTLSeconds
	}
	if seconds > maxTelemetryCacheTTLSeconds {
		if logger != nil {
			logger.Warn("clamping MILLS_TELEMETRY_CACHE_TTL_SECONDS above maximum", "value", raw, "max_seconds", maxTelemetryCacheTTLSeconds)
		}
		seconds = maxTelemetryCacheTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}

// telemetryCacheEntry is one window's memoized aggregation. generatedAt is the
// wall-clock at compute time — served back verbatim so the panel's freshness
// read reflects the data, not the cache hit.
type telemetryCacheEntry struct {
	report      *store.StageTelemetry
	generatedAt time.Time
	expiresAt   time.Time
}

// telemetryStageCache is a tiny thread-safe TTL memo for the stage telemetry
// roll-up, keyed by window seconds. Zero external deps; a sync.Mutex guards the
// map. Not an LRU: the key space is the three accepted windows (1d/7d/30d), so
// the map never exceeds three entries.
type telemetryStageCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[int]telemetryCacheEntry
}

// newTelemetryStageCache constructs a cache with the given TTL. A non-positive
// ttl falls back to defaultTelemetryCacheTTL so a misconfigured caller can't
// disable memoization by accident.
func newTelemetryStageCache(ttl time.Duration) *telemetryStageCache {
	if ttl <= 0 {
		ttl = defaultTelemetryCacheTTL
	}
	return &telemetryStageCache{
		ttl:     ttl,
		entries: make(map[int]telemetryCacheEntry),
	}
}

// get returns the memoized report + its compute time for windowSeconds when a
// non-expired entry exists. now is passed in so tests can drive expiry
// deterministically. A nil cache is a miss (defensive; production always wires
// one).
func (c *telemetryStageCache) get(windowSeconds int, now time.Time) (*store.StageTelemetry, time.Time, bool) {
	if c == nil {
		return nil, time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[windowSeconds]
	if !ok || !now.Before(e.expiresAt) {
		return nil, time.Time{}, false
	}
	return e.report, e.generatedAt, true
}

// put memoizes report for windowSeconds, stamping generatedAt=now and an expiry
// ttl later. A nil cache or report is a no-op.
func (c *telemetryStageCache) put(windowSeconds int, report *store.StageTelemetry, now time.Time) {
	if c == nil || report == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[windowSeconds] = telemetryCacheEntry{
		report:      report,
		generatedAt: now,
		expiresAt:   now.Add(c.ttl),
	}
}
