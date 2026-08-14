package gates

import (
	"sort"
	"strings"
	"time"
)

const (
	FlakinessCodeOK               = "ok"
	FlakinessCodeInsufficientData = "insufficient_data"
	FlakinessCodeFlakyGate        = "flaky_gate"
)

// FlakinessPolicy configures deterministic flaky-gate classification.
type FlakinessPolicy struct {
	// MinRuns is the minimum recent sample size before a gate can be declared
	// flaky. Default: 5.
	MinRuns int
	// MaxFailureRate is the tolerated fail/(pass+fail) ratio. Default: 0.20.
	MaxFailureRate float64
	// MinTransitions is the minimum pass/fail alternation count that indicates
	// instability rather than a consistently broken gate. Default: 2.
	MinTransitions int
	// Window limits analysis to recent outcomes when set.
	Window time.Duration
	Now    func() time.Time
}

// GateRun is one historical gate outcome.
type GateRun struct {
	GateName string
	Passed   bool
	Skipped  bool
	At       time.Time
}

// FlakinessVerdict is the machine-readable output for flaky-gate policy.
type FlakinessVerdict struct {
	Pass        bool               `json:"pass"`
	Code        string             `json:"code"`
	GateName    string             `json:"gate_name,omitempty"`
	Reasons     []string           `json:"reasons,omitempty"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	FlakyGates  []string           `json:"flaky_gates,omitempty"`
	SampleCount int                `json:"sample_count"`
}

// DefaultFlakinessPolicy returns conservative thresholds for flake detection.
func DefaultFlakinessPolicy() FlakinessPolicy {
	return FlakinessPolicy{MinRuns: 5, MaxFailureRate: 0.20, MinTransitions: 2}
}

// EvaluateGateFlakiness classifies recent gate history. It fails only when the
// signal is strong enough to distinguish flakiness from a single deterministic
// failure.
func EvaluateGateFlakiness(policy FlakinessPolicy, runs []GateRun) FlakinessVerdict {
	policy = normalizeFlakinessPolicy(policy)
	runs = filterFlakinessWindow(policy, runs)
	if len(runs) < policy.MinRuns {
		return insufficientFlakinessVerdict(policy, len(runs), 0)
	}

	byGate := map[string][]GateRun{}
	for _, r := range runs {
		if r.Skipped {
			continue
		}
		name := strings.TrimSpace(r.GateName)
		if name == "" {
			name = "unknown"
		}
		byGate[name] = append(byGate[name], r)
	}

	var flaky []string
	var reasons []string
	metrics := map[string]float64{"min_runs": float64(policy.MinRuns)}
	evaluableGates := 0
	for name, gateRuns := range byGate {
		if len(gateRuns) < policy.MinRuns {
			continue
		}
		evaluableGates++
		sort.SliceStable(gateRuns, func(i, j int) bool { return gateRuns[i].At.Before(gateRuns[j].At) })
		failures, transitions := flakinessCounts(gateRuns)
		failureRate := float64(failures) / float64(len(gateRuns))
		metrics[name+".sample_count"] = float64(len(gateRuns))
		metrics[name+".failure_rate"] = failureRate
		metrics[name+".transitions"] = float64(transitions)
		if failureRate > policy.MaxFailureRate && transitions >= policy.MinTransitions {
			flaky = append(flaky, name)
			reasons = append(reasons, "gate "+name+" exceeded failure-rate and transition thresholds")
		}
	}
	if evaluableGates == 0 {
		return insufficientFlakinessVerdict(policy, len(runs), len(byGate))
	}
	sort.Strings(flaky)
	if len(flaky) == 0 {
		return FlakinessVerdict{Pass: true, Code: FlakinessCodeOK, Metrics: metrics, SampleCount: len(runs)}
	}
	return FlakinessVerdict{
		Pass:        false,
		Code:        FlakinessCodeFlakyGate,
		GateName:    flaky[0],
		Reasons:     reasons,
		Metrics:     metrics,
		FlakyGates:  flaky,
		SampleCount: len(runs),
	}
}

func insufficientFlakinessVerdict(policy FlakinessPolicy, sampleCount, gateCount int) FlakinessVerdict {
	return FlakinessVerdict{
		Pass:        true,
		Code:        FlakinessCodeInsufficientData,
		SampleCount: sampleCount,
		Metrics: map[string]float64{
			"gate_count": float64(gateCount),
			"min_runs":   float64(policy.MinRuns),
		},
	}
}

func normalizeFlakinessPolicy(policy FlakinessPolicy) FlakinessPolicy {
	def := DefaultFlakinessPolicy()
	if policy.MinRuns <= 0 {
		policy.MinRuns = def.MinRuns
	}
	if policy.MaxFailureRate <= 0 || policy.MaxFailureRate >= 1 {
		policy.MaxFailureRate = def.MaxFailureRate
	}
	if policy.MinTransitions <= 0 {
		policy.MinTransitions = def.MinTransitions
	}
	return policy
}

func filterFlakinessWindow(policy FlakinessPolicy, runs []GateRun) []GateRun {
	if policy.Window <= 0 {
		return append([]GateRun(nil), runs...)
	}
	now := time.Now().UTC()
	if policy.Now != nil {
		now = policy.Now().UTC()
	}
	cutoff := now.Add(-policy.Window)
	out := make([]GateRun, 0, len(runs))
	for _, r := range runs {
		if r.At.IsZero() || !r.At.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

func flakinessCounts(runs []GateRun) (failures int, transitions int) {
	if len(runs) == 0 {
		return 0, 0
	}
	prev := runs[0].Passed
	if !prev {
		failures++
	}
	for _, r := range runs[1:] {
		if !r.Passed {
			failures++
		}
		if r.Passed != prev {
			transitions++
			prev = r.Passed
		}
	}
	return failures, transitions
}
