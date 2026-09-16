package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QueuedTargetStampProof is the fail-closed contract used by the queued-proof
// kill-test. A proof is valid only when one target-bound stamp is admitted,
// retains its destination through landing, and a replay is rejected as a
// collision instead of overwriting the original delivery intent.
type QueuedTargetStampProof struct {
	StampID             string   `json:"stamp_id"`
	TargetProject       string   `json:"target_project"`
	LandedTargetProject string   `json:"landed_target_project"`
	QueueStates         []string `json:"queue_states"`
	Admissions          int      `json:"admissions"`
	CollisionDetected   bool     `json:"collision_detected"`
}

// AssertQueuedTargetStampProof rejects missing or contradictory stamp proof.
func AssertQueuedTargetStampProof(p QueuedTargetStampProof) error {
	p.StampID = strings.TrimSpace(p.StampID)
	p.TargetProject = strings.TrimSpace(p.TargetProject)
	p.LandedTargetProject = strings.TrimSpace(p.LandedTargetProject)
	if p.StampID == "" || p.TargetProject == "" {
		return fmt.Errorf("queued target stamp identity is missing")
	}
	if p.LandedTargetProject != p.TargetProject {
		return fmt.Errorf("queued target stamp %q landed on %q, want %q", p.StampID, p.LandedTargetProject, p.TargetProject)
	}
	if len(p.QueueStates) != 2 || p.QueueStates[0] != "queued" || p.QueueStates[1] != "admitted" {
		return fmt.Errorf("queued target stamp %q has incomplete queue evidence: %v", p.StampID, p.QueueStates)
	}
	if p.Admissions != 1 {
		return fmt.Errorf("queued target stamp %q admissions=%d, want 1", p.StampID, p.Admissions)
	}
	if !p.CollisionDetected {
		return fmt.Errorf("queued target stamp %q has no duplicate collision evidence", p.StampID)
	}
	return nil
}

// QueuedProofEvidence is the durable report produced by the live queued-proof
// kill-test. It binds one queued backlog item to one terminal pipeline run and
// the exact GitLab merge request diff produced by that run.
type QueuedProofEvidence struct {
	CapturedAt time.Time               `json:"captured_at"`
	BacklogID  string                  `json:"backlog_id"`
	RunID      string                  `json:"run_id"`
	States     []QueuedProofState      `json:"states"`
	Terminal   QueuedProofTerminal     `json:"terminal"`
	MR         QueuedProofMergeRequest `json:"merge_request"`
}

type QueuedProofState struct {
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}

type QueuedProofTerminal struct {
	State       string `json:"state"`
	Quarantined bool   `json:"quarantined"`
}

type QueuedProofMergeRequest struct {
	Project      string `json:"project"`
	IID          int64  `json:"iid"`
	URL          string `json:"url"`
	State        string `json:"state"`
	ChangedFiles int    `json:"changed_files"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
}

// AssertQueuedProof fails closed unless the evidence proves exactly one live
// queue identity reached a successful terminal state with a non-empty MR diff.
func AssertQueuedProof(e QueuedProofEvidence) error {
	if e.CapturedAt.IsZero() || strings.TrimSpace(e.BacklogID) == "" || strings.TrimSpace(e.RunID) == "" {
		return fmt.Errorf("queued-proof identity or capture time is missing")
	}
	if len(e.States) < 2 || e.States[0].State != "queued" {
		return fmt.Errorf("queued-proof does not begin with a queued observation")
	}
	queuedObservations := 0
	for i, state := range e.States {
		if strings.TrimSpace(state.State) == "" || state.ObservedAt.IsZero() || state.ObservedAt.After(e.CapturedAt) || (i > 0 && !state.ObservedAt.After(e.States[i-1].ObservedAt)) {
			return fmt.Errorf("queued-proof state %d is missing or unordered", i)
		}
		if i > 0 && state.State == e.States[i-1].State {
			return fmt.Errorf("queued-proof state %d does not progress from %q", i, state.State)
		}
		if state.State == "quarantined" {
			return fmt.Errorf("queued-proof run %s was quarantined", e.RunID)
		}
		if state.State == "queued" {
			queuedObservations++
		}
	}
	if queuedObservations != 1 {
		return fmt.Errorf("queued-proof requires exactly one queued observation; got %d", queuedObservations)
	}
	if e.States[len(e.States)-1].State != e.Terminal.State {
		return fmt.Errorf("queued-proof terminal state contradicts the final observation")
	}
	if e.Terminal.Quarantined || e.Terminal.State == "quarantined" {
		return fmt.Errorf("queued-proof run %s was quarantined", e.RunID)
	}
	if e.Terminal.State != "done" {
		return fmt.Errorf("queued-proof run %s terminal state is %q, want done", e.RunID, e.Terminal.State)
	}
	if e.MR.IID <= 0 || strings.TrimSpace(e.MR.Project) == "" || strings.TrimSpace(e.MR.URL) == "" {
		return fmt.Errorf("queued-proof terminal MR identity is missing")
	}
	wantMRSuffix := "/-/merge_requests/" + strconv.FormatInt(e.MR.IID, 10)
	legacyMRSuffix := "/mr/" + strconv.FormatInt(e.MR.IID, 10)
	trimmedMRURL := strings.TrimRight(e.MR.URL, "/")
	if !strings.HasSuffix(trimmedMRURL, wantMRSuffix) && !strings.HasSuffix(trimmedMRURL, legacyMRSuffix) {
		return fmt.Errorf("queued-proof MR URL %q contradicts IID %d", e.MR.URL, e.MR.IID)
	}
	if e.MR.State != "merged" {
		return fmt.Errorf("queued-proof MR state %q is not auto-merged proof", e.MR.State)
	}
	if e.MR.ChangedFiles <= 0 || e.MR.Additions+e.MR.Deletions <= 0 {
		return fmt.Errorf("queued-proof MR %s!%d has an empty diff", e.MR.Project, e.MR.IID)
	}
	return nil
}
