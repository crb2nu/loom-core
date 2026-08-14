// Package mills_test holds the one mining test that must run against the REAL
// classifier corpus. pkg/mills/pipeline imports pkg/mills, so an in-package
// test cannot reach the classifiers without an import cycle; the external test
// package can, and proving the exclusion against a stub instead of the real
// predicate would prove nothing at all.
package mills_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedClassifierEvidence writes one escalated run carrying evidence, leaving
// every escalation column empty: the run looks unexplained to the STORE, so the
// only thing that can exclude it is the classifier predicate itself.
func seedClassifierEvidence(t *testing.T, st *store.Store, id string, startedAt time.Time, evidence string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &store.BacklogItem{
		ID: id, Title: "mined " + id, State: store.BacklogEscalated,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", id, err)
	}
	run := &store.PipelineRun{
		ID: "RUN-" + id, BacklogID: id, Template: "t",
		State: store.PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
		StartedAt: startedAt,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run %s: %v", run.ID, err)
	}
	outcome := store.StageOutcomeError
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "ci_watch", Attempt: 1,
		StartedAt: startedAt, Outcome: &outcome, LogTail: evidence,
	}); err != nil {
		t.Fatalf("put stage %s: %v", run.ID, err)
	}
}

// TestSignatureMiningExcludesRealClassifierMatches: three escalations of a
// shape pkg/mills/pipeline already classifies produce no proposal, while three
// of a shape it does not produce exactly one. The classified runs carry no
// escalation columns, so the store-level filter cannot help here — the real
// classifier is doing the excluding.
func TestSignatureMiningExcludesRealClassifierMatches(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: t.TempDir() + "/mining.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	rec := &mills.Reconciler{Store: st}
	rec.Clock = func() time.Time { return now }
	rec.SignatureEvidenceClassified = pipeline.KnownFailureSignature

	for i := 1; i <= 3; i++ {
		// Recognised by classifyObservedExternalIncident (clickhouse merge task).
		seedClassifierEvidence(t, st, fmt.Sprintf("MILLS-KNOWN-%d", i), now.Add(-time.Duration(i)*time.Hour),
			fmt.Sprintf("clickhouse: failed to execute merge task on shard %d (part all_%d_%d_0)", i, i, i))
		// Recognised by nothing.
		seedClassifierEvidence(t, st, fmt.Sprintf("MILLS-NOVEL-%d", i), now.Add(-time.Duration(i)*time.Hour),
			fmt.Sprintf("fatal: knitter sidecar refused sync token for shard %d after %ds", i, i*3))
	}

	res, err := rec.SweepSignatureMining(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.TextsScanned != 6 {
		t.Fatalf("scanned = %d, want 6", res.TextsScanned)
	}
	if res.Unclassified != 3 {
		t.Fatalf("unclassified = %d, want 3 — the clickhouse shape is already classified (%+v)", res.Unclassified, res)
	}
	if res.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1 (%+v)", res.Candidates, res)
	}

	events, err := st.Events.ListByActorSince(ctx, mills.SignatureMinerActor, now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("candidate events = %d, want 1: %+v", len(events), events)
	}
	phrase, _ := events[0].Payload["phrase"].(string)
	if phrase == "" || pipeline.KnownFailureSignature(phrase) {
		t.Errorf("proposed phrase %q is already covered by the live classifiers", phrase)
	}
}
