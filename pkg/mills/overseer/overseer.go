package overseer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	telemetrypkg "github.com/crb2nu/loom/pkg/telemetry"
)

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
)

const (
	// SoakGateMinimumDuration is the inclusive minimum observation window.
	SoakGateMinimumDuration = 168 * time.Hour
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
	ElapsedDays     int      `json:"mills_overseer_soak_elapsed_days"`
	DryRunDecisions int      `json:"mills_overseer_soak_dry_run_decisions"`
	WouldHaveActed  int      `json:"mills_overseer_soak_would_have_acted"`
	Divergences     int      `json:"mills_overseer_soak_divergences"`
	Promotable      bool     `json:"promotable"`
	FailClosed      bool     `json:"fail_closed"`
	FailureReasons  []string `json:"failure_reasons,omitempty"`
}

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
