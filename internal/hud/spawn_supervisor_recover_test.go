package hud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

// fakeStreamBackend is a recordingBackend that also satisfies streamExecCapable
// so runSpawnReattach takes the reattach path. The real StreamExec is never
// invoked in these tests (reattachExecFn is seamed), so nil k8s handles are
// safe.
type fakeStreamBackend struct{ recordingBackend }

func (f *fakeStreamBackend) Clientset() kubernetes.Interface { return nil }
func (f *fakeStreamBackend) RestConfig() *rest.Config        { return nil }
func (f *fakeStreamBackend) Namespace() string               { return "devbox" }
func (f *fakeStreamBackend) NFSFlush() bool                  { return false }

func seedSupervisedRunningSpawn(t *testing.T, ctrl *spawn.K8sController, supervised bool) (string, spawn.Request) {
	t.Helper()
	ctx := context.Background()
	req := spawn.Request{
		AgentType:       "claude-code",
		TaskDescription: "supervised turn",
		Project:         "services/loom-core",
		IdempotencyKey:  "wf-supervised",
		TimeoutMinutes:  30,
	}
	id, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("seed supervised spawn: %v", err)
	}
	st, _ := ctrl.Get(id)
	st.Status = spawn.StatusRunning
	st.PodName = "spawn-" + id
	st.Supervised = supervised
	ctrl.UpdateState(ctx, st)
	return id, req
}

// A supervised keyed spawn must classify to interruptedReattach (never
// re-drive), while a non-supervised keyed spawn keeps the legacy re-drive path
// so older-controller pods still recover.
func TestClassifyInterruptedSpawn_SupervisedReattach(t *testing.T) {
	keyed := spawn.Request{
		AgentType: "claude-code", TaskDescription: "t",
		Project: "services/loom-core", IdempotencyKey: "wf-x",
	}
	supervised := &spawn.State{Status: spawn.StatusRunning, PodName: "spawn-x", Request: keyed, Supervised: true}
	if got := classifyInterruptedSpawn(supervised); got != interruptedReattach {
		t.Fatalf("supervised keyed running = %v, want interruptedReattach", got)
	}
	legacy := &spawn.State{Status: spawn.StatusRunning, PodName: "spawn-x", Request: keyed, Supervised: false}
	if got := classifyInterruptedSpawn(legacy); got != interruptedRedrive {
		t.Fatalf("non-supervised keyed running = %v, want interruptedRedrive", got)
	}
	// A terminal supervised spawn is still a skip (freeze).
	term := &spawn.State{Status: spawn.StatusCompleted, Request: keyed, Supervised: true}
	if got := classifyInterruptedSpawn(term); got != interruptedSkip {
		t.Fatalf("terminal supervised = %v, want interruptedSkip", got)
	}
}

// recoverInterruptedSpawns dispatches a supervised spawn to reattach when the
// probe finds a live reaper or a recorded outcome, and to re-drive (liveness)
// when the supervisor is gone or the probe errors.
func TestRecoverInterruptedSpawns_SupervisedDispatch(t *testing.T) {
	cases := []struct {
		name         string
		probe        supervisorProbe
		probeErr     error
		wantReattach bool
		wantRedrive  bool
	}{
		{"reaper alive -> reattach", supervisorProbe{Found: true, ReaperAlive: true}, nil, true, false},
		{"outcome present -> reattach(collect)", supervisorProbe{Found: true, OutcomePresent: true}, nil, true, false},
		{"supervisor gone -> redrive", supervisorProbe{}, nil, false, true},
		{"probe error -> redrive", supervisorProbe{}, context.DeadlineExceeded, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := spawn.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
			id, _ := seedSupervisedRunningSpawn(t, ctrl, true)

			var reattached, redriven []string
			o := &SpawnOrchestrator{
				ctrl:             ctrl,
				logger:           slog.Default(),
				backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
				defaultSubstrate: DefaultSubstrate,
				reattachSpawn:    func(id string, _ SpawnRequest) { reattached = append(reattached, id) },
				redriveSpawn:     func(id string, _ SpawnRequest) { redriven = append(redriven, id) },
				probeSupervisorFn: func(_ context.Context, _, _ string) (supervisorProbe, error) {
					return tc.probe, tc.probeErr
				},
			}

			o.recoverInterruptedSpawns()

			if tc.wantReattach && (len(reattached) != 1 || reattached[0] != id) {
				t.Fatalf("reattached = %v, want [%s]", reattached, id)
			}
			if !tc.wantReattach && len(reattached) != 0 {
				t.Fatalf("unexpected reattach %v", reattached)
			}
			if tc.wantRedrive && (len(redriven) != 1 || redriven[0] != id) {
				t.Fatalf("redriven = %v, want [%s]", redriven, id)
			}
			if !tc.wantRedrive && len(redriven) != 0 {
				t.Fatalf("unexpected redrive %v", redriven)
			}
			// The recovery loop itself must never terminalize the spawn.
			got, _ := ctrl.Get(id)
			if got.Status != spawn.StatusRunning {
				t.Fatalf("recovery terminalized spawn: status=%s", got.Status)
			}
		})
	}
}

