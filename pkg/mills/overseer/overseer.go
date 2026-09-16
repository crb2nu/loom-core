package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	telemetrypkg "github.com/crb2nu/loom/pkg/telemetry"
)

// PromotionMode is the effective overseer execution mode selected from the
// persisted soak-complete artifact.
type PromotionMode string

const (
	PromotionModeDryRun PromotionMode = "dry-run"
	PromotionModeActive PromotionMode = "active"

	// Stable operator-facing reasons. A rejection names the first condition
	// the artifact fails, in the order SoakProgress.CompleteAt checks them.
	PromotionReasonArtifactMissing      = "soak-complete artifact is missing"
	PromotionReasonArtifactUnreadable   = "soak-complete artifact is unreadable"
	PromotionReasonArtifactMalformed    = "soak-complete artifact is malformed"
	PromotionReasonSoakStartFutureDated = "soak-complete artifact soak start is after its generated_at"
	PromotionReasonSoakTooShort         = "soak-complete artifact covers less than 168 hours"
	PromotionReasonSoakDiverged         = "soak-complete artifact records policy divergences"
	PromotionReasonSoakIncomplete       = "soak-complete artifact does not satisfy the S2 soak contract"
	PromotionReasonSoakComplete         = "soak-complete artifact satisfies the S2 soak contract"
)

// PromotionDecision records both the effective mode and its operator-facing
// reason. Callers must use Mode, rather than interpreting an error as approval.
type PromotionDecision struct {
	Mode   PromotionMode `json:"mode"`
	Reason string        `json:"reason"`
}

// SoakCompleteArtifact is the on-disk promotion evidence: the soak projection
// the shift report already emits (generated_at plus soak_progress), deposited
// by an operator once the S2 checklist has passed. It carries no verdict of
// its own. Completion is decided by SoakProgress.CompleteAt at the artifact's
// own generated_at, exactly as shiftreport.Compose derives soak_complete, so
// the gate can never disagree with the report the evidence came from. Any
// other field in the file, including a projected soak_complete, is ignored
// rather than trusted.
type SoakCompleteArtifact struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	SoakProgress *SoakProgress `json:"soak_progress"`
}

// PromotionGate reads the artifact on every decision, allowing a newly
// deposited (or removed) artifact to take effect without a process restart.
// ReadFile is an optional test seam; production callers should leave it nil.
type PromotionGate struct {
	ArtifactPath string
	Logger       *slog.Logger
	ReadFile     func(string) ([]byte, error)
}

// Decide returns active mode only for a readable, well-formed artifact whose
// soak progress SoakProgress.CompleteAt accepts: a persisted start no later
// than generated_at, at least S2SoakMinimumDuration elapsed, and zero
// divergences. Every uncertainty fails closed to dry-run and is logged.
func (g PromotionGate) Decide() PromotionDecision {
	readFile := g.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	b, err := readFile(g.ArtifactPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return g.reject(PromotionReasonArtifactMissing)
		}
		return g.reject(PromotionReasonArtifactUnreadable)
	}

	var artifact SoakCompleteArtifact
	if err := json.Unmarshal(b, &artifact); err != nil {
		return g.reject(PromotionReasonArtifactMalformed)
	}
	if !artifact.SoakProgress.CompleteAt(artifact.GeneratedAt) {
		return g.reject(explainIncompleteSoak(artifact.SoakProgress, artifact.GeneratedAt))
	}

	decision := PromotionDecision{Mode: PromotionModeActive, Reason: PromotionReasonSoakComplete}
	g.logger().Info("overseer promotion gate evaluated", "mode", decision.Mode, "reason", decision.Reason, "artifact", g.ArtifactPath)
	return decision
}

// explainIncompleteSoak names the first S2 condition a soak projection fails.
// It is consulted only after SoakProgress.CompleteAt has rejected the
// evidence, so it chooses the reason but never the verdict; a condition it
// does not recognise still reads as incomplete.
func explainIncompleteSoak(p *SoakProgress, observedAt time.Time) string {
	switch {
	case p == nil || observedAt.IsZero() || p.StartedAt.IsZero() || p.ElapsedSeconds < 0 || p.Divergences < 0:
		return PromotionReasonArtifactMalformed
	case p.StartedAt.After(observedAt):
		return PromotionReasonSoakStartFutureDated
	case p.ElapsedSeconds < S2SoakMinimumSeconds:
		return PromotionReasonSoakTooShort
	case p.Divergences > 0:
		return PromotionReasonSoakDiverged
	}
	return PromotionReasonSoakIncomplete
}

