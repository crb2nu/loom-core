package gates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func staticProbe(name string, critical bool, state HealthState) DependencyProbe {
	return DependencyProbe{
		Name: name, Critical: critical,
		Probe: func(context.Context) ProbeResult { return ProbeResult{State: state} },
	}
}

func TestCompositeStorageEvaluator_StampsEachComponentAndSortsByName(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	e := &CompositeStorageEvaluator{
		Now: func() time.Time { return now },
		Probes: []DependencyProbe{
			staticProbe("zebra", false, HealthStateDegraded),
			staticProbe("alpha", true, HealthStateHealthy),
		},
	}

	snapshot, err := e.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("EvaluateStorageHealth() error = %v", err)
	}
	if len(snapshot.Components) != 2 {
		t.Fatalf("got %d components, want 2", len(snapshot.Components))
	}
	if snapshot.Components[0].Name != "alpha" || snapshot.Components[1].Name != "zebra" {
		t.Fatalf("components not sorted by name: %+v", snapshot.Components)
	}
	for _, c := range snapshot.Components {
		if !c.CheckedAt.Equal(now) {
			t.Errorf("component %s CheckedAt = %v, want %v", c.Name, c.CheckedAt, now)
		}
	}
	if !snapshot.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v", snapshot.ObservedAt, now)
	}
}

// A probe that is declared but has no function is a wiring bug. It must read
// as unknown so the gate blocks rather than treating the silence as healthy.
func TestCompositeStorageEvaluator_MissingProbeFuncIsUnknown(t *testing.T) {
	e := &CompositeStorageEvaluator{Probes: []DependencyProbe{{Name: "broken", Critical: true}}}

	snapshot, err := e.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("EvaluateStorageHealth() error = %v", err)
	}
	if got := snapshot.Components[0].State; got != HealthStateUnknown {
		t.Fatalf("state = %q, want %q", got, HealthStateUnknown)
	}
	if decision := EvaluateHealthSnapshot(snapshot, time.Now().UTC()); decision.Allowed || !decision.FailClosed {
		t.Fatalf("decision = %+v, want fail-closed block", decision)
	}
}

func TestCompositeStorageEvaluator_PanicIsUnknownNotACrash(t *testing.T) {
	e := &CompositeStorageEvaluator{Probes: []DependencyProbe{{
		Name: "panics", Critical: true,
		Probe: func(context.Context) ProbeResult { panic("probe exploded") },
	}}}

	snapshot, err := e.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("EvaluateStorageHealth() error = %v", err)
	}
	c := snapshot.Components[0]
	if c.State != HealthStateUnknown {
		t.Fatalf("state = %q, want %q", c.State, HealthStateUnknown)
	}
	if c.Error == "" {
		t.Fatal("panicking probe recorded no error text")
	}
	if c.CheckedAt.IsZero() {
		t.Fatal("panicking probe left CheckedAt zero, which reads as missing evidence")
	}
}

// fixedUsage pins capacity so the assertion is about the probe's policy
// mapping rather than about how full the host's disk happens to be.
func fixedUsage(capacity, inodes float64) func(string) (float64, float64, error) {
	return func(string) (float64, float64, error) { return capacity, inodes, nil }
}

func TestFilesystemStorageProbe_HealthyOnWritableDir(t *testing.T) {
	result := FilesystemStorageProbe{Path: t.TempDir(), Usage: fixedUsage(10, 5)}.probe(context.Background())
	if result.State != HealthStateHealthy {
		t.Fatalf("state = %q (%s), want healthy", result.State, result.Error)
	}
}

// 80% used is advisory: the component stays healthy so the mill keeps running,
// but it carries the evidence so the HUD tile can show the pressure.
func TestFilesystemStorageProbe_WarningIsAdvisoryByDefault(t *testing.T) {
	result := FilesystemStorageProbe{Path: t.TempDir(), Usage: fixedUsage(85, 0)}.probe(context.Background())
	if result.State != HealthStateHealthy {
		t.Fatalf("state = %q, want healthy (warning is advisory)", result.State)
	}
	if result.Error == "" {
		t.Error("warning state recorded no evidence for the operator")
	}
}

