package pipeline

import (
	"context"
	"testing"
)

func TestNoOpWorkerMatchesNoOpDispatcher(t *testing.T) {
	stage := Stage{ID: "implement"}
	want, err := (&NoOpDispatcher{MRIID: 7, Cost: 0.25}).Dispatch(context.Background(), nil, nil, stage, nil)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	got, err := (&NoOpWorker{MRIID: 7, Cost: 0.25}).Run(context.Background(), JobContext{Stage: stage})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if got.CostUSD != want.CostUSD || string(got.DiffPatch) != string(want.DiffPatch) || got.LinesAdded != want.LinesAdded {
		t.Fatalf("worker output = %+v, want dispatcher-equivalent %+v", got, want)
	}
}
