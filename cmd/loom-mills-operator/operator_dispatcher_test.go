package main

import (
	"context"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type operatorRecordingSpawn struct{ requests []pipeline.SpawnRequest }

func (s *operatorRecordingSpawn) Run(_ context.Context, req pipeline.SpawnRequest) (pipeline.SpawnResponse, error) {
	s.requests = append(s.requests, req)
	return pipeline.SpawnResponse{SpawnID: "spawn-retry"}, nil
}

func TestOperatorDispatcher_RetrySpawnUsesAttemptAndFeedback(t *testing.T) {
	spawn := &operatorRecordingSpawn{}
	worker := &pipeline.SpawnWorker{Client: spawn, Project: "loom-core", PromptFor: implementPromptFor(nil), BaseBranch: "main"}
	d := newOperatorDispatcher(map[string]pipeline.Worker{"implement": worker})
	ctx := pipeline.WithStageAttempt(context.Background(), 2)
	ctx = pipeline.WithStageRetryContext(ctx, &pipeline.StageRetryContext{Attempt: 2, GateStage: "post_implement_gate", FirstFailure: "docs_guardrail: missing changelog fragment"})
	run := &store.PipelineRun{ID: "PIPE-RETRY", BacklogID: "bl-retry"}
	item := &store.BacklogItem{ID: "bl-retry", Title: "retry correctly"}

	if _, err := d.Dispatch(ctx, run, item, pipeline.Stage{ID: "implement"}, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(spawn.requests) != 1 {
		t.Fatalf("spawn requests = %d, want 1", len(spawn.requests))
	}
	req := spawn.requests[0]
	if !strings.HasSuffix(req.IdempotencyKey, ":2") {
		t.Errorf("IdempotencyKey = %q, want suffix :2", req.IdempotencyKey)
	}
	for _, want := range []string{"post_implement_gate", "docs_guardrail: missing changelog fragment"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q: %s", want, req.Prompt)
		}
	}
}
