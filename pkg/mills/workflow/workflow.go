package workflow

import (
	"fmt"
	"strings"
	"time"
)

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
		if strings.TrimSpace(state.State) == "" || state.ObservedAt.IsZero() || (i > 0 && state.ObservedAt.Before(e.States[i-1].ObservedAt)) {
			return fmt.Errorf("queued-proof state %d is missing or unordered", i)
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
	if e.MR.State != "merged" {
		return fmt.Errorf("queued-proof MR state %q is not auto-merged proof", e.MR.State)
	}
	if e.MR.ChangedFiles <= 0 || e.MR.Additions+e.MR.Deletions <= 0 {
		return fmt.Errorf("queued-proof MR %s!%d has an empty diff", e.MR.Project, e.MR.IID)
	}
	return nil
}
