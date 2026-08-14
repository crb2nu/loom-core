// Package mrwatch is the HUD daemon's branch→merge-request status registry
// (slice M1 of the "no MR left behind" spec).
//
// It polls open merge requests for a configurable set of GitLab projects,
// classifies each into a bounded stall taxonomy, and exposes the result as a
// mutex-guarded in-memory snapshot for the HUD REST layer. An unreachable
// GitLab must never crash or block HUD init: the poller degrades to serving
// the last good snapshot with Stale=true (institutional rule; cf. the HUD-init
// hard-fail-on-k3s-outage incident).
//
// M1 is deliberately read-only: no MCP tool, no CLI, no proxy trailer, no
// shepherd auto-actions. Those are later slices (M2–M5) and consume this
// registry as their source of truth.
package mrwatch

import "time"

// State is the bounded stall taxonomy a merge request is classified into.
// Every value is wire-safe (lowercase snake) and matches the product spec.
type State string

const (
	// StateOK — open, green head pipeline, auto-merge armed, no conflicts,
	// not draft, not stale. Nothing to do.
	StateOK State = "ok"
	// StateAwaitingPipeline — open, no head pipeline yet (push not yet
	// picked up, or pipeline not created).
	StateAwaitingPipeline State = "awaiting_pipeline"
	// StateCIRunning — head pipeline is running/pending.
	StateCIRunning State = "ci_running"
	// StateCIFailedFlaky — head pipeline failed with a signature the audit
	// classifier flags as retryable/transient (schema fetch, runner pod, …).
	StateCIFailedFlaky State = "ci_failed_flaky"
	// StateCIFailedDeterministic — head pipeline failed with a
	// non-retryable/unknown signature; needs a real fix.
	StateCIFailedDeterministic State = "ci_failed_deterministic"
	// StateConflict — the MR has merge conflicts with its target.
	StateConflict State = "conflict"
	// StateAutomergeUnarmed — open, green or running CI, not draft, no
	// conflicts, but merge_when_pipeline_succeeds is false.
	StateAutomergeUnarmed State = "automerge_unarmed"
	// StatePipelineSkipped — the head pipeline was skipped (no jobs / rules
	// excluded it), so it will never go green on its own.
	StatePipelineSkipped State = "pipeline_skipped"
	// StateStaleBranch — otherwise-healthy MR whose source branch has not
	// been updated in a long time (likely behind target; candidate rebase).
	StateStaleBranch State = "stale_branch"
	// StateDraftIdle — the MR is a draft / work-in-progress.
	StateDraftIdle State = "draft_idle"
	// StateMerged — the MR merged. Terminal, and the only positive merge
	// marker consumers may key on. Merged MRs are retained in the snapshot for
	// a bounded window (Options.MergedRetention) so "merged" is distinguishable
	// from "never existed" and from a degraded/absent registry; without it a
	// merged MR simply vanishes and every consumer must guess. Nothing acts on
	// it: the shepherd and notifier both allow-list the states they handle.
	StateMerged State = "merged"
	// StateClosed — the MR was closed WITHOUT merging. Named explicitly so a
	// closed MR can never be conflated with a merged one; the poller does not
	// retain it (it is dropped exactly as before).
	StateClosed State = "closed"
)

// AllStates lists every taxonomy value, ordered for deterministic count maps
// and to let callers enumerate the space (e.g. seed a counts map with zeros).
func AllStates() []State {
	return []State{
		StateOK,
		StateAwaitingPipeline,
		StateCIRunning,
		StateCIFailedFlaky,
		StateCIFailedDeterministic,
		StateConflict,
		StateAutomergeUnarmed,
		StatePipelineSkipped,
		StateStaleBranch,
		StateDraftIdle,
		StateMerged,
		StateClosed,
	}
}

// Healthy reports whether a state needs no attention: StateOK (open and on
// track) and StateMerged (terminal, already landed). Every other class is an
// actionable stall (used by later slices to gate nudges/trailers — exposed here
// so the taxonomy stays in one place).
func (s State) Healthy() bool { return s == StateOK || s == StateMerged }

// PipelineInfo is a bounded view of a merge request's head pipeline. Nil on an
// MRInfo means "no head pipeline" (→ awaiting_pipeline).
type PipelineInfo struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
	WebURL string `json:"web_url,omitempty"`
	// FailureReason carries any failure message available for a failed
	// pipeline. In M1 the poller does NOT fetch job logs (API calls are
	// bounded to list-MRs + head-pipeline), so this is typically empty and a
	// failed pipeline classifies as ci_failed_deterministic reason
	// "unclassified". It exists so the flaky-vs-deterministic path can reuse
	// pkg/mills/audit.ClassifyCIFailureMessage when a signature is available.
	FailureReason string `json:"failure_reason,omitempty"`
}

