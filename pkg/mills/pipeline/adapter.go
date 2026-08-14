package pipeline

import (
	"context"
	"errors"
	"log/slog"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// RunnerStarter is the operator-facing PipelineStarter. It picks the
// fan-out path (Integrator → Runner) when a backlog item declares
// parallel slices, and the straight Runner path otherwise. Either way
// it returns nil immediately so the reconciler tick stays cheap; the
// real work happens in a goroutine that reports progress via
// stage_results + events.
//
// This is the single binding point between the reconciler and the
// pipeline package: mills.PipelineStarter → RunnerStarter → Runner /
// Integrator → WorkerDispatcher → real backing clients.
type RunnerStarter struct {
	Runner     *Runner
	Integrator *Integrator
	Logger     *slog.Logger
}

// NewRunnerStarter wires the two drivers together. Either may be nil:
// a nil Integrator forces the Runner path; a nil Runner is invalid
// because the parent run still needs the post-merge stages driven.
func NewRunnerStarter(r *Runner, i *Integrator) *RunnerStarter {
	return &RunnerStarter{
		Runner:     r,
		Integrator: i,
		Logger:     slog.Default(),
	}
}

// Start satisfies mills.PipelineStarter.
func (s *RunnerStarter) Start(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if s == nil || s.Runner == nil {
		return errors.New("pipeline: starter not configured")
	}
	if run == nil || run.ID == "" {
		return errors.New("pipeline: run.ID required")
	}
	if item == nil || item.ID == "" {
		return errors.New("pipeline: item.ID required")
	}
	if ShouldFanOut(item) && s.Integrator != nil {
		// Fan-out must share Runner's process-local run-ID guard. Without
		// this claim, duplicate reconciler deliveries each launch their own
		// Integrator goroutine for the same durable run.
		if !s.Runner.activate(run.ID) {
			return nil
		}
		if !shouldStartFanOut(run) {
			// This run resumes on the ordinary Runner path. Release our
			// provisional claim so Runner.Start can own the same lifecycle.
			s.Runner.deactivate(run.ID)
			return s.Runner.Start(ctx, run, item)
		}

		// The Integrator replaces the supplied run value when fan-out
		// completes. Drive a private copy so concurrent duplicate Start
		// calls only read the caller-owned run ID and never race that write.
		fanOutRun := *run
		// Register on the Runner's waitgroup so Runner.Wait (and our own
		// Wait) covers fan-out goroutines too. Add before launching so a
		// concurrent Wait can't miss it.
		s.Runner.wg.Add(1)
		go func() {
			defer s.Runner.wg.Done()
			defer s.Runner.deactivate(fanOutRun.ID)
			s.driveFanOut(&fanOutRun, item)
		}()
		return nil
	}
	return s.Runner.Start(ctx, run, item)
}

// Wait blocks until every goroutine this starter launched (fan-out drives
// and Runner.Start drives) has exited. Delegates to the shared Runner
// waitgroup. Tests call this before closing the store.
func (s *RunnerStarter) Wait() {
	if s == nil || s.Runner == nil {
		return
	}
	s.Runner.Wait()
}

func shouldStartFanOut(run *store.PipelineRun) bool {
	if run == nil {
		return false
	}
	return run.State == store.PipelineQueued ||
		run.State == store.PipelineSlicing ||
		run.CurrentStage == "fan_out"
}

// driveFanOut runs the integrator to completion (sub-runs + merge), then
// hands the parent run off to the Runner from its post-merge resume
// point (PipelineMR). The runner picks up at the mr stage if the
// integrator hasn't already produced one, otherwise at ci_watch.
//
// We use a detached background context so a reconciler tick that
// returns doesn't cancel an in-flight fan-out.
func (s *RunnerStarter) driveFanOut(run *store.PipelineRun, item *store.BacklogItem) {
	ctx := context.Background()
	if err := s.Integrator.Run(ctx, run, item); err != nil {
		s.logger().Error("integrator drive failed", "run", run.ID, "error", err)
		if eerr := s.Runner.escalateWithItem(ctx, run, item, Classify(err), "integrator drive failed: "+err.Error()); eerr != nil {
			s.logger().Error("integrator drive failure escalation failed", "run", run.ID, "error", eerr)
		}
		return
	}
	// On success the integrator left run.State = PipelineMR with
	// CurrentStage = "mr". The Runner resumes from there and drives
	// mr → ci → merge → cleanup against the integration branch.
	if run.State != store.PipelineMR {
		// Integrator didn't reach MR (e.g. escalated). Nothing to do —
		// the run row already carries the terminal state.
		return
	}
	if err := s.Runner.Drive(ctx, run, item); err != nil {
		s.logger().Error("post-fanout runner drive failed", "run", run.ID, "error", err)
		if eerr := s.Runner.escalateWithItem(ctx, run, item, Classify(err), "post-fanout runner drive failed: "+err.Error()); eerr != nil {
			s.logger().Error("post-fanout drive failure escalation failed", "run", run.ID, "error", eerr)
		}
	}
}

func (s *RunnerStarter) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
