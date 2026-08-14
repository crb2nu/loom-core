package gates

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultProbeTimeout bounds one dependency probe. It is deliberately shorter
// than the pipeline's stage budgets: a hung probe must surface as unknown
// evidence (which blocks) rather than stalling the admission gate itself.
const defaultProbeTimeout = 5 * time.Second

// ProbeResult is one dependency probe's observation. A zero value is
// deliberately HealthStateUnknown ("") so a probe that forgets to set a state
// blocks instead of being read as healthy.
type ProbeResult struct {
	State          HealthState
	Error          string
	Remediation    string
	IncidentID     string
	IncidentActive bool
}

// DependencyProbe is one named infrastructure check contributed to the
// admission snapshot. Critical probes must be healthy and fresh for autonomy
// to proceed; non-critical probes only colour the operational state.
type DependencyProbe struct {
	Name     string
	Critical bool
	Probe    func(ctx context.Context) ProbeResult
}

// CompositeStorageEvaluator is the production gates.StorageHealthEvaluator. It
// runs every probe concurrently and stamps each component with the instant its
// own probe returned, so EvaluateHealthSnapshot can enforce per-dependency
// freshness rather than trusting one bundle-level timestamp.
type CompositeStorageEvaluator struct {
	Probes  []DependencyProbe
	MaxAge  time.Duration
	Timeout time.Duration
	Now     func() time.Time
}

// EvaluateStorageHealth satisfies StorageHealthEvaluator. It never returns an
// error: a failed probe is evidence (an unhealthy component), not an
// evaluator malfunction, and reporting it as a component keeps the blocking
// reason attributable to the dependency that actually broke.
func (e *CompositeStorageEvaluator) EvaluateStorageHealth(ctx context.Context) (HealthSnapshot, error) {
	now := time.Now().UTC
	if e.Now != nil {
		now = e.Now
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	components := make([]HealthComponent, len(e.Probes))
	var wg sync.WaitGroup
	for i, probe := range e.Probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			components[i] = runDependencyProbe(ctx, probe, timeout, now)
		}()
	}
	wg.Wait()

	sort.SliceStable(components, func(i, j int) bool { return components[i].Name < components[j].Name })
	return HealthSnapshot{Components: components, ObservedAt: now().UTC(), MaxAge: e.MaxAge}, nil
}

// runDependencyProbe executes one probe under its own timeout and converts a
// panic or missing probe function into unknown evidence. CheckedAt is stamped
// after the probe returns so a slow probe cannot claim fresher evidence than
// it actually observed.
func runDependencyProbe(ctx context.Context, probe DependencyProbe, timeout time.Duration, now func() time.Time) (component HealthComponent) {
	name := strings.TrimSpace(probe.Name)
	component = HealthComponent{Name: name, Critical: probe.Critical, State: HealthStateUnknown}
	defer func() {
		if rec := recover(); rec != nil {
			component.State = HealthStateUnknown
			component.Error = fmt.Sprintf("health probe panicked: %v", rec)
		}
		component.CheckedAt = now().UTC()
	}()

	if probe.Probe == nil {
		component.Error = "health probe is not configured"
		return component
	}

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := probe.Probe(pctx)

	component.State = normalizeHealthState(result.State)
	component.Error = strings.TrimSpace(result.Error)
	component.Remediation = strings.TrimSpace(result.Remediation)
	component.IncidentID = strings.TrimSpace(result.IncidentID)
	component.IncidentActive = result.IncidentActive
	return component
}

