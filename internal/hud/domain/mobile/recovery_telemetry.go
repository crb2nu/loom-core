package mobile

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Recovery telemetry constants. These mirror the iOS
// ConnectionHealthMonitor so a device's self-reported samples are
// interpreted identically server-side.
const (
	// MaxRecoverySamples caps the rolling per-device sample window. Matches
	// ConnectionHealthMonitor.maxRecoverySamples (50). Larger windows are
	// truncated to the most-recent samples on ingest.
	MaxRecoverySamples = 50

	// DefaultRecoverySLOTargetSeconds is the disconnect-to-recovered p95 SLO
	// target. Matches ConnectionHealthMonitor.recoveryP95TargetSeconds (30s,
	// one poll-fallback cycle). Used when a device omits slo_target_seconds.
	DefaultRecoverySLOTargetSeconds = 30.0

	// maxRecoverySLOTargetSeconds bounds an attacker/garbage target so the
	// verdict stays meaningful.
	maxRecoverySLOTargetSeconds = 3600.0
)

// recoveryIngestRequest is the POST body for recovery-telemetry ingestion.
// A device reports its rolling window of disconnect-to-recovered durations.
type recoveryIngestRequest struct {
	// Samples are recovery durations in seconds (each > 0, finite). The
	// device's rolling window; order is not significant (stats are computed
	// over the multiset).
	Samples []float64 `json:"samples"`
	// SLOTargetSeconds is the device's p95 SLO target. Optional; defaults to
	// DefaultRecoverySLOTargetSeconds.
	SLOTargetSeconds float64 `json:"slo_target_seconds,omitempty"`
}

// RecoveryDeviceStat is the server-computed per-device recovery summary.
type RecoveryDeviceStat struct {
	DeviceID     string  `json:"device_id"`
	SampleCount  int     `json:"sample_count"`
	MeanSeconds  float64 `json:"mean_seconds"`
	P95Seconds   float64 `json:"p95_seconds"`
	SLOTargetSec float64 `json:"slo_target_seconds"`
	MeetsSLO     bool    `json:"meets_slo"`
	UpdatedAt    string  `json:"updated_at"`
}

// RecoveryAggregate is the fleet-wide recovery-SLO rollup returned by the read
// endpoint. Fleet mean/p95 are computed over the pooled samples of all devices
// (not an average of per-device summaries), so the fleet p95 is a true
// nearest-rank percentile of the combined window.
type RecoveryAggregate struct {
	DeviceCount       int                  `json:"device_count"`
	TotalSamples      int                  `json:"total_samples"`
	FleetMeanSeconds  float64              `json:"fleet_mean_seconds"`
	FleetP95Seconds   float64              `json:"fleet_p95_seconds"`
	SLOTargetSeconds  float64              `json:"slo_target_seconds"`
	DevicesMeetingSLO int                  `json:"devices_meeting_slo"`
	MeetsSLO          bool                 `json:"meets_slo"`
	Devices           []RecoveryDeviceStat `json:"devices"`
	UpdatedAt         string               `json:"updated_at"`
}

// deviceRecovery is the retained per-device state: the raw sample window plus
// the device's declared target and last-update time.
type deviceRecovery struct {
	samples   []float64
	sloTarget float64
	updatedAt time.Time
}

// recoveryStore holds the latest recovery-telemetry snapshot per device and
// computes fleet aggregates. In-memory only (mirrors the rate-limiter and
// revocation-list pattern); not durable across restarts.
type recoveryStore struct {
	mu      sync.RWMutex
	devices map[string]deviceRecovery
	now     func() time.Time
}

