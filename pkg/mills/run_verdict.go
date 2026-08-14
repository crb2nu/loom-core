package mills

import (
	"context"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Run verdicts (Trustworthy Verdicts S1, .loom/135). A run's terminal row is
// immutable history: State/EscalationClass record what the pipeline concluded
// at terminal time and are never rewritten. The VERDICT is what we currently
// believe, resolved as the newest superseding event on the run's own event
// subject — or derived from the terminal row when nothing superseded it.
// Consumers (reports, retry policy, storm detection, the HUD drawer) read the
// verdict, never the frozen class, so a rescued false escalation stops
// counting as an escalation everywhere at once.

// RunVerdictEventKindPrefix namespaces explicit verdict-superseding events.
// The suffix names the source ("run.verdict.ghost_spark_merged"); later
// slices add sources (regression attribution, operator override) as new
// suffixes without touching the resolver contract.
const RunVerdictEventKindPrefix = "run.verdict."

// RunVerdictKindGhostSparkMerged supersedes an escalation whose work landed:
// the ghost-spark sweep closed the item because its MR merged (any pass —
// recorded MR state, merged branch archaeology, or green-MR adoption; the
// payload's outcome preserves which).
const RunVerdictKindGhostSparkMerged = RunVerdictEventKindPrefix + "ghost_spark_merged"

// RunVerdictKindOperatorOverride records an administrator-confirmed correction
// for a run whose work was rescued and merged after the run escalated.
const RunVerdictKindOperatorOverride = RunVerdictEventKindPrefix + "operator_override"

// GhostSparkClosedEventKind is the reconciler's pre-existing first-writer
// closure event, recognized as an implicit supersede source so every run the
// sweep already reconciled gets a corrected verdict retroactively — no
// backfill migration. Exported so the guard reports can include it in their
// kind-filtered window scans.
const GhostSparkClosedEventKind = "reconciler.ghost_spark_closed"

// RunVerdictClassMergedAfterEscalation is the corrected class for an
// escalated run whose MR later merged. It is deliberately NOT "merged": a
// report may fold it into merged for a win rate, but the label keeps the
// history visible so corrected runs are never silently indistinguishable
// from clean merges.
const RunVerdictClassMergedAfterEscalation = "merged_after_escalation"

// RunVerdictCorrectionKinds lists every event kind that supersedes an
// escalated run's verdict, for bulk window scans (the foreman storm rule,
// KPI writer, and guard reports read corrections in one filtered query).
func RunVerdictCorrectionKinds() []string {
	return []string{RunVerdictKindGhostSparkMerged, RunVerdictKindOperatorOverride, GhostSparkClosedEventKind}
}

// runVerdictKindLister is the narrow events read the bulk resolver needs.
// *store.EventDAO satisfies it.
type runVerdictKindLister interface {
	ListSinceByKinds(ctx context.Context, kinds []string, since time.Time, limit int) ([]*store.Event, error)
}

// supersededScanLimit bounds the correction scan. Corrections are low-volume
// (one first-writer event per rescued run); a saturated scan under-discounts,
// which only makes storm/KPI counts conservative — never hides an alert.
const supersededScanLimit = 5000

// SupersededRunIDsSince returns the run IDs whose escalation verdict was
// superseded by a correction event at or after since. Callers intersect it
// with an escalated-run population — the set alone says nothing about a
// run's terminal state (see ResolveRunVerdict for the per-run contract).
func SupersededRunIDsSince(ctx context.Context, events runVerdictKindLister, since time.Time) (map[string]struct{}, error) {
	evs, err := events.ListSinceByKinds(ctx, RunVerdictCorrectionKinds(), since, supersededScanLimit)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(evs))
	for _, e := range evs {
		if e == nil || e.SubjectID == "" {
			continue
		}
		out[e.SubjectID] = struct{}{}
	}
	return out, nil
}

// RunVerdict is the resolved HEAD of a run's verdict history.
type RunVerdict struct {
	// Class is the current belief: the terminal row's class until something
	// supersedes it, then the superseding class.
	Class string `json:"class"`
	// Superseded is true when a correction event overrode the terminal row.
	Superseded bool `json:"superseded"`
	// Source names what superseded it ("ghost_spark_merged",
	// "ghost_spark_closed" for the legacy closure event); empty when derived.
	Source string `json:"source,omitempty"`
	// PriorClass is the class the supersede replaced (the escalation's
	// class at terminal time), empty when nothing superseded.
	PriorClass string `json:"prior_class,omitempty"`
	// Outcome carries the superseding event's outcome detail
	// ("adopted_green_mr", "merged_branch", "merged").
	Outcome string `json:"outcome,omitempty"`
	// OccurredAt is when the current belief was established: the superseding
	// event's timestamp, else the run's EndedAt (zero for live runs).
	OccurredAt time.Time `json:"occurred_at"`
}

// derivedRunClass is the verdict class a terminal row yields on its own.
func derivedRunClass(run *store.PipelineRun) string {
	if run.State != store.PipelineEscalated {
		return string(run.State)
	}
	if run.EscalationClass != "" {
		return run.EscalationClass
	}
	if run.FailureClass != "" {
		return run.FailureClass
	}
	// Unclassified escalations fail closed to code, matching
	// pipeline.FailureClassFromString.
	return "code"
}

// ResolveRunVerdict resolves the verdict HEAD for a run from its own event
// subject (events newest-first, as EventDAO.ListBySubject returns them).
// Precedence: newest explicit run.verdict.* event, else the reconciler's
// ghost-spark closure event, else the terminal row. Nil run yields the zero
// verdict; nil/foreign events are skipped.
func ResolveRunVerdict(run *store.PipelineRun, events []*store.Event) RunVerdict {
	if run == nil {
		return RunVerdict{}
	}
	derived := derivedRunClass(run)
	var legacy *store.Event
	for _, e := range events {
		if e == nil {
			continue
		}
		if strings.HasPrefix(e.Kind, RunVerdictEventKindPrefix) {
			v := RunVerdict{
				Class:      RunVerdictClassMergedAfterEscalation,
				Superseded: true,
				Source:     strings.TrimPrefix(e.Kind, RunVerdictEventKindPrefix),
				PriorClass: derived,
				OccurredAt: e.OccurredAt,
			}
			if c, ok := e.Payload["class"].(string); ok && c != "" {
				v.Class = c
			}
			if p, ok := e.Payload["prior_class"].(string); ok && p != "" {
				v.PriorClass = p
			}
			if o, ok := e.Payload["outcome"].(string); ok {
				v.Outcome = o
			}
			return v
		}
		if e.Kind == GhostSparkClosedEventKind && legacy == nil {
			legacy = e
		}
	}
	if legacy != nil && run.State == store.PipelineEscalated {
		v := RunVerdict{
			Class:      RunVerdictClassMergedAfterEscalation,
			Superseded: true,
			Source:     "ghost_spark_closed",
			PriorClass: derived,
			OccurredAt: legacy.OccurredAt,
		}
		if o, ok := legacy.Payload["outcome"].(string); ok {
			v.Outcome = o
		}
		return v
	}
	v := RunVerdict{Class: derived}
	if run.EndedAt != nil {
		v.OccurredAt = *run.EndedAt
	}
	return v
}