// FilesystemStorageProbe observes the filesystem backing a Mills data
// directory: capacity, inodes, and whether a real write still succeeds. It is
// the probe that catches the failure mode SQLite cannot survive — a full or
// read-only volume — before the pipeline starts mutating the canonical store.
type FilesystemStorageProbe struct {
	// Name is the component name reported to the gate and the HUD.
	Name string

	// Path is the directory to measure and write-probe. The SQLite store's
	// directory is the meaningful target: measuring the mount that actually
	// receives Mills writes is the point.
	Path string

	// Policy supplies the capacity thresholds. The zero value uses the
	// documented 80% warning / 90% critical defaults.
	Policy StorageHealthPolicy

	// Ping, when set, is an integrity check for the store itself (a SQLite
	// PingContext). A failure is treated as storage exhaustion because
	// mutations are no longer safe.
	Ping func(ctx context.Context) error

	// Usage reads byte and inode usage percentages for Path. Nil uses the
	// platform statfs implementation. It exists so tests can pin capacity
	// instead of asserting against whatever the host disk happens to hold.
	Usage func(path string) (capacityUsedPercent, inodeUsedPercent float64, err error)

	// BlockOnWarning promotes the warning state (default: 80% used) from
	// advisory to blocking. It is off by default: halting all autonomy at
	// 80% disk trades a real outage for a threshold that is meant to prompt
	// a cleanup, while the critical and exhausted states — where SQLite
	// corruption risk is real — always block.
	BlockOnWarning bool
}

// DependencyProbe adapts the filesystem probe into a critical dependency.
// Storage is always critical: EvaluateHealthSnapshot requires at least one
// critical component, and a mill that cannot write its own store cannot
// safely do anything else either.
func (p FilesystemStorageProbe) DependencyProbe() DependencyProbe {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "mills-store-filesystem"
	}
	return DependencyProbe{Name: name, Critical: true, Probe: p.probe}
}

func (p FilesystemStorageProbe) probe(ctx context.Context) ProbeResult {
	path := strings.TrimSpace(p.Path)
	if path == "" {
		return ProbeResult{
			State:       HealthStateUnknown,
			Error:       "storage path is not configured",
			Remediation: "set the operator db-path so the storage gate can measure the volume Mills writes to",
		}
	}

	usage := p.Usage
	if usage == nil {
		usage = statfsUsage
	}
	snapshot := StorageHealthSnapshot{}
	capacityUsed, inodeUsed, err := usage(path)
	if err != nil {
		return ProbeResult{
			State:       HealthStateUnknown,
			Error:       fmt.Sprintf("cannot stat filesystem at %s: %v", path, err),
			Remediation: "verify the Mills data volume is mounted and readable",
		}
	}
	snapshot.CapacityUsedPercent = capacityUsed
	snapshot.InodeUsedPercent = inodeUsed

	if writeErr := probeWritable(path); writeErr != nil {
		if isReadOnlyError(writeErr) {
			snapshot.ReadOnly = true
		} else {
			snapshot.WriteError = writeErr.Error()
		}
	}
	if p.Ping != nil {
		if err := p.Ping(ctx); err != nil {
			snapshot.SQLiteError = err.Error()
		}
	}

	verdict := EvaluateStorageHealthPolicy(p.Policy, snapshot)
	result := ProbeResult{
		State:       storageStateToHealthState(verdict.State, p.BlockOnWarning),
		Remediation: strings.TrimSpace(verdict.Classification.Reason),
	}
	if verdict.State != StorageHealthStateNormal {
		result.Error = fmt.Sprintf("%s (%.1f%% used at %s)", verdict.Classification.Reason, verdict.UsedPercent, path)
		result.Remediation = "free space on the Mills data volume or expand the PVC"
	}
	if verdict.Classification.RequiresManualRecovery {
		result.IncidentActive = true
		result.IncidentID = "storage-" + string(verdict.State)
	}
	return result
}

// storageStateToHealthState maps the capacity policy's state onto the
// admission gate's vocabulary. Warning is reported as degraded-but-healthy
// unless the operator opted into blocking: the component still carries its
// error text, so the HUD shows the pressure without the mill stopping.
func storageStateToHealthState(state StorageHealthState, blockOnWarning bool) HealthState {
	switch state {
	case StorageHealthStateNormal:
		return HealthStateHealthy
	case StorageHealthStateWarning:
		if blockOnWarning {
			return HealthStateDegraded
		}
		return HealthStateHealthy
	case StorageHealthStateCritical:
		return HealthStateDegraded
	default:
		return HealthStateDown
	}
}