// markerExec returns a reattachExecFn seam that emits the given attach-mode
// marker line via onLine (the out-of-band status channel) and returns exit 0,
// mirroring the real attach launcher.
func markerExec(marker string, execCalls *int) func(*spawnDriverOwner, streamExecCapable, string, string, int, func([]byte)) (*backend.ExecResult, error) {
	return func(_ *spawnDriverOwner, _ streamExecCapable, _, _ string, _ int, onLine func([]byte)) (*backend.ExecResult, error) {
		if execCalls != nil {
			*execCalls++
		}
		onLine([]byte(marker))
		return &backend.ExecResult{ExitCode: 0}, nil
	}
}

// runSpawnReattach delivers the reaper's outcome exactly once: a success
// outcome completes the spawn, a repeat reattach is a terminal-frozen no-op
// (never a second outcome).
func TestRunSpawnReattach_ExactlyOnceComplete(t *testing.T) {
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
	id, req := seedSupervisedRunningSpawn(t, ctrl, true)

	execCalls := 0
	o := &SpawnOrchestrator{
		ctrl:             ctrl,
		logger:           slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
		defaultSubstrate: DefaultSubstrate,
		reattachExecFn:   markerExec(supervisorMarkerPrefix+"outcome 0", &execCalls),
	}

	o.runSpawnReattach(id, req)
	got, _ := ctrl.Get(id)
	if got.Status != spawn.StatusCompleted {
		t.Fatalf("after reattach status = %s, want completed", got.Status)
	}

	// A second recovery pass must be a terminal-frozen no-op — the attach exec
	// must not run again, and the outcome must not be re-delivered.
	o.runSpawnReattach(id, req)
	if execCalls != 1 {
		t.Fatalf("attach exec ran %d times, want exactly 1 (freeze invariant)", execCalls)
	}
	got, _ = ctrl.Get(id)
	if got.Status != spawn.StatusCompleted {
		t.Fatalf("second reattach mutated terminal spawn: status=%s", got.Status)
	}
}

// A nonzero recorded outcome fails the spawn honestly after reattach.
func TestRunSpawnReattach_FailsOnNonzeroOutcome(t *testing.T) {
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
	id, req := seedSupervisedRunningSpawn(t, ctrl, true)

	o := &SpawnOrchestrator{
		ctrl:             ctrl,
		logger:           slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
		defaultSubstrate: DefaultSubstrate,
		reattachExecFn:   markerExec(supervisorMarkerPrefix+"outcome 5", nil),
	}
	o.runSpawnReattach(id, req)
	got, _ := ctrl.Get(id)
	if got.Status != spawn.StatusFailed || got.Error == "" {
		t.Fatalf("after nonzero reattach status=%s error=%q, want failed+error", got.Status, got.Error)
	}
}