func (g PromotionGate) reject(reason string) PromotionDecision {
	decision := PromotionDecision{Mode: PromotionModeDryRun, Reason: reason}
	g.logger().Warn("overseer promotion gate rejected artifact", "mode", decision.Mode, "reason", reason, "artifact", g.ArtifactPath)
	return decision
}

func (g PromotionGate) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

const (
	// S2SoakMinimumDuration is the closed evidence window required before an
	// overseer action class can leave dry-run.
	S2SoakMinimumDuration = 7 * 24 * time.Hour
	// S2SoakMinimumDryRunDecisions prevents an empty soak from passing.
	S2SoakMinimumDryRunDecisions = 1
	// S2SoakMinimumWouldHaveActed proves the action path was exercised.
	S2SoakMinimumWouldHaveActed = 1
	// S2SoakMaximumDivergences requires exact agreement with reviewed policy.
	S2SoakMaximumDivergences = 0
	// S2SoakMinimumSeconds is the wire-level form of the seven-day gate.
	S2SoakMinimumSeconds int64 = 604800
)

// SoakProgress is the stable telemetry contract shared with shift reports.
// StartedAt is persisted once by SoakProgressStore; elapsed time is projected
// from an injected observation time and is therefore deterministic.
type SoakProgress struct {
	StartedAt      time.Time `json:"soak_started_at"`
	ElapsedSeconds int64     `json:"soak_elapsed_seconds"`
	Divergences    int       `json:"soak_divergences"`
}

// Complete fails closed for structurally malformed telemetry. Call CompleteAt
// when an observation time is available and future-dated starts must also be
// rejected. The duration threshold is inclusive and divergences must be zero.
func (p *SoakProgress) Complete() bool {
	return p != nil && !p.StartedAt.IsZero() && p.ElapsedSeconds >= S2SoakMinimumSeconds && p.Divergences == 0
}

// CompleteAt evaluates completion relative to the timestamp of the containing
// observation, preventing future-dated telemetry from opening the gate.
func (p *SoakProgress) CompleteAt(observedAt time.Time) bool {
	return !observedAt.IsZero() && p.Complete() && !p.StartedAt.After(observedAt)
}

// SoakProgressStore atomically initializes and returns the durable S2 soak
// start. Implementations must return the existing value after the first call.
type SoakProgressStore interface {
	EnsureOverseerSoakStart(context.Context, time.Time) (time.Time, error)
}

// ObserveSoakProgress persists the first observation time and projects the
// current machine-readable progress. Callers supply now so tests and replays do
// not depend on the wall clock. Storage failures and invalid values fail closed.
func ObserveSoakProgress(ctx context.Context, persistence SoakProgressStore, now time.Time, divergences int) (*SoakProgress, error) {
	if persistence == nil {
		return nil, errors.New("overseer soak start persistence is not configured")
	}
	if now.IsZero() {
		return nil, errors.New("overseer soak observation time is required")
	}
	startedAt, err := persistence.EnsureOverseerSoakStart(ctx, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("persist overseer soak start: %w", err)
	}
	startedAt = startedAt.UTC()
	if startedAt.IsZero() || startedAt.After(now.UTC()) || divergences < 0 {
		return nil, errors.New("overseer soak telemetry is malformed")
	}
	return &SoakProgress{
		StartedAt:      startedAt,
		ElapsedSeconds: int64(now.UTC().Sub(startedAt) / time.Second),
		Divergences:    divergences,
	}, nil
}

const (
	// SoakGateMinimumDuration is the inclusive minimum observation window.
	SoakGateMinimumDuration = S2SoakMinimumDuration
	// SoakGatePassMetric is a stable numeric projection of the gate verdict.
	SoakGatePassMetric = "mills_overseer_s2_soak_gate_pass"
)

// SoakGateTelemetry is one complete snapshot of reviewed dry-run evidence.
// Window is supplied explicitly so evaluation does not depend on wall time.
type SoakGateTelemetry struct {
	Window            time.Duration `json:"window"`
	Regressions       int           `json:"regressions"`
	ReviewedDecisions int           `json:"reviewed_decisions"`
	Disagreements     int           `json:"disagreements"`
}

