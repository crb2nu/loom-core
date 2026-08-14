package mrwatch

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/audit"
)

// DefaultStaleAfter is how long an otherwise-healthy MR's source branch can go
// without an update before it is flagged stale_branch (and Stale=true).
const DefaultStaleAfter = 7 * 24 * time.Hour

// runningPipelineStatuses are GitLab pipeline states that mean "not terminal
// yet" — the MR is waiting on CI, so it classifies as ci_running.
var runningPipelineStatuses = map[string]struct{}{
	"created":              {},
	"waiting_for_resource": {},
	"preparing":            {},
	"pending":              {},
	"running":              {},
	"scheduled":            {},
}

// greenPipelineStatuses are terminal-success (or manual-hold, which is not a
// failure) states — CI is not blocking a merge.
var greenPipelineStatuses = map[string]struct{}{
	"success": {},
	"manual":  {},
}

// Classify maps one open merge request onto the stall taxonomy and returns the
// state plus a short machine reason. now/staleAfter are injected so the result
// is deterministic in tests.
//
// Precedence (first match wins):
//  1. conflict          — merge conflicts block everything else.
//  2. draft_idle        — a draft MR is not trying to merge.
//  3. awaiting_pipeline — no head pipeline yet.
//  4. pipeline_skipped  — head pipeline was skipped (never goes green alone).
//  5. ci_running        — head pipeline is pending/running.
//  6. ci_failed_*       — head pipeline failed (flaky vs deterministic).
//  7. green CI:
//     - automerge_unarmed — MWPS false.
//     - stale_branch      — armed + green but branch untouched > staleAfter.
//     - ok                — nothing to do.
//
// Terminal lifecycles classify onto their own states so a merge is an
// affirmative signal rather than an absence: merged → StateMerged (retained by
// the poller for a bounded window), closed → StateClosed (dropped). The two are
// never conflated. Any other lifecycle (locked, or an unrecognised value)
// returns ("", "") and is dropped — the registry only reports what it can name.
func Classify(mr MRInfo, now time.Time, staleAfter time.Duration) (State, string) {
	switch strings.ToLower(strings.TrimSpace(mr.State)) {
	case "opened":
	case "merged":
		return StateMerged, "merged"
	case "closed":
		return StateClosed, "closed"
	default:
		return "", ""
	}
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}

	if hasConflicts(mr) {
		return StateConflict, "merge_conflict"
	}
	if mr.Draft {
		return StateDraftIdle, "draft"
	}

	stale := !mr.UpdatedAt.IsZero() && now.Sub(mr.UpdatedAt) > staleAfter

	if mr.Pipeline == nil {
		return StateAwaitingPipeline, "no_head_pipeline"
	}

	status := strings.ToLower(strings.TrimSpace(mr.Pipeline.Status))
	switch {
	case status == "skipped":
		return StatePipelineSkipped, "pipeline_skipped"
	case isRunning(status):
		// Running CI on an unarmed MR is still worth flagging so it can be
		// armed before it goes green and races the session end.
		if !mr.MergeWhenPipelineSucceeds {
			return StateAutomergeUnarmed, "mwps_unarmed_ci_running"
		}
		return StateCIRunning, "ci_running"
	case status == "failed":
		return classifyFailure(mr)
	case status == "canceled" || status == "cancelled":
		return StateCIFailedDeterministic, "pipeline_canceled"
	case isGreen(status):
		if !mr.MergeWhenPipelineSucceeds {
			return StateAutomergeUnarmed, "mwps_unarmed"
		}
		if stale {
			return StateStaleBranch, "branch_stale"
		}
		return StateOK, ""
	default:
		// Unknown/other status: treat as awaiting so we neither cry failure
		// nor claim green on a state we don't understand.
		return StateAwaitingPipeline, "pipeline_status_" + status
	}
}

// classifyFailure decides flaky vs deterministic for a failed head pipeline.
// It reuses pkg/mills/audit.ClassifyCIFailureMessage on any failure signature
// available. In M1 no job logs are fetched, so FailureReason is typically empty
// and this returns ci_failed_deterministic reason "unclassified" — the spec's
// explicit v1 behavior when only pipeline status is known.
func classifyFailure(mr MRInfo) (State, string) {
	msg := ""
	if mr.Pipeline != nil {
		msg = strings.TrimSpace(mr.Pipeline.FailureReason)
	}
	if msg == "" {
		return StateCIFailedDeterministic, "unclassified"
	}
	c := audit.ClassifyCIFailureMessage(msg)
	if c.Matched && c.Retryable {
		return StateCIFailedFlaky, nonEmpty(c.Reason, "flaky")
	}
	return StateCIFailedDeterministic, nonEmpty(c.Reason, "unclassified")
}

// hasConflicts detects a conflicted MR from either the boolean has_conflicts
// flag or GitLab's detailed_merge_status enum (conflict / broken_status).
func hasConflicts(mr MRInfo) bool {
	if mr.HasConflicts {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus)) {
	case "conflict", "broken_status":
		return true
	}
	return false
}

func isRunning(status string) bool {
	_, ok := runningPipelineStatuses[status]
	return ok
}

func isGreen(status string) bool {
	_, ok := greenPipelineStatuses[status]
	return ok
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
