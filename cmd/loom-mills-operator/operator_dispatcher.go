package main

import "github.com/crb2nu/loom/pkg/mills/pipeline"

// newOperatorDispatcher routes every operator stage through the canonical
// JobContext builder. Unmapped bring-up stages retain the historical no-op
// behavior through the Worker-compatible fallback.
func newOperatorDispatcher(routes map[string]pipeline.Worker) pipeline.WorkerDispatcher {
	return pipeline.NewDispatcher(routes, &pipeline.NoOpWorker{})
}