// SoakGateVerdict is the stable, machine-readable result of one atomic
// evaluation. MetricPass is 1 on pass and 0 on every fail-closed result.
type SoakGateVerdict struct {
	Pass                     bool     `json:"pass"`
	DecisionDisagreementRate float64  `json:"decision_disagreement_rate"`
	FailureReasons           []string `json:"failure_reasons"`
	MetricPass               int      `json:"mills_overseer_s2_soak_gate_pass"`
}

// SoakGate evaluates telemetry only. It has no promotion dependency or side
// effects, so a verdict cannot itself change rollout state.
type SoakGate struct{}

// NewSoakGate constructs the stateless S2 soak evaluator.
func NewSoakGate() SoakGate { return SoakGate{} }

// Stable metric names consumed by status and promotion tooling.
const (
	S2SoakElapsedDaysMetric     = "mills_overseer_soak_elapsed_days"
	S2SoakDryRunDecisionsMetric = "mills_overseer_soak_dry_run_decisions"
	S2SoakWouldHaveActedMetric  = "mills_overseer_soak_would_have_acted"
	S2SoakDivergencesMetric     = "mills_overseer_soak_divergences"
)

// SoakMetrics is the machine-readable S2 promotion verdict. It is a pure
// projection of the persisted promotion report plus reviewed divergences; it
// never changes an allow flag or performs an overseer action.
type SoakMetrics struct {
	StartedAt       time.Time `json:"started_at"`
	ElapsedDays     int       `json:"mills_overseer_soak_elapsed_days"`
	DryRunDecisions int       `json:"mills_overseer_soak_dry_run_decisions"`
	WouldHaveActed  int       `json:"mills_overseer_soak_would_have_acted"`
	Divergences     int       `json:"mills_overseer_soak_divergences"`
	Promotable      bool      `json:"promotable"`
	FailClosed      bool      `json:"fail_closed"`
	FailureReasons  []string  `json:"failure_reasons,omitempty"`
}

// DecisionCount returns the number of reviewed dry-run decisions in the soak.
func (m SoakMetrics) DecisionCount() int { return m.DryRunDecisions }

// AgreementCount returns reviewed decisions that agreed with approved policy.
// Invalid telemetry is clamped to zero so report consumers never display a
// negative agreement count.
func (m SoakMetrics) AgreementCount() int {
	agreements := m.DryRunDecisions - m.Divergences
	if agreements < 0 {
		return 0
	}
	return agreements
}

// AgreementRate returns the fraction of reviewed decisions that agreed with
// approved policy. The boolean is false when no decisions have been reviewed.
func (m SoakMetrics) AgreementRate() (float64, bool) {
	if m.DryRunDecisions <= 0 {
		return 0, false
	}
	return float64(m.AgreementCount()) / float64(m.DryRunDecisions), true
}

// SoakStart returns the start of the evidence window in UTC.
func (m SoakMetrics) SoakStart() time.Time { return m.StartedAt.UTC() }

// SoakTelemetryStore is the persistence contract used by overseer dry-run
// decisions and status evaluation. *store.Store satisfies this interface.
type SoakTelemetryStore interface {
	RecordOverseerSoakDecision(context.Context, time.Time, bool, bool) error
	OverseerSoakTelemetry(context.Context, time.Time) ([]store.OverseerSoakDailyCounters, error)
}

// RecordDryRunDecision records one dry-run policy decision. Callers pass the
// decision timestamp explicitly so tests, replays, and UTC bucketing are
// deterministic.
func RecordDryRunDecision(ctx context.Context, telemetry SoakTelemetryStore, at time.Time, wouldHaveActed, policyDisagreement bool) error {
	if telemetry == nil {
		return errors.New("overseer soak telemetry is not configured")
	}
	if err := telemetry.RecordOverseerSoakDecision(ctx, at, wouldHaveActed, policyDisagreement); err != nil {
		return err
	}
	// Emit only after persistence succeeds so the Prometheus population matches
	// the evidence that EvaluatePersistedS2Soak evaluates fail closed.
	telemetrypkg.RecordOverseerDryRunDecision(ctx, wouldHaveActed, policyDisagreement)
	return nil
}

