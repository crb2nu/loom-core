package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func newJournalTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatalf("open journal store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestDAOJournalAppendPreservesPrecreatedRunMetadata(t *testing.T) {
	st := newJournalTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	run := &store.WorkflowRun{
		ID:                 "WF-JOURNAL-PRECREATED",
		Engine:             store.WorkflowEngineImperative,
		Template:           "real-workflow",
		TemplateVersion:    "v9",
		InterpreterVersion: "starlark-pinned",
		WorkflowParams:     `{"plan":"customer"}`,
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
		ParentSessionID:    "session-original",
	}
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("seed workflow run: %v", err)
	}

	journal := NewDAOJournal(ctx, st)
	if err := journal.Append(Record{
		RunID: run.ID, StepKey: "gate:0", PrimName: "gate",
		CallHash: "gate-hash", Status: StatusPending,
	}); err != nil {
		t.Fatalf("append first journal step: %v", err)
	}

	stored, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get workflow run: %v", err)
	}
	if stored.Template != run.Template ||
		stored.TemplateVersion != run.TemplateVersion ||
		stored.InterpreterVersion != run.InterpreterVersion ||
		stored.WorkflowParams != run.WorkflowParams ||
		stored.ParentSessionID != run.ParentSessionID {
		t.Fatalf("first journal append overwrote pre-created metadata: %+v", stored)
	}
}

func TestDAOJournalAppendRejectsPrecreatedDAGRun(t *testing.T) {
	st := newJournalTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	run := &store.WorkflowRun{
		ID:                 "WF-JOURNAL-DAG",
		Engine:             store.WorkflowEngineDAG,
		Template:           "legacy-dag",
		TemplateVersion:    "v1",
		InterpreterVersion: "dag-runner-v1",
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("seed DAG run: %v", err)
	}

	journal := NewDAOJournal(ctx, st)
	err := journal.Append(Record{
		RunID: run.ID, StepKey: "agent:0", PrimName: "agent",
		CallHash: "agent-hash", Status: StatusPending,
	})
	if !errors.Is(err, store.ErrWorkflowRunMetadataMismatch) {
		t.Fatalf("journal append error = %v, want engine metadata mismatch", err)
	}
	steps, listErr := st.Workflow.ListByRun(ctx, run.ID)
	if listErr != nil {
		t.Fatalf("list DAG steps: %v", listErr)
	}
	if len(steps) != 0 {
		t.Fatalf("journal appended %d imperative steps under DAG run", len(steps))
	}
}

func TestDAOJournalCallHashMismatchReportsStoredAndIncomingHashes(t *testing.T) {
	st := newJournalTestStore(t)
	ctx := context.Background()
	journal := NewDAOJournal(ctx, st)
	recorded := Record{
		RunID: "WF-JOURNAL-HASH-MISMATCH", StepKey: "agent:0", PrimName: "agent",
		CallHash: "recorded-hash", Status: StatusPending,
	}
	if err := journal.Append(recorded); err != nil {
		t.Fatalf("append recorded step: %v", err)
	}

	incoming := recorded
	incoming.CallHash = "incoming-hash"
	err := journal.Append(incoming)
	var quarantine *QuarantineError
	if !errors.As(err, &quarantine) {
		t.Fatalf("mismatch error = %v, want *QuarantineError", err)
	}
	if quarantine.StepKey != incoming.StepKey || quarantine.Want != recorded.CallHash || quarantine.Got != incoming.CallHash {
		t.Fatalf("quarantine hashes = %+v, want stored=%q incoming=%q", quarantine, recorded.CallHash, incoming.CallHash)
	}
}

func TestDAOJournalConcurrentFirstAppends(t *testing.T) {
	st := newJournalTestStore(t)
	ctx := context.Background()
	journal := NewDAOJournal(ctx, st)
	const workers = 64
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- journal.Append(Record{
				RunID: "WF-JOURNAL-CONCURRENT", StepKey: fmt.Sprintf("agent:%d", i), PrimName: "agent",
				CallHash: fmt.Sprintf("hash-%d", i), Status: StatusPending,
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent journal append: %v", err)
		}
	}
	steps, err := st.Workflow.ListByRun(ctx, "WF-JOURNAL-CONCURRENT")
	if err != nil {
		t.Fatalf("list concurrent steps: %v", err)
	}
	if len(steps) != workers {
		t.Fatalf("concurrent journal recorded %d steps, want %d", len(steps), workers)
	}
}