// MRInfo is the raw, source-agnostic view of one open merge request that the
// classifier consumes. The GitLab adapter maps its REST response into this;
// tests construct it directly.
type MRInfo struct {
	Repo         string
	IID          int64
	Title        string
	SourceBranch string
	TargetBranch string
	WebURL       string
	State        string // GitLab lifecycle: opened/merged/closed/locked
	// SHA is the head commit of the source branch as observed by THIS poll
	// (GitLab's `sha` field). The shepherd binds its auto-merge arm to it so a
	// branch that moves between poll and action cannot be armed unreviewed.
	// Empty when the source did not report one (older snapshot, odd payload) —
	// the shepherd refuses to arm in that case.
	SHA                       string
	Draft                     bool
	HasConflicts              bool
	DetailedMergeStatus       string
	MergeWhenPipelineSucceeds bool
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	// MergedAt is GitLab's merged_at. Non-zero only for a merged MR; it anchors
	// the retention window so a merge observed late still expires on its real
	// merge time rather than on first sight.
	MergedAt time.Time
	Pipeline *PipelineInfo
}

// MergeRequest is the wire-safe, classified view of one open MR emitted by the
// registry. All fields are JSON-safe; slices/maps at the Snapshot level are
// always non-nil so an empty registry encodes arrays as [] (never null).
type MergeRequest struct {
	Repo           string `json:"repo"`
	IID            int64  `json:"iid"`
	Title          string `json:"title"`
	SourceBranch   string `json:"source_branch"`
	TargetBranch   string `json:"target_branch,omitempty"`
	State          State  `json:"state"`
	Reason         string `json:"reason,omitempty"`
	WebURL         string `json:"web_url,omitempty"`
	PipelineStatus string `json:"pipeline_status,omitempty"`
	PipelineURL    string `json:"pipeline_url,omitempty"`
	// PipelineID is the head pipeline id (0 when there is no head pipeline).
	// The shepherd (M4) needs it to retry a flaky-classified pipeline.
	PipelineID int64 `json:"pipeline_id,omitempty"`
	// SHA is the source-branch head commit observed by the poll that produced
	// this record. The shepherd sends it as GitLab's merge `sha` precondition so
	// an arm can only apply to the head it actually classified; if the branch
	// moved since, GitLab rejects the arm (409) instead of arming a new,
	// unreviewed head. Empty means "not observed" → the shepherd refuses to arm.
	SHA string `json:"sha,omitempty"`
	// CreatedAt is when the MR was opened. The shepherd gates age-sensitive
	// actions on it (arm auto-merge only after 30 min, create pipeline only
	// after 10 min). Zero when the source did not populate it.
	CreatedAt        time.Time `json:"created_at,omitempty"`
	LastTransitionAt time.Time `json:"last_transition_at"`
	Stale            bool      `json:"stale"`
	// Merged is the explicit positive merge marker, true if and only if State
	// is StateMerged. It is emitted alongside the state so a consumer can match
	// on either without re-deriving the taxonomy; a closed-unmerged MR never
	// sets it.
	Merged bool `json:"merged,omitempty"`
	// MergedAt is GitLab's merged_at for a merged MR. Zero when the source did
	// not report one — retention then anchors on LastTransitionAt.
	MergedAt time.Time `json:"merged_at,omitempty"`
}

// Snapshot is the full registry state served by GET /api/mrwatch/summary.
// MRs and Counts are always non-nil.
type Snapshot struct {
	MergeRequests []MergeRequest `json:"merge_requests"`
	Counts        map[string]int `json:"counts"`
	LastPollAt    time.Time      `json:"last_poll_at"`
	// Stale is true when the most recent poll attempt failed for at least one
	// configured project and the registry is serving retained data.
	Stale bool `json:"stale"`
	// Projects is the set of GitLab project paths the poller watches.
	Projects []string `json:"projects"`
}

// emptySnapshot returns a zero-value snapshot with non-nil slices/maps so it
// encodes as {"merge_requests":[],"counts":{...},"projects":[]} rather than
// nulls. Used by the poller before its first successful poll and by the domain
// adapter when the poller is disabled.
func emptySnapshot(projects []string) Snapshot {
	counts := make(map[string]int, len(AllStates()))
	for _, s := range AllStates() {
		counts[string(s)] = 0
	}
	if projects == nil {
		projects = []string{}
	}
	return Snapshot{
		MergeRequests: []MergeRequest{},
		Counts:        counts,
		Projects:      projects,
	}
}