func TestFilesystemStorageProbe_WarningBlocksWhenOptedIn(t *testing.T) {
	result := FilesystemStorageProbe{
		Path: t.TempDir(), Usage: fixedUsage(85, 0), BlockOnWarning: true,
	}.probe(context.Background())
	if result.State != HealthStateDegraded {
		t.Fatalf("state = %q, want degraded", result.State)
	}
}

func TestFilesystemStorageProbe_CriticalCapacityBlocks(t *testing.T) {
	result := FilesystemStorageProbe{Path: t.TempDir(), Usage: fixedUsage(95, 0)}.probe(context.Background())
	if result.State == HealthStateHealthy {
		t.Fatalf("state = %q, want a blocking state at 95%% used", result.State)
	}
}

// Inode exhaustion fails writes while bytes look fine; the more severe of the
// two readings must win.
func TestFilesystemStorageProbe_InodeExhaustionWins(t *testing.T) {
	result := FilesystemStorageProbe{Path: t.TempDir(), Usage: fixedUsage(5, 100)}.probe(context.Background())
	if result.State != HealthStateDown {
		t.Fatalf("state = %q, want down on inode exhaustion", result.State)
	}
}

func TestFilesystemStorageProbe_UsageErrorIsUnknown(t *testing.T) {
	result := FilesystemStorageProbe{
		Path:  t.TempDir(),
		Usage: func(string) (float64, float64, error) { return 0, 0, errors.New("statfs failed") },
	}.probe(context.Background())
	if result.State != HealthStateUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
}

func TestFilesystemStorageProbe_UnconfiguredPathIsUnknown(t *testing.T) {
	result := FilesystemStorageProbe{}.probe(context.Background())
	if result.State != HealthStateUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
}

func TestFilesystemStorageProbe_MissingPathIsUnknown(t *testing.T) {
	result := FilesystemStorageProbe{Path: "/nonexistent/mills/storage"}.probe(context.Background())
	if result.State != HealthStateUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
}

// A failing store ping means mutations are no longer safe, which the capacity
// policy classifies as exhausted — the probe must report that as down.
func TestFilesystemStorageProbe_StorePingFailureIsDown(t *testing.T) {
	result := FilesystemStorageProbe{
		Path:  t.TempDir(),
		Usage: fixedUsage(10, 10),
		Ping:  func(context.Context) error { return errors.New("database is locked") },
	}.probe(context.Background())

	if result.State != HealthStateDown {
		t.Fatalf("state = %q, want down", result.State)
	}
	if !result.IncidentActive {
		t.Error("exhausted storage did not raise an active incident")
	}
}

func TestFilesystemStorageProbe_IsAlwaysCritical(t *testing.T) {
	probe := FilesystemStorageProbe{Path: t.TempDir()}.DependencyProbe()
	if !probe.Critical {
		t.Fatal("filesystem probe is not critical; EvaluateHealthSnapshot needs a critical component")
	}
	if probe.Name != "mills-store-filesystem" {
		t.Fatalf("default name = %q", probe.Name)
	}
}

func TestStorageStateToHealthState(t *testing.T) {
	tests := []struct {
		state          StorageHealthState
		blockOnWarning bool
		want           HealthState
	}{
		{StorageHealthStateNormal, false, HealthStateHealthy},
		// Warning is advisory by default: 80% used should surface in the HUD
		// without halting every pipeline.
		{StorageHealthStateWarning, false, HealthStateHealthy},
		{StorageHealthStateWarning, true, HealthStateDegraded},
		{StorageHealthStateCritical, false, HealthStateDegraded},
		{StorageHealthStateExhausted, false, HealthStateDown},
	}
	for _, tc := range tests {
		if got := storageStateToHealthState(tc.state, tc.blockOnWarning); got != tc.want {
			t.Errorf("storageStateToHealthState(%q, %t) = %q, want %q", tc.state, tc.blockOnWarning, got, tc.want)
		}
	}
}

