package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/runner"
)

type stubPreflight struct {
	decision gates.HealthDecision
	err      error
	calls    int
}

func (s *stubPreflight) DecideHealthGates(context.Context) (gates.HealthDecision, error) {
	s.calls++
	return s.decision, s.err
}

func TestResolveHealthGateMode(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"", healthGateModeObserve},
		{"observe", healthGateModeObserve},
		{"enforce", healthGateModeEnforce},
		{"ENFORCE", healthGateModeEnforce},
		{"  off  ", healthGateModeOff},
		{"nonsense", healthGateModeObserve},
	}
	for _, tc := range tests {
		t.Setenv(healthGateModeEnv, tc.value)
		if got := resolveHealthGateMode(); got != tc.want {
			t.Errorf("resolveHealthGateMode(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// Observe mode must never block a pipeline, but it must keep the real reasons
// so the preflight event records that a block was suppressed rather than
// claiming the dependencies were healthy.
func TestObserveOnlyPreflight_AllowsButPreservesReasons(t *testing.T) {
	inner := &stubPreflight{decision: gates.HealthDecision{
		Allowed: false, FailClosed: true, Status: "block",
		Reasons: []string{"critical dependency mills-store is down"},
	}}

	decision, err := observeOnlyPreflight{inner: inner}.DecideHealthGates(context.Background())
	if err != nil {
		t.Fatalf("DecideHealthGates() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatal("observe mode blocked a pipeline")
	}
	if decision.FailClosed {
		t.Error("observe mode reported fail-closed")
	}
	if decision.Status != healthGateModeObserve {
		t.Errorf("status = %q, want observe", decision.Status)
	}
	if len(decision.Reasons) != 1 {
		t.Errorf("reasons = %v, want the suppressed block reason preserved", decision.Reasons)
	}
}

func TestObserveOnlyPreflight_AllowsOnEvaluatorError(t *testing.T) {
	inner := &stubPreflight{err: context.DeadlineExceeded}
	decision, err := observeOnlyPreflight{inner: inner}.DecideHealthGates(context.Background())
	if err != nil {
		t.Fatalf("DecideHealthGates() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatal("observe mode blocked on an evaluator error")
	}
	if len(decision.Reasons) == 0 {
		t.Error("evaluator error was not recorded in the reasons")
	}
}

func TestHealthGateWiring_RunnerPreflightHonoursMode(t *testing.T) {
	blocking := &stubPreflight{decision: gates.HealthDecision{Allowed: false, Status: "block"}}

	enforce := &healthGateWiring{Mode: healthGateModeEnforce, Preflight: blocking}
	decision, _ := enforce.runnerPreflight().DecideHealthGates(context.Background())
	if decision.Allowed {
		t.Error("enforce mode allowed a blocked verdict")
	}

	observe := &healthGateWiring{Mode: healthGateModeObserve, Preflight: blocking}
	decision, _ = observe.runnerPreflight().DecideHealthGates(context.Background())
	if !decision.Allowed {
		t.Error("observe mode blocked a pipeline")
	}
}

// A nil wiring is the "gates off" path; the runners must end up with a nil
// interface so the preflight short-circuits exactly as it did before.
func TestHealthGateWiring_NilYieldsNilPreflight(t *testing.T) {
	var w *healthGateWiring
	if got := w.runnerPreflight(); got != nil {
		t.Fatalf("runnerPreflight() = %v, want nil", got)
	}
	if w.enforcing() {
		t.Error("nil wiring reported enforcing")
	}
	decision, err := w.decide(context.Background())
	if err != nil {
		t.Fatalf("decide() error = %v", err)
	}
	if decision.Allowed {
		t.Error("nil wiring returned an allowing decision")
	}
}

// pinStorageUsage makes the composed gates read a fixed capacity for the test,
// so the assertion is about the wiring rather than the host's free space.
func pinStorageUsage(t *testing.T, capacity, inodes float64) {
	t.Helper()
	previous := storageUsage
	storageUsage = func(string) (float64, float64, error) { return capacity, inodes, nil }
	t.Cleanup(func() { storageUsage = previous })
}

func TestBuildHealthGates_OffReturnsNil(t *testing.T) {
	t.Setenv(healthGateModeEnv, healthGateModeOff)
	if got := buildHealthGates(Config{}, nil, nil, discardLogger()); got != nil {
		t.Fatalf("buildHealthGates() = %+v, want nil when disabled", got)
	}
}

// The composed gates must admit a healthy environment. This is the end-to-end
// proof that the production evaluators, not just their unit tests, produce an
// allowing verdict for a well-formed operator.
func TestBuildHealthGates_AdmitsHealthyEnvironment(t *testing.T) {
	pinStorageUsage(t, 10, 10)
	t.Setenv(healthGateModeEnv, healthGateModeEnforce)
	dir := t.TempDir()
	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("seed .git: %v", err)
	}

	w := buildHealthGates(Config{
		DBPath:   filepath.Join(dir, "state.db"),
		RepoRoot: repoRoot,
	}, nil, nil, discardLogger())
	if w == nil {
		t.Fatal("buildHealthGates() = nil")
	}
	if !w.enforcing() {
		t.Fatal("enforce mode did not resolve")
	}

	decision, err := w.decide(context.Background())
	if err != nil {
		t.Fatalf("decide() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("healthy environment blocked: %+v", decision.Reasons)
	}
}

// A repo root that is not a git working tree is a local-config block, not a
// storage one — the classification drives which owner the escalation names.
func TestBuildHealthGates_BlocksOnBadRepoRoot(t *testing.T) {
	pinStorageUsage(t, 10, 10)
	t.Setenv(healthGateModeEnv, healthGateModeEnforce)
	dir := t.TempDir()

	w := buildHealthGates(Config{
		DBPath:   filepath.Join(dir, "state.db"),
		RepoRoot: t.TempDir(), // exists but has no .git
	}, nil, nil, discardLogger())

	decision, err := w.decide(context.Background())
	if err != nil {
		t.Fatalf("decide() error = %v", err)
	}
	if decision.Allowed {
		t.Fatal("a repo root with no .git was admitted")
	}
	if !containsSubstring(decision.Reasons, "config") {
		t.Errorf("reasons = %v, want a config-classified block", decision.Reasons)
	}
}

func TestBuildHealthGates_HubProbeIsNonCritical(t *testing.T) {
	pinStorageUsage(t, 10, 10)
	t.Setenv(healthGateModeEnv, healthGateModeEnforce)
	dir := t.TempDir()
	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("seed .git: %v", err)
	}

	// AutonomyGate already blocks on hub readiness; an unhealthy hub here must
	// colour the verdict without double-blocking the pipeline.
	w := buildHealthGates(Config{
		DBPath:   filepath.Join(dir, "state.db"),
		RepoRoot: repoRoot,
	}, nil, func() (bool, string) { return false, "hub session not established" }, discardLogger())

	decision, err := w.decide(context.Background())
	if err != nil {
		t.Fatalf("decide() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("a degraded hub blocked the pipeline: %+v", decision.Reasons)
	}
	if len(decision.DegradedDependencies) == 0 {
		t.Error("degraded hub was not reported as a degraded dependency")
	}
}

// The status endpoint must publish health_gates; its absence is what left the
// HUD tile rendering its fail-closed default.
func TestStatus_PublishesHealthGates(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	checked := gates.HealthDecision{
		Allowed: true, Status: "pass",
		Components: []gates.HealthComponent{{Name: "mills-store", State: gates.HealthStateHealthy, Critical: true}},
	}
	op.withHealthGates(&healthGateWiring{
		Mode:      healthGateModeObserve,
		Preflight: &stubPreflight{decision: checked},
	})

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		HealthGates *gates.HealthGateReport `json:"health_gates"`
		Mode        string                  `json:"health_gates_mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.HealthGates == nil {
		t.Fatal("status payload omitted health_gates")
	}
	if !resp.HealthGates.Allowed || resp.HealthGates.Status != "pass" {
		t.Errorf("health_gates = %+v", resp.HealthGates)
	}
	if len(resp.HealthGates.Components) != 1 {
		t.Errorf("components = %+v", resp.HealthGates.Components)
	}
	if resp.Mode != healthGateModeObserve {
		t.Errorf("health_gates_mode = %q, want observe", resp.Mode)
	}
}

// Observe mode must still publish the honest verdict: the tile should show the
// block even while the pipeline is allowed to run.
func TestStatus_PublishesHonestVerdictInObserveMode(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	blocking := &stubPreflight{decision: gates.HealthDecision{
		Allowed: false, FailClosed: true, Status: "block",
		Reasons: []string{"critical dependency mills-store is down"},
	}}
	w := &healthGateWiring{Mode: healthGateModeObserve, Preflight: blocking}
	op.withHealthGates(w)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))

	var resp struct {
		HealthGates *gates.HealthGateReport `json:"health_gates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.HealthGates == nil {
		t.Fatal("status payload omitted health_gates")
	}
	if resp.HealthGates.Allowed {
		t.Error("observe mode published an allowing verdict; the tile would hide a real block")
	}
	// ...while the runner-facing preflight still allows the pipeline through.
	decision, _ := w.runnerPreflight().DecideHealthGates(context.Background())
	if !decision.Allowed {
		t.Error("observe mode blocked the pipeline")
	}
}

func TestStatus_OmitsHealthGatesWhenDisabled(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["health_gates"]; ok {
		t.Error("health_gates present although the gates are not wired")
	}
}

// main.go installs one preflight value on both runners, but the pipeline lane
// and the council lane declare their gate interfaces independently. This pins
// that the single value satisfies both, so a signature change on either side
// fails here rather than at the next operator build.
func TestHealthGateWiring_SatisfiesBothRunnerInterfaces(t *testing.T) {
	w := &healthGateWiring{Mode: healthGateModeEnforce, Preflight: &stubPreflight{}}
	preflight := w.runnerPreflight()

	var pipelineRunner pipeline.Runner
	pipelineRunner.HealthGates = preflight

	var councilRunner runner.Runner
	councilRunner.HealthGates = preflight

	if pipelineRunner.HealthGates == nil || councilRunner.HealthGates == nil {
		t.Fatal("preflight did not install on both runners")
	}
}

func containsSubstring(values []string, want string) bool {
	for _, v := range values {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
