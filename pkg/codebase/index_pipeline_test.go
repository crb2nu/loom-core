package codebase

import (
	"testing"

	"github.com/crb2nu/loom/pkg/codebase/schema"
)

// TestRecordFlushWarning_DoesNotMarkJobFailed asserts that a job-final
// Flush failure is surfaced as a soft warning on job stats but the job
// still reports status=done. This is the structural invariant the
// indexer must preserve: an EOF on the trailing durability ping cannot
// mark a fully-processed run as failed, because all data has already
// landed in Qdrant via wait=false upserts and is durable through WAL
// fsync regardless.
func TestRecordFlushWarning_DoesNotMarkJobFailed(t *testing.T) {
	t.Parallel()

	svc := &Service{
		jobs: map[string]*indexJob{},
	}
	jobID := "test-job"
	svc.jobs[jobID] = &indexJob{
		id:     jobID,
		status: "running",
		stats:  schema.IndexStats{RepoID: "r", FilesTotal: 10, FilesDone: 10},
	}

	svc.recordFlushWarning(jobID, "job-final flush: EOF")
	svc.setJobDone(jobID)

	job := svc.jobs[jobID]
	if job.status != "done" {
		t.Fatalf("status=%q want \"done\" after recordFlushWarning + setJobDone", job.status)
	}
	if job.stats.FlushWarnings != 1 {
		t.Fatalf("FlushWarnings=%d want 1", job.stats.FlushWarnings)
	}
	if job.stats.LastFlushWarning == "" {
		t.Fatalf("LastFlushWarning should be set to the warning message")
	}
	if job.stats.Errors != 0 {
		t.Fatalf("Errors=%d want 0 (flush warning must NOT bump hard error count)", job.stats.Errors)
	}
	if job.err != "" {
		t.Fatalf("job.err=%q want empty (flush warning is soft)", job.err)
	}
}

// TestRecordFlushWarning_IsAdditive asserts the counter increments on
// repeated calls (e.g. if a future code path retries Flush).
func TestRecordFlushWarning_IsAdditive(t *testing.T) {
	t.Parallel()

	svc := &Service{jobs: map[string]*indexJob{}}
	jobID := "j"
	svc.jobs[jobID] = &indexJob{id: jobID, status: "running"}

	svc.recordFlushWarning(jobID, "first warn")
	svc.recordFlushWarning(jobID, "second warn")

	got := svc.jobs[jobID].stats.FlushWarnings
	if got != 2 {
		t.Fatalf("FlushWarnings=%d want 2", got)
	}
	if last := svc.jobs[jobID].stats.LastFlushWarning; last != "second warn" {
		t.Fatalf("LastFlushWarning=%q want %q", last, "second warn")
	}
}

// TestCheckVectorDim_RejectsMismatch is the regression test for the
// 2026-07-09 silent-drop incident: morph-embedding-v3 changed its output
// dimension in place (1024 -> 1536), and mixed batches of cached 1024-dim
// and fresh 1536-dim vectors passed the first-vector-only collection guard.
// With wait=false upserts Qdrant ACKed the mismatched points into the WAL
// and silently declined them at apply time, so every fresh point was lost
// without any client-visible error. checkVectorDim must reject every
// per-point mismatch before the upsert is issued.
func TestCheckVectorDim_RejectsMismatch(t *testing.T) {
	t.Parallel()

	vec := func(n int) []float64 {
		v := make([]float64, n)
		if n > 0 {
			v[0] = 1
		}
		return v
	}

	cases := []struct {
		name    string
		vector  []float64
		dim     int
		wantErr bool
	}{
		{name: "match", vector: vec(1024), dim: 1024, wantErr: false},
		{name: "drifted model output", vector: vec(1536), dim: 1024, wantErr: true},
		{name: "stale cached vector", vector: vec(1024), dim: 1536, wantErr: true},
		{name: "empty vector", vector: nil, dim: 1024, wantErr: true},
		{name: "empty vector even without known dim", vector: nil, dim: 0, wantErr: true},
		{name: "unknown collection dim skips dim check", vector: vec(768), dim: 0, wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkVectorDim("chunk-1", tc.vector, tc.dim)
			if tc.wantErr && err == nil {
				t.Fatalf("checkVectorDim(dim=%d, len=%d) = nil, want error", tc.dim, len(tc.vector))
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkVectorDim(dim=%d, len=%d) = %v, want nil", tc.dim, len(tc.vector), err)
			}
		})
	}
}