func TestHTTPDependencyProbe_HealthyOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := HTTPDependencyProbe{Name: "qdrant", URL: srv.URL}.probe(context.Background())
	if result.State != HealthStateHealthy {
		t.Fatalf("state = %q (%s), want healthy", result.State, result.Error)
	}
}

func TestHTTPDependencyProbe_DownOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	result := HTTPDependencyProbe{Name: "qdrant", URL: srv.URL}.probe(context.Background())
	if result.State != HealthStateDown {
		t.Fatalf("state = %q, want down", result.State)
	}
}

func TestHTTPDependencyProbe_DownOnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	result := HTTPDependencyProbe{Name: "qdrant", URL: url}.probe(context.Background())
	if result.State != HealthStateDown {
		t.Fatalf("state = %q, want down", result.State)
	}
}

func TestHTTPDependencyProbe_UnconfiguredURLIsUnknown(t *testing.T) {
	result := HTTPDependencyProbe{Name: "qdrant"}.probe(context.Background())
	if result.State != HealthStateUnknown {
		t.Fatalf("state = %q, want unknown", result.State)
	}
}

// The cache must hand back the original timestamps. If it restamped them, a
// stale probe would look fresh and EvaluateHealthSnapshot could never catch it.
func TestCachedStorageEvaluator_ReusesSnapshotWithOriginalTimestamps(t *testing.T) {
	probed := 0
	stamp := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	inner := storageHealthFunc(func(context.Context) (HealthSnapshot, error) {
		probed++
		return HealthSnapshot{
			ObservedAt: stamp,
			Components: []HealthComponent{{Name: "s", State: HealthStateHealthy, Critical: true, CheckedAt: stamp}},
		}, nil
	})

	now := stamp
	c := &CachedStorageEvaluator{Evaluator: inner, TTL: time.Minute, Now: func() time.Time { return now }}

	first, err := c.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("first EvaluateStorageHealth() error = %v", err)
	}
	now = stamp.Add(30 * time.Second)
	second, err := c.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("cached EvaluateStorageHealth() error = %v", err)
	}
	if probed != 1 {
		t.Fatalf("probed %d times within TTL, want 1", probed)
	}
	if !second.Components[0].CheckedAt.Equal(first.Components[0].CheckedAt) {
		t.Fatal("cache restamped CheckedAt; stale evidence would look fresh")
	}

	now = stamp.Add(2 * time.Minute)
	if _, err := c.EvaluateStorageHealth(context.Background()); err != nil {
		t.Fatalf("post-TTL EvaluateStorageHealth() error = %v", err)
	}
	if probed != 2 {
		t.Fatalf("probed %d times after TTL expiry, want 2", probed)
	}
}

func TestCachedStorageEvaluator_StaleCacheStillBlocks(t *testing.T) {
	stamp := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	c := &CachedStorageEvaluator{
		// A TTL longer than the gate's staleness bound is a misconfiguration;
		// the gate must still reject the evidence rather than trust the cache.
		TTL: time.Hour,
		Now: func() time.Time { return stamp },
		Evaluator: storageHealthFunc(func(context.Context) (HealthSnapshot, error) {
			return HealthSnapshot{
				ObservedAt: stamp,
				Components: []HealthComponent{{Name: "s", State: HealthStateHealthy, Critical: true, CheckedAt: stamp}},
			}, nil
		}),
	}

	snapshot, err := c.EvaluateStorageHealth(context.Background())
	if err != nil {
		t.Fatalf("EvaluateStorageHealth() error = %v", err)
	}
	decision := EvaluateHealthSnapshot(snapshot, stamp.Add(30*time.Minute))
	if decision.Allowed || !decision.FailClosed {
		t.Fatalf("decision = %+v, want fail-closed block on stale evidence", decision)
	}
}

