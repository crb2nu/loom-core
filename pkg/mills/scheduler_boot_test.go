package mills

import (
	"context"
	"testing"
	"time"
)

// TestReconciler_StaggerHousekeepingOrdersSweeps pins the offsets and the
// never-earlier rule.
func TestReconciler_StaggerHousekeepingOrdersSweeps(t *testing.T) {
	env := newRecEnv(t, nil)
	r := env.rec
	from := env.now
	step := time.Minute
	// Everything is due at boot before the stagger.
	if !r.retentionDue(from) {
		t.Fatal("retention should be due on a fresh reconciler")
	}
	r.staggerHousekeeping(from, step)
	want := []struct {
		name string
		due  func(time.Time) bool
		at   time.Time
	}{
		{"regression", r.regressionSweepDue, from.Add(1 * step)},
		{"signature mining", r.signatureMiningDue, from.Add(2 * step)},
		{"learning signals", r.learningSignalDue, from.Add(3 * step)},
		{"retention", r.retentionDue, from.Add(4 * step)},
	}
	// The due-checks also require their enablers; wire the minimum so the
	// time component is what is under test.
	r.LearningSignals = stubLearningSignalPublisher{}
	r.SignatureEvidenceClassified = func(string) bool { return true }
	r.RegressionMergedMRs = stubMergedMRLister{}
	r.RegressionCommits = stubBranchCommitLister{}
	for _, w := range want {
		if w.due(from) {
			t.Fatalf("%s due on the boot tick after stagger", w.name)
		}
		if w.due(w.at.Add(-time.Second)) {
			t.Fatalf("%s due before its offset", w.name)
		}
		if !w.due(w.at) {
			t.Fatalf("%s not due at its offset %s", w.name, w.at)
		}
	}
	// Never earlier: a sweep that already stamped a later next-run keeps it.
	r.nextRetention = from.Add(24 * time.Hour)
	r.staggerHousekeeping(from, step)
	if !r.nextRetention.Equal(from.Add(24 * time.Hour)) {
		t.Fatalf("stagger pulled retention earlier: %s", r.nextRetention)
	}
}

type stubLearningSignalPublisher struct{}

func (stubLearningSignalPublisher) PublishLearningSignals(context.Context, time.Time, time.Time) (LearningSignalSweepResult, error) {
	return LearningSignalSweepResult{}, nil
}

type stubMergedMRLister struct{}

func (stubMergedMRLister) ListMergedMRs(context.Context, time.Time, int) ([]MergedMRRecord, error) {
	return nil, nil
}

type stubBranchCommitLister struct{}

func (stubBranchCommitLister) ListBranchCommits(context.Context, string, time.Time, int) ([]BranchCommitRecord, error) {
	return nil, nil
}

// TestScheduler_BootTickReleasesPhaseAndStaggersHousekeeping is the boot
// ordering contract on the scheduler side: OnBootTick fires once, after the
// boot reconcile has returned, and the boot tick itself ran with the
// housekeeping sweeps pushed onto later ticks.
func TestScheduler_BootTickReleasesPhaseAndStaggersHousekeeping(t *testing.T) {
	env := newRecEnv(t, nil)
	bootTicks := 0
	env.rec.AutonomyGate = func(context.Context) (bool, []string) {
		bootTicks++
		return false, []string{"test no-op"}
	}
	phase := NewBootPhase("reconciler_boot_tick", nil)
	sch := NewScheduler(env.rec)
	sch.Interval = time.Hour
	sch.Logger = nil
	sch.OnBootTick = phase.Release

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sch.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(time.Second):
			t.Error("scheduler did not stop")
		}
	})

	if !phase.Wait(ctx, 2*time.Second) {
		t.Fatal("OnBootTick did not release the phase")
	}
	if bootTicks != 1 {
		t.Fatalf("boot reconciles before release = %d, want 1", bootTicks)
	}
	// Retention (the last staggered sweep) is pushed 4 intervals out from
	// the reconciler's clock; it must not have been due on the boot tick.
	if env.rec.retentionDue(env.now) {
		t.Fatal("retention sweep still due on the boot tick")
	}
	if !env.rec.retentionDue(env.now.Add(4 * time.Hour)) {
		t.Fatalf("retention not due after 4 intervals: next=%s", env.rec.nextRetention)
	}
}

func TestScheduler_HousekeepingStaggerResolution(t *testing.T) {
	s := &Scheduler{}
	if got := s.housekeepingStagger(time.Minute); got != time.Minute {
		t.Fatalf("zero stagger resolved to %s, want the interval", got)
	}
	s.HousekeepingStagger = -1
	if got := s.housekeepingStagger(time.Minute); got != 0 {
		t.Fatalf("negative stagger resolved to %s, want disabled", got)
	}
	s.HousekeepingStagger = 30 * time.Second
	if got := s.housekeepingStagger(time.Minute); got != 30*time.Second {
		t.Fatalf("explicit stagger resolved to %s", got)
	}
}