func newRecoveryStore() *recoveryStore {
	return &recoveryStore{
		devices: make(map[string]deviceRecovery),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Ingest records (replacing any prior snapshot) a device's recovery samples and
// returns the server-computed per-device stat. Callers must pre-validate that
// deviceID is non-empty and samples is non-empty; sanitizeSamples here drops
// non-finite / non-positive values and truncates to MaxRecoverySamples.
func (s *recoveryStore) Ingest(deviceID string, samples []float64, sloTarget float64) RecoveryDeviceStat {
	clean := sanitizeSamples(samples)
	target := normalizeSLOTarget(sloTarget)

	s.mu.Lock()
	ts := s.now()
	s.devices[deviceID] = deviceRecovery{samples: clean, sloTarget: target, updatedAt: ts}
	s.mu.Unlock()

	mean, p95 := meanAndP95(clean)
	return RecoveryDeviceStat{
		DeviceID:     deviceID,
		SampleCount:  len(clean),
		MeanSeconds:  mean,
		P95Seconds:   p95,
		SLOTargetSec: target,
		MeetsSLO:     len(clean) > 0 && p95 <= target,
		UpdatedAt:    ts.Format(time.RFC3339),
	}
}

// Aggregate computes the fleet rollup over the pooled samples of all devices.
// An empty store yields zeros with MeetsSLO=true (vacuously) and the default
// SLO target.
func (s *recoveryStore) Aggregate() RecoveryAggregate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agg := RecoveryAggregate{
		SLOTargetSeconds: DefaultRecoverySLOTargetSeconds,
		MeetsSLO:         true,
		Devices:          []RecoveryDeviceStat{},
	}
	if len(s.devices) == 0 {
		return agg
	}

	pooled := make([]float64, 0, len(s.devices)*8)
	var latest time.Time
	// Use the largest device-declared target as the fleet target so the
	// verdict isn't stricter than any reporting device expects.
	target := DefaultRecoverySLOTargetSeconds

	// Stable, deterministic device ordering for the breakdown.
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		dev := s.devices[id]
		pooled = append(pooled, dev.samples...)
		if dev.sloTarget > target {
			target = dev.sloTarget
		}
		if dev.updatedAt.After(latest) {
			latest = dev.updatedAt
		}
		mean, p95 := meanAndP95(dev.samples)
		stat := RecoveryDeviceStat{
			DeviceID:     id,
			SampleCount:  len(dev.samples),
			MeanSeconds:  mean,
			P95Seconds:   p95,
			SLOTargetSec: dev.sloTarget,
			MeetsSLO:     len(dev.samples) > 0 && p95 <= dev.sloTarget,
			UpdatedAt:    dev.updatedAt.Format(time.RFC3339),
		}
		agg.Devices = append(agg.Devices, stat)
		if stat.MeetsSLO {
			agg.DevicesMeetingSLO++
		}
	}

	fleetMean, fleetP95 := meanAndP95(pooled)
	agg.DeviceCount = len(ids)
	agg.TotalSamples = len(pooled)
	agg.FleetMeanSeconds = fleetMean
	agg.FleetP95Seconds = fleetP95
	agg.SLOTargetSeconds = target
	agg.MeetsSLO = len(pooled) == 0 || fleetP95 <= target
	if !latest.IsZero() {
		agg.UpdatedAt = latest.Format(time.RFC3339)
	}
	return agg
}

// sanitizeSamples drops non-finite and non-positive values and truncates to the
// most-recent MaxRecoverySamples. Returns a fresh slice (never aliases input).
func sanitizeSamples(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			continue
		}
		out = append(out, v)
	}
	if len(out) > MaxRecoverySamples {
		out = out[len(out)-MaxRecoverySamples:]
	}
	return out
}

// normalizeSLOTarget clamps a device-declared target to a sane range, falling
// back to the default for non-positive/garbage values.
func normalizeSLOTarget(target float64) float64 {
	if math.IsNaN(target) || math.IsInf(target, 0) || target <= 0 {
		return DefaultRecoverySLOTargetSeconds
	}
	if target > maxRecoverySLOTargetSeconds {
		return maxRecoverySLOTargetSeconds
	}
	return target
}

// meanAndP95 computes the arithmetic mean and the nearest-rank p95 of the
// samples. The p95 formula is identical to the iOS ConnectionHealthMonitor
// recoveryStats: rank = ceil(0.95 * n), index = clamp(rank-1, 0, n-1). Returns
// (0, 0) for an empty slice.
func meanAndP95(samples []float64) (mean, p95 float64) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	mean = sum / float64(n)

	sorted := make([]float64, n)
	copy(sorted, samples)
	sort.Float64s(sorted)
	rank := int(math.Ceil(0.95 * float64(n)))
	index := rank - 1
	if index < 0 {
		index = 0
	}
	if index > n-1 {
		index = n - 1
	}
	return mean, sorted[index]
}