// A genuinely recorded agent outcome equal to the legacy launch-mode orphan
// sentinel (231) must be COLLECTED as an outcome — delivered exactly once as a
// terminal failure carrying exit 231 — and must NEVER be misrouted to the
// orphan path (which would re-drive a COMPLETED turn and risk double side
// effects). This is the disjoint-channel guarantee of the out-of-band marker
// protocol; 232 gets the same treatment.
func TestRunSpawnReattach_SentinelValuedOutcomeCollects(t *testing.T) {
	for _, code := range []int{supervisorReaperOrphanExit, supervisorMalformedOutcomeExit} {
		t.Run(fmt.Sprintf("outcome=%d", code), func(t *testing.T) {
			store, err := spawn.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
			id, req := seedSupervisedRunningSpawn(t, ctrl, true)

			var redriven []string
			o := &SpawnOrchestrator{
				ctrl:             ctrl,
				logger:           slog.Default(),
				backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
				defaultSubstrate: DefaultSubstrate,
				redriveSpawn:     func(id string, _ SpawnRequest) { redriven = append(redriven, id) },
				reattachExecFn:   markerExec(fmt.Sprintf("%soutcome %d", supervisorMarkerPrefix, code), nil),
			}
			o.runSpawnReattach(id, req)

			if len(redriven) != 0 {
				t.Fatalf("sentinel-valued outcome %d was re-driven %v — completed turn replayed", code, redriven)
			}
			got, _ := ctrl.Get(id)
			if got.Status != spawn.StatusFailed {
				t.Fatalf("outcome %d status = %s, want failed (collected nonzero outcome)", code, got.Status)
			}
			if !strings.Contains(got.Error, fmt.Sprintf("exit %d", code)) {
				t.Fatalf("outcome %d error = %q, want it to carry the true exit code", code, got.Error)
			}
		})
	}
}

// An orphaned reaper (attach launcher reports the out-of-band orphan marker)
// falls back to re-drive for liveness and must NOT terminalize the spawn.
func TestRunSpawnReattach_OrphanRedrives(t *testing.T) {
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
	id, req := seedSupervisedRunningSpawn(t, ctrl, true)

	var redriven []string
	o := &SpawnOrchestrator{
		ctrl:             ctrl,
		logger:           slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
		defaultSubstrate: DefaultSubstrate,
		redriveSpawn:     func(id string, _ SpawnRequest) { redriven = append(redriven, id) },
		reattachExecFn:   markerExec(supervisorMarkerPrefix+"orphan", nil),
	}
	o.runSpawnReattach(id, req)
	if len(redriven) != 1 || redriven[0] != id {
		t.Fatalf("orphan reattach redriven = %v, want [%s]", redriven, id)
	}
	got, _ := ctrl.Get(id)
	if got.Status != spawn.StatusRunning {
		t.Fatalf("orphan reattach terminalized spawn: status=%s", got.Status)
	}
}