// EvaluatePersistedS2Soak aggregates the seven complete UTC days immediately
// before now and deterministically evaluates the S2 bar. Store errors, missing
// day buckets, and inconsistent counters all fail closed.
func EvaluatePersistedS2Soak(ctx context.Context, telemetry SoakTelemetryStore, now time.Time) SoakMetrics {
	result := SoakMetrics{}
	if telemetry == nil || now.IsZero() {
		return failSoak(result, "soak telemetry is missing or unreadable")
	}
	days, err := telemetry.OverseerSoakTelemetry(ctx, now)
	if err != nil {
		return failSoak(result, "soak telemetry is missing or unreadable: "+err.Error())
	}
	end := now.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -7)
	result.StartedAt = start
	if len(days) != 7 {
		return failSoak(result, "soak telemetry does not contain seven complete UTC days")
	}
	for i, day := range days {
		wantDay := start.AddDate(0, 0, i)
		if !day.Day.Equal(wantDay) || day.Decisions <= 0 || day.WouldHaveActed < 0 || day.WouldHaveActed > day.Decisions || day.Disagreements < 0 || day.Disagreements > day.Decisions {
			return failSoak(result, "soak telemetry is incomplete or inconsistent")
		}
		result.DryRunDecisions += day.Decisions
		result.WouldHaveActed += day.WouldHaveActed
		result.Divergences += day.Disagreements
	}
	result.ElapsedDays = 7
	if result.DryRunDecisions < S2SoakMinimumDryRunDecisions {
		result.FailureReasons = append(result.FailureReasons, "dry-run decision threshold not met")
	}
	if result.WouldHaveActed < S2SoakMinimumWouldHaveActed {
		result.FailureReasons = append(result.FailureReasons, "would-have-acted threshold not met")
	}
	if result.Divergences > S2SoakMaximumDivergences {
		result.FailureReasons = append(result.FailureReasons, "divergence threshold exceeded")
	}
	result.Promotable = len(result.FailureReasons) == 0
	result.FailClosed = !result.Promotable
	return result
}

// EvaluateS2Soak evaluates one closed overseer promotion report. A nil or
// malformed report is unreadable evidence and therefore fails closed.
// divergences counts reviewed disagreements between a recorded dry-run
// decision and the action expected from the approved policy for the same
// subject and observation.
func EvaluateS2Soak(report *guard.PromotionReport, divergences int) SoakMetrics {
	result := SoakMetrics{Divergences: divergences}
	if report == nil {
		return failSoak(result, "promotion evidence is missing or unreadable")
	}
	if report.WindowStart.IsZero() || report.WindowEnd.IsZero() {
		return failSoak(result, "promotion evidence window is missing or unreadable")
	}
	result.StartedAt = report.WindowStart.UTC()

	window := report.WindowEnd.Sub(report.WindowStart)
	if window > 0 {
		result.ElapsedDays = int(window / (24 * time.Hour))
	}
	result.DryRunDecisions = report.TotalDryRun
	result.WouldHaveActed = report.TotalDryRun

	if report.ActorPrefix != "overseer." {
		result.FailureReasons = append(result.FailureReasons, "actor_prefix must equal overseer.")
	}
	if window < S2SoakMinimumDuration {
		result.FailureReasons = append(result.FailureReasons,
			fmt.Sprintf("closed soak window must be at least %s", S2SoakMinimumDuration))
	}
	if report.ZeroEvidence || report.TotalActions == 0 {
		result.FailureReasons = append(result.FailureReasons, "promotion evidence is empty")
	}
	if result.DryRunDecisions < S2SoakMinimumDryRunDecisions {
		result.FailureReasons = append(result.FailureReasons, "dry-run decision threshold not met")
	}
	if result.WouldHaveActed < S2SoakMinimumWouldHaveActed {
		result.FailureReasons = append(result.FailureReasons, "would-have-acted threshold not met")
	}
	// An executed action in an expressly dry-run soak is itself divergent.
	if report.TotalExecuted > 0 {
		result.Divergences += report.TotalExecuted
	}
	if result.Divergences > S2SoakMaximumDivergences {
		result.FailureReasons = append(result.FailureReasons, "divergence threshold exceeded")
	}
	if divergences < 0 || report.TotalDryRun < 0 || report.TotalExecuted < 0 || report.TotalActions != report.TotalDryRun+report.TotalExecuted {
		result.FailureReasons = append(result.FailureReasons, "promotion evidence is inconsistent")
	}

	result.Promotable = len(result.FailureReasons) == 0
	result.FailClosed = !result.Promotable
	return result
}

func failSoak(result SoakMetrics, reason string) SoakMetrics {
	result.FailClosed = true
	result.FailureReasons = []string{reason}
	return result
}
