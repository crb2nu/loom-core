package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// healthGateModeEnv selects how the infrastructure admission gates behave.
const healthGateModeEnv = "LOOM_MILLS_HEALTH_GATES"

const (
	// healthGateModeOff disables the gates entirely: no probes run and the
	// status endpoint omits health_gates.
	healthGateModeOff = "off"

	// healthGateModeObserve runs every probe and publishes the verdict to the
	// status endpoint and the HUD tile, but never blocks a pipeline. It is
	// the default: the admission chain has not previously run in production,
	// and a fail-closed gate whose probes have never been exercised against a
	// live cluster must prove itself green before it is allowed to halt work.
	healthGateModeObserve = "observe"

	// healthGateModeEnforce lets a blocked verdict escalate the run.
	healthGateModeEnforce = "enforce"
)

// healthGateCacheTTL bounds how often the probes run. The pipeline preflight
// and the HUD status endpoint share one evaluator, so without a cache every
// HUD poll would re-probe every dependency. It is far below the gate's own
// staleness bound, so cached evidence is always fresh enough to be trusted —
// and when it is not, EvaluateHealthSnapshot rejects it rather than the cache
// hiding the gap.
const healthGateCacheTTL = 20 * time.Second

// healthGateWiring is the operator's handle on the admission gates.
type healthGateWiring struct {
	// Mode is the resolved mode string, surfaced on the status endpoint so
	// an operator can tell an advisory verdict from an enforcing one.
	Mode string

	// Preflight always reports the honest verdict, including in observe
	// mode. The status endpoint reads this one so the HUD tile shows what
	// the gate would decide, not what it was allowed to do.
	Preflight pipeline.HealthGatePreflight
}

// storageUsage overrides the capacity reader the composed gates use. It is nil
// in production, which makes the probe fall back to the platform statfs call.
// Tests set it to pin capacity, so their assertions are about the composition
// rather than about how full the host's disk happens to be.
var storageUsage func(path string) (capacityUsedPercent, inodeUsedPercent float64, err error)

// resolveHealthGateMode reads the mode env var, defaulting to observe.
func resolveHealthGateMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(healthGateModeEnv))) {
	case healthGateModeOff:
		return healthGateModeOff
	case healthGateModeEnforce:
		return healthGateModeEnforce
	default:
		return healthGateModeObserve
	}
}

// buildHealthGates composes the production admission gates. It returns nil
// when the gates are disabled, which leaves Runner.HealthGates nil and the
// preflight a no-op exactly as before this wiring existed.
func buildHealthGates(cfg Config, st *store.Store, hubHealth func() (bool, string), logger *slog.Logger) *healthGateWiring {
	mode := resolveHealthGateMode()
	if mode == healthGateModeOff {
		logger.Warn("mills health gates disabled", "env", healthGateModeEnv)
		return nil
	}

	storageDir := gates.StorageDirFor(cfg.DBPath)
	probes := []gates.DependencyProbe{
		gates.FilesystemStorageProbe{
			Name:  "mills-store",
			Path:  storageDir,
			Ping:  storePing(st),
			Usage: storageUsage,
		}.DependencyProbe(),
	}

	// The MCP hub fronts the plan store and agent-context. It is reported as
	// non-critical here on purpose: the capability report already lists it as
	// required for autonomy and AutonomyGate blocks on it, so making it
	// critical here would double-block the same dependency while burying the
	// storage verdict this gate exists to deliver.
	if hubHealth != nil {
		probes = append(probes, gates.DependencyProbe{
			Name: "mcp-hub-agent-context",
			Probe: func(context.Context) gates.ProbeResult {
				healthy, message := hubHealth()
				if healthy {
					return gates.ProbeResult{State: gates.HealthStateHealthy}
				}
				return gates.ProbeResult{
					State:       gates.HealthStateDegraded,
					Error:       message,
					Remediation: "check the MCP hub deployment and the operator's agent-context session",
				}
			},
		})
	}

	// Optional vector-DB probe. Unset means "not deployed here" rather than
	// "unhealthy", so an absent URL adds no component at all.
	if qdrantURL := strings.TrimSpace(os.Getenv("LOOM_MILLS_QDRANT_HEALTH_URL")); qdrantURL != "" {
		probes = append(probes, gates.HTTPDependencyProbe{
			Name:        "qdrant",
			URL:         qdrantURL,
			Critical:    false,
			Remediation: "check the qdrant deployment; recall quality degrades while it is down",
			Client:      &http.Client{Timeout: 5 * time.Second},
		}.DependencyProbe())
	}

	storage := &gates.CachedStorageEvaluator{
		Evaluator: &gates.CompositeStorageEvaluator{Probes: probes},
		TTL:       healthGateCacheTTL,
	}

	localConfig := gates.LocalConfigChecker{Checks: []gates.LocalConfigCheck{
		gates.WritableDirCheck("mills store directory", storageDir),
		gates.RepoRootCheck("repo root", cfg.RepoRoot),
	}}

	logger.Info("mills health gates wired",
		"mode", mode,
		"storage_dir", storageDir,
		"repo_root", cfg.RepoRoot,
		"probes", len(probes))

	return &healthGateWiring{
		Mode:      mode,
		Preflight: pipeline.NewFailClosedPreflight(storage, localConfig),
	}
}

// storePing adapts the canonical store's database handle into the filesystem
// probe's integrity check. A nil store yields a nil check rather than a
// failing one — the store's absence is already a startup error.
func storePing(st *store.Store) func(context.Context) error {
	if st == nil {
		return nil
	}
	db := st.DB()
	if db == nil {
		return nil
	}
	return db.PingContext
}

// enforcing reports whether a blocked verdict may stop a pipeline.
func (w *healthGateWiring) enforcing() bool {
	return w != nil && w.Mode == healthGateModeEnforce
}

// runnerPreflight returns the value to install on the pipeline and council
// runners. In observe mode the honest verdict is wrapped so it can never
// block; the unwrapped verdict still reaches the status endpoint.
func (w *healthGateWiring) runnerPreflight() pipeline.HealthGatePreflight {
	if w == nil || w.Preflight == nil {
		return nil
	}
	if w.enforcing() {
		return w.Preflight
	}
	return observeOnlyPreflight{inner: w.Preflight}
}

// decide returns the honest verdict for the status endpoint.
func (w *healthGateWiring) decide(ctx context.Context) (gates.HealthDecision, error) {
	if w == nil || w.Preflight == nil {
		return gates.HealthDecision{}, nil
	}
	return w.Preflight.DecideHealthGates(ctx)
}

// observeOnlyPreflight evaluates the real gates and then forces the verdict to
// allow. The reasons are preserved and the status is rewritten to "observe" so
// the pipeline's preflight event records that a real block was suppressed
// rather than claiming the dependencies were healthy.
type observeOnlyPreflight struct {
	inner pipeline.HealthGatePreflight
}

func (p observeOnlyPreflight) DecideHealthGates(ctx context.Context) (gates.HealthDecision, error) {
	decision, err := p.inner.DecideHealthGates(ctx)
	if err != nil {
		decision = gates.HealthDecision{Reasons: []string{"health gates unavailable: " + err.Error()}}
	}
	decision.Allowed = true
	decision.FailClosed = false
	decision.Status = healthGateModeObserve
	return decision, nil
}
