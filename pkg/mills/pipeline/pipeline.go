package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/mills/gates"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

// KilltestScenario is a deterministic, evidence-only Mills workflow check.
type KilltestScenario string

const (
	KilltestQueuedProof KilltestScenario = "queued-proof"
	KilltestMRAwareness KilltestScenario = "mr-awareness"
)

// KilltestEvidence is the strict interchange document consumed by the
// mills-workflow-killtest scenario runner. Deadline bounds the producer's
// observation window; CapturedAt is independently checked for staleness.
type KilltestEvidence struct {
	Scenario    KilltestScenario     `json:"scenario"`
	CapturedAt  time.Time            `json:"captured_at"`
	Deadline    time.Time            `json:"deadline"`
	QueuedProof *QueuedProofEvidence `json:"queued_proof,omitempty"`
	MRAwareness *MRAwarenessEvidence `json:"mr_awareness,omitempty"`
}

type QueuedProofEvidence struct {
	RunID       string               `json:"run_id"`
	BacklogID   string               `json:"backlog_id"`
	Transitions []KilltestTransition `json:"transitions"`
}

type KilltestTransition struct {
	RunID      string    `json:"run_id"`
	BacklogID  string    `json:"backlog_id"`
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}

type MRAwarenessEvidence struct {
	Repo         string          `json:"repo"`
	IID          int64           `json:"iid"`
	SourceBranch string          `json:"source_branch"`
	Recognitions []MRRecognition `json:"recognitions"`
}

type MRRecognition struct {
	Repo         string    `json:"repo"`
	IID          int64     `json:"iid"`
	SourceBranch string    `json:"source_branch"`
	State        string    `json:"state"`
	Recognized   bool      `json:"recognized"`
	ObservedAt   time.Time `json:"observed_at"`
}

type KilltestReport struct {
	Scenario   KilltestScenario `json:"scenario"`
	Verdict    string           `json:"verdict"`
	Passed     bool             `json:"passed"`
	ReasonCode string           `json:"reason_code,omitempty"`
	Detail     string           `json:"detail,omitempty"`
	Evidence   []string         `json:"evidence,omitempty"`
}

type killtestValidationError struct {
	code string
	err  error
}

func (e *killtestValidationError) Error() string { return e.err.Error() }
func (e *killtestValidationError) Unwrap() error { return e.err }

func killtestError(code, format string, args ...any) error {
	return &killtestValidationError{code: code, err: fmt.Errorf(format, args...)}
}

// KilltestFailureCode returns the stable reason code attached to a semantic
// validation error. Errors outside the evidence validator fail closed as
// invalid evidence.
func KilltestFailureCode(err error) string {
	var validationErr *killtestValidationError
	if errors.As(err, &validationErr) {
		return validationErr.code
	}
	return "invalid_evidence"
}

// FailKilltestReport creates the stable, machine-readable failure envelope
// used by the command for both decoding and semantic validation failures.
func FailKilltestReport(scenario KilltestScenario, code string, err error) KilltestReport {
	report := KilltestReport{Scenario: scenario, Verdict: "FAIL", ReasonCode: code}
	if err != nil {
		report.Detail = err.Error()
	}
	return report
}

// AssertKilltestScenario evaluates already-collected evidence without I/O or
// sleeps. It deliberately rejects surplus and contradictory observations.
func AssertKilltestScenario(scenario KilltestScenario, evidence KilltestEvidence, now time.Time, maxAge time.Duration) (KilltestReport, error) {
	report := KilltestReport{Scenario: scenario, Verdict: "FAIL"}
	if scenario != KilltestQueuedProof && scenario != KilltestMRAwareness {
		return report, killtestError("unknown_scenario", "unknown scenario %q", scenario)
	}
	if evidence.Scenario != scenario {
		return report, killtestError("contradictory_identity", "evidence scenario %q does not match requested %q", evidence.Scenario, scenario)
	}
	if now.IsZero() || evidence.CapturedAt.IsZero() || evidence.Deadline.IsZero() {
		return report, killtestError("missing_identity", "scenario timing evidence is incomplete")
	}
	if maxAge <= 0 || evidence.CapturedAt.After(now) || now.Sub(evidence.CapturedAt) > maxAge {
		return report, killtestError("stale_evidence", "scenario evidence is stale or future-dated: captured_at=%s now=%s max_age=%s", evidence.CapturedAt, now, maxAge)
	}
	if now.After(evidence.Deadline) || evidence.Deadline.Before(evidence.CapturedAt) {
		return report, killtestError("expired_deadline", "scenario observation timed out: captured_at=%s deadline=%s now=%s", evidence.CapturedAt, evidence.Deadline, now)
	}

	var err error
	switch scenario {
	case KilltestQueuedProof:
		if evidence.MRAwareness != nil {
			return report, killtestError("contradictory_identity", "queued-proof evidence contains contradictory mr-awareness evidence")
		}
		report.Evidence, err = assertQueuedProof(evidence.QueuedProof, evidence.CapturedAt)
	case KilltestMRAwareness:
		if evidence.QueuedProof != nil {
			return report, killtestError("contradictory_identity", "mr-awareness evidence contains contradictory queued-proof evidence")
		}
		report.Evidence, err = assertMRAwareness(evidence.MRAwareness, evidence.CapturedAt)
	}
	if err != nil {
		return report, err
	}
	report.Passed = true
	report.Verdict = "PASS"
	return report, nil
}