func TestCachedStorageEvaluator_NilInnerErrors(t *testing.T) {
	if _, err := (&CachedStorageEvaluator{}).EvaluateStorageHealth(context.Background()); err == nil {
		t.Fatal("nil inner evaluator returned no error")
	}
}

// End-to-end: the production evaluator feeding the real GateRunner admits work
// when storage is healthy and config is safe.
func TestProductionEvaluatorAdmitsHealthyEnvironment(t *testing.T) {
	dir := t.TempDir()
	storage := &CompositeStorageEvaluator{
		Probes: []DependencyProbe{FilesystemStorageProbe{Path: dir, Usage: fixedUsage(10, 10)}.DependencyProbe()},
	}
	config := LocalConfigChecker{Checks: []LocalConfigCheck{WritableDirCheck("store dir", dir)}}

	result := GateRunner{StorageHealth: storage, LocalConfig: config}.Run(context.Background())
	if !result.Allowed {
		t.Fatalf("Run() = %+v, want allowed", result)
	}
	if result.Classification != GateClassificationOK {
		t.Fatalf("classification = %q, want ok", result.Classification)
	}
}

func TestProductionEvaluatorBlocksOnUnwritableStore(t *testing.T) {
	storage := &CompositeStorageEvaluator{
		Probes: []DependencyProbe{FilesystemStorageProbe{
			Path:  t.TempDir(),
			Usage: fixedUsage(10, 10),
			Ping:  func(context.Context) error { return errors.New("disk I/O error") },
		}.DependencyProbe()},
	}
	config := LocalConfigChecker{Checks: []LocalConfigCheck{{
		Name: "never reached", Check: func(context.Context) error { return nil },
	}}}

	result := GateRunner{StorageHealth: storage, LocalConfig: config}.Run(context.Background())
	if result.Allowed {
		t.Fatal("Run() allowed work on a store that cannot be pinged")
	}
	if result.Classification != GateClassificationStorageHealth {
		t.Fatalf("classification = %q, want storage_health", result.Classification)
	}
	if result.PipelineClass != "infra" {
		t.Fatalf("pipeline class = %q, want infra", result.PipelineClass)
	}
}

// Regression: GateRunner used to sample the clock before invoking the
// evaluator, so evidence stamped by any real probe landed in the future
// relative to it and EvaluateHealthSnapshot rejected it as stale — producing
// the nonsensical "age 0s exceeds 5m0s" and blocking every pipeline. Only
// fakes returning fixed past timestamps passed. This pins the ordering by
// using an evaluator that stamps wall-clock time, as production probes do.
func TestGateRunner_AcceptsEvidenceStampedDuringEvaluation(t *testing.T) {
	storage := storageHealthFunc(func(context.Context) (HealthSnapshot, error) {
		stamped := time.Now().UTC()
		return HealthSnapshot{
			ObservedAt: stamped,
			Components: []HealthComponent{{
				Name: "mills-store", State: HealthStateHealthy, Critical: true, CheckedAt: stamped,
			}},
		}, nil
	})
	config := configPreflightFunc(func(context.Context) (LocalConfigResult, error) {
		return LocalConfigResult{Safe: true}, nil
	})

	result := GateRunner{StorageHealth: storage, LocalConfig: config}.Run(context.Background())
	if !result.Allowed {
		t.Fatalf("Run() = %+v, want allowed for freshly stamped evidence", result)
	}
}

func TestStorageDirFor(t *testing.T) {
	if got := StorageDirFor("/var/lib/loom-mills/state.db"); got != "/var/lib/loom-mills" {
		t.Errorf("StorageDirFor() = %q", got)
	}
	if got := StorageDirFor("  "); got != "" {
		t.Errorf("StorageDirFor(blank) = %q, want empty", got)
	}
}
