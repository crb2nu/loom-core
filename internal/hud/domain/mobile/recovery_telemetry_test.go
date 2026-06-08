package mobile

import (
	"math"
	"sort"
	"testing"
	"time"
)

// swiftNearestRankP95 reproduces ConnectionHealthMonitor.recoveryStats's p95
// (apps/loom-companion-ios/.../ConnectionHealthMonitor.swift):
//
//	let sorted = samples.sorted()
//	let rank = Int((0.95 * Double(sorted.count)).rounded(.up))
//	let index = min(max(rank - 1, 0), sorted.count - 1)
//	p95 = sorted[index]
//
// The Go meanAndP95 must agree with this on any input — that parity is the
// slice's load-bearing assumption (uploaded p95 == dashboard p95).
func swiftNearestRankP95(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)
	rank := int(math.Ceil(0.95 * float64(len(sorted))))
	index := rank - 1
	if index < 0 {
		index = 0
	}
	if index > len(sorted)-1 {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func TestRecoveryStore_P95_MatchesSwiftNearestRank(t *testing.T) {
	// Hand-computed expectations pin the formula; the swift cross-check guards
	// against drift on arbitrary vectors.
	cases := []struct {
		name    string
		samples []float64
		wantP95 float64
	}{
		{"single", []float64{7}, 7},
		{"n5_unsorted", []float64{3, 1, 4, 1, 5}, 5},                 // sorted[ceil(4.75)-1=4]=5
		{"n10_linear", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 10}, // ceil(9.5)=10 -> idx9
		{"n20_linear", linspace(1, 20), 19},                          // ceil(19)=19 -> idx18 -> 19
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotP95 := meanAndP95(tc.samples)
			if gotP95 != tc.wantP95 {
				t.Errorf("p95 = %v, want %v", gotP95, tc.wantP95)
			}
			if want := swiftNearestRankP95(tc.samples); gotP95 != want {
				t.Errorf("p95 = %v diverges from swift formula %v", gotP95, want)
			}
		})
	}
}

func TestRecoveryStore_MeanAndP95_Empty(t *testing.T) {
	mean, p95 := meanAndP95(nil)
	if mean != 0 || p95 != 0 {
		t.Fatalf("empty meanAndP95 = (%v,%v), want (0,0)", mean, p95)
	}
}

func TestRecoveryStore_Ingest_ReplacesPerDevice(t *testing.T) {
	s := newRecoveryStore()
	s.Ingest("dev-a", []float64{10, 20, 30}, 0)
	s.Ingest("dev-a", []float64{5}, 0) // replace, not append

	agg := s.Aggregate()
	if agg.DeviceCount != 1 {
		t.Fatalf("device_count = %d, want 1", agg.DeviceCount)
	}
	if agg.TotalSamples != 1 {
		t.Fatalf("total_samples = %d, want 1 (replace, not append)", agg.TotalSamples)
	}
	if agg.Devices[0].SampleCount != 1 || agg.Devices[0].MeanSeconds != 5 {
		t.Errorf("device stat = %+v, want sample_count=1 mean=5", agg.Devices[0])
	}
}

func TestRecoveryStore_Ingest_TruncatesToCap(t *testing.T) {
	s := newRecoveryStore()
	in := make([]float64, MaxRecoverySamples+25)
	for i := range in {
		in[i] = float64(i + 1)
	}
	stat := s.Ingest("dev", in, 0)
	if stat.SampleCount != MaxRecoverySamples {
		t.Fatalf("sample_count = %d, want %d (truncated)", stat.SampleCount, MaxRecoverySamples)
	}
	// Most-recent kept: the window should end at the largest value.
	agg := s.Aggregate()
	if agg.Devices[0].P95Seconds < float64(len(in)-MaxRecoverySamples) {
		t.Errorf("expected most-recent samples retained, p95=%v", agg.Devices[0].P95Seconds)
	}
}

func TestRecoveryStore_Ingest_DropsInvalid(t *testing.T) {
	s := newRecoveryStore()
	stat := s.Ingest("dev", []float64{math.NaN(), math.Inf(1), -3, 0, 12, 8}, 0)
	if stat.SampleCount != 2 {
		t.Fatalf("sample_count = %d, want 2 (only 12,8 valid)", stat.SampleCount)
	}

	// All-invalid yields an empty window — the handler maps this to 400.
	allBad := s.Ingest("dev2", []float64{math.NaN(), -1, 0}, 0)
	if allBad.SampleCount != 0 {
		t.Errorf("all-invalid sample_count = %d, want 0", allBad.SampleCount)
	}
}