func assertQueuedProof(e *QueuedProofEvidence, capturedAt time.Time) ([]string, error) {
	if e == nil || strings.TrimSpace(e.RunID) == "" || strings.TrimSpace(e.BacklogID) == "" {
		return nil, killtestError("missing_identity", "queued-proof identity evidence is missing")
	}
	want := []string{"queued", "picked_up", "paused"}
	if len(e.Transitions) != len(want) {
		return nil, killtestError("duplicate_or_ambiguous_evidence", "queued-proof requires exactly queued, picked_up, paused transitions; got %d", len(e.Transitions))
	}
	proof := make([]string, 0, len(want))
	var previous time.Time
	for i, transition := range e.Transitions {
		if transition.RunID != e.RunID || transition.BacklogID != e.BacklogID || transition.State != want[i] {
			return nil, killtestError("contradictory_identity", "queued-proof transition %d is contradictory: got run=%q backlog=%q state=%q", i, transition.RunID, transition.BacklogID, transition.State)
		}
		if transition.ObservedAt.IsZero() || transition.ObservedAt.After(capturedAt) || (!previous.IsZero() && !transition.ObservedAt.After(previous)) {
			return nil, killtestError("duplicate_or_ambiguous_evidence", "queued-proof transition %q has missing or unordered timestamp", transition.State)
		}
		previous = transition.ObservedAt
		proof = append(proof, fmt.Sprintf("run %s backlog %s observed %s at %s", e.RunID, e.BacklogID, transition.State, transition.ObservedAt.UTC().Format(time.RFC3339Nano)))
	}
	return proof, nil
}

func assertMRAwareness(e *MRAwarenessEvidence, capturedAt time.Time) ([]string, error) {
	if e == nil || strings.TrimSpace(e.Repo) == "" || e.IID <= 0 || strings.TrimSpace(e.SourceBranch) == "" {
		return nil, killtestError("missing_identity", "mr-awareness identity evidence is missing")
	}
	if len(e.Recognitions) != 1 {
		return nil, killtestError("duplicate_or_ambiguous_evidence", "mr-awareness requires exactly one unambiguous recognition; got %d", len(e.Recognitions))
	}
	r := e.Recognitions[0]
	if !r.Recognized || r.Repo != e.Repo || r.IID != e.IID || r.SourceBranch != e.SourceBranch || strings.TrimSpace(r.State) == "" {
		return nil, killtestError("contradictory_identity", "mr-awareness recognition is missing or contradictory")
	}
	if r.ObservedAt.IsZero() || r.ObservedAt.After(capturedAt) {
		return nil, killtestError("missing_identity", "mr-awareness recognition has missing or future timestamp")
	}
	return []string{fmt.Sprintf("MR %s!%d branch %s recognized as %s at %s", e.Repo, e.IID, e.SourceBranch, r.State, r.ObservedAt.UTC().Format(time.RFC3339Nano))}, nil
}

// concurrencyLimiter is the supervision boundary used by MCP servers and
// test doubles. Keeping it narrow avoids coupling pipeline policy to a server
// implementation.
type concurrencyLimiter interface {
	SetConcurrencyLimit(int)
}

// ConfigureConcurrency validates and applies max_concurrent_pipelines to the
// scheduler limiter, leaving the limiter unchanged when validation fails.
func ConfigureConcurrency(limiter concurrencyLimiter, policy sharedpolicy.PipelineConcurrencyPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	loomconcurrency.ApplyValue(limiter, policy.EffectiveLimit())
	return nil
}

// FailClosedPreflight wires the storage-health and local-config admission
// gates into Runner.HealthGates. It satisfies HealthGatePreflight, so the
// existing Runner preflight escalates before dispatching any autonomous stage.
type FailClosedPreflight struct {
	Gates gates.GateRunner
}

// DecideHealthGates evaluates the complete fail-closed admission contract.
// Safety failures are represented as a decision rather than an error so the
// runner can persist the classified escalation instead of treating a known
// blocked condition as an operator failure.
func (p FailClosedPreflight) DecideHealthGates(ctx context.Context) (gates.HealthDecision, error) {
	return p.Gates.Run(ctx).HealthDecision(), nil
}

// NewFailClosedPreflight constructs the preflight used by a Mills workflow.
// Both dependencies are required at evaluation time; omitted dependencies
// become classified blocks, never a permissive no-op.
func NewFailClosedPreflight(storage gates.StorageHealthEvaluator, config gates.LocalConfigPreflight) HealthGatePreflight {
	return FailClosedPreflight{Gates: gates.GateRunner{
		StorageHealth: storage,
		LocalConfig:   config,
	}}
}

// ConfigureFailClosedPreflight installs the admission gates on a Runner.
// A nil runner is ignored so startup composition can remain optional while
// every non-nil configured runner is fail-closed.
func ConfigureFailClosedPreflight(r *Runner, storage gates.StorageHealthEvaluator, config gates.LocalConfigPreflight) {
	if r == nil {
		return
	}
	r.HealthGates = NewFailClosedPreflight(storage, config)
}