// probeWritable proves the volume still accepts a write. statfs alone is not
// enough: a read-only remount and a quota-exhausted volume both report ample
// free space while every SQLite write fails.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".mills-health-*.probe")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := f.WriteString("health"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func isReadOnlyError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "read-only")
}

// HTTPDependencyProbe checks a dependency's HTTP health endpoint — the plan
// store and agent-context behind the MCP hub, and the vector DB. A non-2xx
// response or a transport failure is reported as down.
type HTTPDependencyProbe struct {
	// Name is the component name reported to the gate and the HUD.
	Name string

	// URL is the health endpoint to GET.
	URL string

	// Critical marks the dependency as required for autonomy. Leave it false
	// for dependencies whose loss degrades quality but not correctness (the
	// vector DB), and true for ones the pipeline cannot proceed without.
	Critical bool

	// Remediation is the operator hint attached when the probe fails.
	Remediation string

	// Client is the HTTP client. Nil uses a client bounded by the probe
	// timeout the evaluator applies.
	Client *http.Client
}

// DependencyProbe adapts the HTTP check into a dependency probe.
func (p HTTPDependencyProbe) DependencyProbe() DependencyProbe {
	return DependencyProbe{Name: strings.TrimSpace(p.Name), Critical: p.Critical, Probe: p.probe}
}

func (p HTTPDependencyProbe) probe(ctx context.Context) ProbeResult {
	url := strings.TrimSpace(p.URL)
	if url == "" {
		return ProbeResult{
			State:       HealthStateUnknown,
			Error:       "health endpoint is not configured",
			Remediation: p.Remediation,
		}
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{State: HealthStateUnknown, Error: err.Error(), Remediation: p.Remediation}
	}
	req.Header.Set("User-Agent", "loom-mills-operator/health-gate")

	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{State: HealthStateDown, Error: err.Error(), Remediation: p.Remediation}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProbeResult{
			State:       HealthStateDown,
			Error:       fmt.Sprintf("health endpoint %s returned HTTP %d", url, resp.StatusCode),
			Remediation: p.Remediation,
		}
	}
	return ProbeResult{State: HealthStateHealthy}
}

// CachedStorageEvaluator memoises a snapshot for TTL. The cached snapshot is
// returned with its original timestamps untouched, exactly as
// StorageHealthEvaluator requires: freshness stays EvaluateHealthSnapshot's
// decision, so a cache outliving MaxAge blocks instead of masking staleness.
// It exists so the pipeline preflight and the HUD status endpoint can share
// one round of probes instead of each re-probing every dependency.
type CachedStorageEvaluator struct {
	Evaluator StorageHealthEvaluator
	TTL       time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	cached   HealthSnapshot
	cachedAt time.Time
	hasValue bool
}

// EvaluateStorageHealth returns the cached snapshot while it is younger than
// TTL and otherwise re-probes.
func (c *CachedStorageEvaluator) EvaluateStorageHealth(ctx context.Context) (HealthSnapshot, error) {
	if c.Evaluator == nil {
		return HealthSnapshot{}, fmt.Errorf("cached storage evaluator has no underlying evaluator")
	}
	now := time.Now().UTC
	if c.Now != nil {
		now = c.Now
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasValue && c.TTL > 0 && now().Sub(c.cachedAt) < c.TTL {
		return c.cached, nil
	}

	snapshot, err := c.Evaluator.EvaluateStorageHealth(ctx)
	if err != nil {
		return HealthSnapshot{}, err
	}
	c.cached, c.cachedAt, c.hasValue = snapshot, now(), true
	return snapshot, nil
}

// StorageDirFor returns the directory a store path lives in, which is the unit
// the filesystem probe measures. An empty path yields an empty result so the
// probe reports a configuration problem rather than silently measuring ".".
func StorageDirFor(dbPath string) string {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return ""
	}
	return filepath.Dir(dbPath)
}