func TestRecoveryStore_Aggregate_PoolsSamples(t *testing.T) {
	s := newRecoveryStore()
	// Two devices, both under the 30s SLO.
	s.Ingest("dev-a", []float64{5, 6, 7}, 0)
	s.Ingest("dev-b", []float64{8, 9, 10}, 0)

	agg := s.Aggregate()
	if agg.DeviceCount != 2 {
		t.Fatalf("device_count = %d, want 2", agg.DeviceCount)
	}
	if agg.TotalSamples != 6 {
		t.Fatalf("total_samples = %d, want 6 (pooled)", agg.TotalSamples)
	}
	// Fleet p95 is nearest-rank over the pooled [5,6,7,8,9,10].
	if want := swiftNearestRankP95([]float64{5, 6, 7, 8, 9, 10}); agg.FleetP95Seconds != want {
		t.Errorf("fleet p95 = %v, want pooled nearest-rank %v", agg.FleetP95Seconds, want)
	}
	if agg.DevicesMeetingSLO != 2 || !agg.MeetsSLO {
		t.Errorf("expected both devices meet SLO; got devices_meeting=%d meets=%v", agg.DevicesMeetingSLO, agg.MeetsSLO)
	}
	// Devices breakdown is deterministically ordered by device_id.
	if agg.Devices[0].DeviceID != "dev-a" || agg.Devices[1].DeviceID != "dev-b" {
		t.Errorf("device order = [%s,%s], want sorted", agg.Devices[0].DeviceID, agg.Devices[1].DeviceID)
	}
}

func TestRecoveryStore_Aggregate_SLOBreach(t *testing.T) {
	s := newRecoveryStore()
	s.Ingest("fast", []float64{2, 3, 4}, 0)
	s.Ingest("slow", []float64{40, 50, 60}, 0) // p95=60 > 30

	agg := s.Aggregate()
	if agg.DevicesMeetingSLO != 1 {
		t.Errorf("devices_meeting_slo = %d, want 1", agg.DevicesMeetingSLO)
	}
	// Pooled p95 over [2,3,4,40,50,60] -> nearest-rank idx ceil(5.7)-1=5 -> 60 > 30.
	if agg.MeetsSLO {
		t.Errorf("fleet should breach SLO; fleet p95=%v target=%v", agg.FleetP95Seconds, agg.SLOTargetSeconds)
	}
}

func TestRecoveryStore_Aggregate_Empty(t *testing.T) {
	s := newRecoveryStore()
	agg := s.Aggregate()
	if agg.DeviceCount != 0 || agg.TotalSamples != 0 {
		t.Fatalf("empty aggregate non-zero: %+v", agg)
	}
	if !agg.MeetsSLO {
		t.Error("empty aggregate should meet SLO vacuously")
	}
	if agg.SLOTargetSeconds != DefaultRecoverySLOTargetSeconds {
		t.Errorf("empty target = %v, want default %v", agg.SLOTargetSeconds, DefaultRecoverySLOTargetSeconds)
	}
	if agg.Devices == nil {
		t.Error("Devices should be a non-nil empty slice for stable JSON")
	}
}

func TestRecoveryStore_SLOTarget_Normalize(t *testing.T) {
	s := newRecoveryStore()
	// Garbage/<=0 target falls back to default (30): p95=25 meets it.
	if stat := s.Ingest("g", []float64{25}, -5); stat.SLOTargetSec != DefaultRecoverySLOTargetSeconds || !stat.MeetsSLO {
		t.Errorf("garbage target = %+v, want default + meets", stat)
	}
	// Custom strict target makes the same sample breach.
	if stat := s.Ingest("c", []float64{25}, 10); stat.SLOTargetSec != 10 || stat.MeetsSLO {
		t.Errorf("custom target = %+v, want 10 + breach", stat)
	}
	// Absurd target clamps.
	if stat := s.Ingest("h", []float64{25}, 1e9); stat.SLOTargetSec != maxRecoverySLOTargetSeconds {
		t.Errorf("clamped target = %v, want %v", stat.SLOTargetSec, maxRecoverySLOTargetSeconds)
	}
}

func TestRecoveryStore_InjectableClock(t *testing.T) {
	s := newRecoveryStore()
	fixed := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	stat := s.Ingest("dev", []float64{5}, 0)
	if stat.UpdatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("updated_at = %q, want %q", stat.UpdatedAt, fixed.Format(time.RFC3339))
	}
	if agg := s.Aggregate(); agg.UpdatedAt != fixed.Format(time.RFC3339) {
		t.Errorf("aggregate updated_at = %q, want %q", agg.UpdatedAt, fixed.Format(time.RFC3339))
	}
}

// linspace returns [lo, lo+1, ..., hi].
func linspace(lo, hi int) []float64 {
	out := make([]float64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, float64(i))
	}
	return out
}
