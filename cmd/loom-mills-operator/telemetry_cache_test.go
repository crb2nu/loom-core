package main

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestTelemetryStageCache_HitMissExpiry(t *testing.T) {
	c := newTelemetryStageCache(10 * time.Second)
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	rep := &store.StageTelemetry{Stages: []store.StageAgg{}, ModelEconomics: []store.ModelEconomicsRow{}}

	// Cold miss.
	if _, _, ok := c.get(604800, base); ok {
		t.Fatal("expected cold miss")
	}

	c.put(604800, rep, base)

	// Hit within TTL returns the same report + the stored generated_at.
	got, gen, ok := c.get(604800, base.Add(5*time.Second))
	if !ok {
		t.Fatal("expected hit within TTL")
	}
	if got != rep {
		t.Errorf("hit returned a different report pointer")
	}
	if !gen.Equal(base) {
		t.Errorf("generated_at = %v, want stored compute time %v", gen, base)
	}

	// A different window is an independent key → miss.
	if _, _, ok := c.get(86400, base.Add(5*time.Second)); ok {
		t.Fatal("expected miss for a different window key")
	}

	// Exactly at expiry is a miss (expiresAt is exclusive).
	if _, _, ok := c.get(604800, base.Add(10*time.Second)); ok {
		t.Fatal("expected miss exactly at expiry")
	}
	// Past expiry is a miss.
	if _, _, ok := c.get(604800, base.Add(11*time.Second)); ok {
		t.Fatal("expected miss past expiry")
	}
}

func TestTelemetryStageCache_NilAndZeroTTL(t *testing.T) {
	// A nil cache is always a miss and put is a no-op (defensive).
	var c *telemetryStageCache
	if _, _, ok := c.get(604800, time.Now()); ok {
		t.Error("nil cache must miss")
	}
	c.put(604800, &store.StageTelemetry{}, time.Now()) // must not panic

	// Zero TTL falls back to the default so memoization can't be disabled by
	// accident.
	d := newTelemetryStageCache(0)
	if d.ttl != defaultTelemetryCacheTTL {
		t.Errorf("zero ttl = %v, want fallback %v", d.ttl, defaultTelemetryCacheTTL)
	}

	// A nil report is not stored.
	d.put(604800, nil, time.Now())
	if _, _, ok := d.get(604800, time.Now()); ok {
		t.Error("nil report must not be cached")
	}
}

func TestResolveTelemetryCacheTTL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		present bool
		want    time.Duration
	}{
		{
			name:    "unset uses default",
			present: false,
			want:    defaultTelemetryCacheTTL,
		},
		{
			name:    "valid in range",
			raw:     "12",
			present: true,
			want:    12 * time.Second,
		},
		{
			name:    "below minimum clamps",
			raw:     "0",
			present: true,
			want:    time.Second,
		},
		{
			name:    "above maximum clamps",
			raw:     "61",
			present: true,
			want:    60 * time.Second,
		},
		{
			name:    "non integer uses default",
			raw:     "soon",
			present: true,
			want:    defaultTelemetryCacheTTL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTelemetryCacheTTL(tt.raw, tt.present, nil)
			if got != tt.want {
				t.Fatalf("resolveTelemetryCacheTTL(%q, %v) = %v, want %v", tt.raw, tt.present, got, tt.want)
			}
		})
	}
}
