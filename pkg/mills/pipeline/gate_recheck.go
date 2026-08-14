package pipeline

import (
	"sync"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// GateRecheckObservation describes one deterministic gate evaluation.
type GateRecheckObservation struct {
	GateID    string
	RunID     string
	InputHash string
	Verdict   telemetry.GateVerdict
}

// GateRecheckDecision tells the runner whether this evaluation qualifies for
// a free gate-only re-evaluation. A false decision follows the normal retry or
// escalation policy.
type GateRecheckDecision struct {
	Recheck        bool
	GateOnly       bool
	ConsumesRetry  bool
	RecheckAttempt uint64
}

type gateRecheckKey struct {
	runID     string
	gateID    string
	inputHash string
}

type gateRecheckState struct {
	verdict        telemetry.GateVerdict
	recheckGranted bool
}

// GateRecheckPolicy detects conflicting verdicts for identical inputs and
// grants one free re-check for the deterministic docs_guardrail and scope
// gates. It is safe for concurrent use.
type GateRecheckPolicy struct {
	mu     sync.Mutex
	states map[gateRecheckKey]gateRecheckState
	sink   gates.GateFlakeDetectedEventSink
}

// NewGateRecheckPolicy constructs a bounded gate-flake policy.
func NewGateRecheckPolicy(sink gates.GateFlakeDetectedEventSink) *GateRecheckPolicy {
	return &GateRecheckPolicy{
		states: make(map[gateRecheckKey]gateRecheckState),
		sink:   sink,
	}
}

// Observe records a verdict and returns whether it earns the one free
// gate-only re-check. Telemetry is emitted after committing the decision and
// is panic-safe, so it cannot alter the returned policy result.
func (p *GateRecheckPolicy) Observe(observation GateRecheckObservation) GateRecheckDecision {
	if p == nil || !supportsGateFlakeRecheck(observation.GateID) ||
		observation.RunID == "" || observation.InputHash == "" {
		return GateRecheckDecision{}
	}

	key := gateRecheckKey{
		runID: observation.RunID, gateID: observation.GateID,
		inputHash: observation.InputHash,
	}

	p.mu.Lock()
	state, exists := p.states[key]
	if !exists {
		p.states[key] = gateRecheckState{verdict: observation.Verdict}
		p.mu.Unlock()
		return GateRecheckDecision{}
	}
	if state.verdict == observation.Verdict || state.recheckGranted {
		p.mu.Unlock()
		return GateRecheckDecision{}
	}

	previous := state.verdict
	state.verdict = observation.Verdict
	state.recheckGranted = true
	p.states[key] = state
	sink := p.sink
	p.mu.Unlock()

	const recheckAttempt = uint64(1)
	decision := GateRecheckDecision{
		Recheck: true, GateOnly: true, ConsumesRetry: false,
		RecheckAttempt: recheckAttempt,
	}
	gates.EmitGateFlakeDetected(sink, gates.GateFlakeDetectedEvent{
		GateID: observation.GateID, RunID: observation.RunID,
		InputHash: observation.InputHash, PreviousVerdict: previous,
		ConflictingVerdict: observation.Verdict, RecheckAttempt: recheckAttempt,
	})
	return decision
}

func supportsGateFlakeRecheck(gateID string) bool {
	return gateID == "docs_guardrail" || gateID == "scope"
}