// resolveReattachFromDurableState is the recovery decision when the attach
// stream could not deliver a marker (exec error, or marker-less stream end
// such as a launcher exec timeout). All branches must hold:
//   - durable outcome present → collect it (exactly once, correct terminal);
//   - reaper alive            → leave the spawn RUNNING (never terminalize a
//     live turn), no re-drive;
//   - supervisor gone / probe error → re-drive for liveness, not terminalized.
func TestRunSpawnReattach_ResolvesFromDurableStateWhenMarkerUndeliverable(t *testing.T) {
	streamOutcomes := []struct {
		name string
		exec func(*spawnDriverOwner, streamExecCapable, string, string, int, func([]byte)) (*backend.ExecResult, error)
	}{
		{"exec error", func(_ *spawnDriverOwner, _ streamExecCapable, _, _ string, _ int, _ func([]byte)) (*backend.ExecResult, error) {
			return nil, errors.New("transport severed mid-attach")
		}},
		{"marker-less stream end", func(_ *spawnDriverOwner, _ streamExecCapable, _, _ string, _ int, _ func([]byte)) (*backend.ExecResult, error) {
			// StreamExec's synthesized timeout shape: exit 124, no marker line.
			return &backend.ExecResult{ExitCode: 124}, nil
		}},
	}
	probes := []struct {
		name         string
		probe        supervisorProbe
		probeErr     error
		wantStatus   spawn.Status
		wantRedrive  bool
		wantErrorSub string
	}{
		{"outcome success -> collect completed", supervisorProbe{Found: true, OutcomePresent: true, OutcomeExit: 0}, nil, spawn.StatusCompleted, false, ""},
		{"outcome nonzero -> collect failed", supervisorProbe{Found: true, OutcomePresent: true, OutcomeExit: 9}, nil, spawn.StatusFailed, false, "exit 9"},
		{"reaper alive -> left recoverable", supervisorProbe{Found: true, ReaperAlive: true}, nil, spawn.StatusRunning, false, ""},
		{"supervisor gone -> redrive", supervisorProbe{}, nil, spawn.StatusRunning, true, ""},
		{"probe error -> redrive", supervisorProbe{}, errors.New("exec refused"), spawn.StatusRunning, true, ""},
	}
	for _, so := range streamOutcomes {
		for _, tc := range probes {
			t.Run(so.name+"/"+tc.name, func(t *testing.T) {
				store, err := spawn.NewFileStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
				id, req := seedSupervisedRunningSpawn(t, ctrl, true)

				var redriven []string
				o := &SpawnOrchestrator{
					ctrl:             ctrl,
					logger:           slog.Default(),
					backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
					defaultSubstrate: DefaultSubstrate,
					redriveSpawn:     func(id string, _ SpawnRequest) { redriven = append(redriven, id) },
					reattachExecFn:   so.exec,
					probeSupervisorFn: func(_ context.Context, _, _ string) (supervisorProbe, error) {
						return tc.probe, tc.probeErr
					},
				}
				o.runSpawnReattach(id, req)

				got, _ := ctrl.Get(id)
				if got.Status != tc.wantStatus {
					t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
				}
				if tc.wantErrorSub != "" && !strings.Contains(got.Error, tc.wantErrorSub) {
					t.Fatalf("error = %q, want substring %q", got.Error, tc.wantErrorSub)
				}
				if tc.wantRedrive != (len(redriven) == 1 && redriven[0] == id) {
					t.Fatalf("redriven = %v, wantRedrive = %v", redriven, tc.wantRedrive)
				}
				if tc.probe.ReaperAlive && !tc.probe.OutcomePresent {
					// The live-turn branch must ALSO release the driver so the
					// next recovery pass can re-acquire it.
					if _, acquired := o.acquireSpawnDriver(id); !acquired {
						t.Fatal("live-turn branch left the spawn driver held; next recovery pass would be locked out")
					}
				}
			})
		}
	}
}

// The !1025 terminal-freeze fence: a reattach against an already-terminal spawn
// must be a total no-op — the attach exec must never run.
func TestRunSpawnReattach_FreezeInvariant(t *testing.T) {
	store, err := spawn.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := spawn.NewK8sController(nil, "", store, slog.Default())
	ctx := context.Background()
	req := spawn.Request{
		AgentType: "claude-code", TaskDescription: "t",
		Project: "services/loom-core", IdempotencyKey: "wf-frozen", TimeoutMinutes: 30,
	}
	id, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := ctrl.Get(id)
	st.Status = spawn.StatusCompleted
	st.Supervised = true
	ctrl.UpdateState(ctx, st)

	execRan := false
	o := &SpawnOrchestrator{
		ctrl:             ctrl,
		logger:           slog.Default(),
		backends:         map[string]backend.Backend{DefaultSubstrate: &fakeStreamBackend{}},
		defaultSubstrate: DefaultSubstrate,
		reattachExecFn: func(_ *spawnDriverOwner, _ streamExecCapable, _, _ string, _ int, _ func([]byte)) (*backend.ExecResult, error) {
			execRan = true
			return &backend.ExecResult{ExitCode: 0}, nil
		},
	}
	o.runSpawnReattach(id, req)
	if execRan {
		t.Fatal("reattach ran the attach exec against a terminal spawn (freeze violated)")
	}
	got, _ := ctrl.Get(id)
	if got.Status != spawn.StatusCompleted {
		t.Fatalf("terminal spawn mutated by reattach: status=%s", got.Status)
	}
}
