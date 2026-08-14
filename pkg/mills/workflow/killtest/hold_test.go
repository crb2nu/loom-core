package killtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow"
)

type procFixture struct {
	pid   int
	ppid  int
	state string
	start uint64
	argv  []string
	// statusPPID, when non-zero, makes /status report a different parent
	// than /stat. A real probe cannot read both files atomically, so this
	// models a process reparented by the kernel part-way through one
	// snapshot — what CRASH B does to the canary's process tree.
	statusPPID int
}

func writeProcFixture(t *testing.T, root string, fixture procFixture) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(fixture.pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir proc fixture: %v", err)
	}
	statusPPID := fixture.statusPPID
	if statusPPID == 0 {
		statusPPID = fixture.ppid
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(fmt.Sprintf(
		"Name:\tfixture\nState:\t%s (fixture)\nPPid:\t%d\n", fixture.state, statusPPID)), 0o644); err != nil {
		t.Fatalf("write proc status: %v", err)
	}
	start := fixture.start
	if start == 0 {
		start = uint64(fixture.pid) * 100
	}
	statFields := []string{
		strconv.Itoa(fixture.pid), "(fixture process)", fixture.state, strconv.Itoa(fixture.ppid),
		"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "1", "0",
		strconv.FormatUint(start, 10),
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(strings.Join(statFields, " ")+"\n"), 0o644); err != nil {
		t.Fatalf("write proc stat: %v", err)
	}
	var cmdline strings.Builder
	for _, arg := range fixture.argv {
		cmdline.WriteString(arg)
		cmdline.WriteByte(0)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline.String()), 0o644); err != nil {
		t.Fatalf("write proc cmdline: %v", err)
	}
}

func runCanaryHoldProbeScript(t *testing.T, fixtures ...procFixture) string {
	t.Helper()
	root := t.TempDir()
	for _, fixture := range fixtures {
		writeProcFixture(t, root, fixture)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", canaryHoldProbeScript,
		"mills-canary-hold-probe", strconv.Itoa(workflow.CanaryHoldSeconds), root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe script failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func completionWrapperArgv(duration string) []string {
	return []string{
		"sh",
		"-c",
		"cd /workspace && agent; loom_agent_exit=$?; " +
			"if [ \"$loom_agent_exit\" -ne 0 ]; then exit \"$loom_agent_exit\"; fi; " +
			"sleep " + duration + "; loom_hold_exit=$?; " +
			"if [ \"$loom_hold_exit\" -ne 0 ]; then exit \"$loom_hold_exit\"; fi; " +
			"exit \"$loom_agent_exit\"",
	}
}

func runCanaryProcessProbeScript(
	t *testing.T,
	expectedHoldPID int,
	expectedHoldStart uint64,
	expectedDriverPID int,
	expectedDriverStart uint64,
	fixtures ...procFixture,
) string {
	t.Helper()
	return runCanaryProcessProbeScriptSkipping(t,
		expectedHoldPID, expectedHoldStart, expectedDriverPID, expectedDriverStart,
		"", fixtures...)
}

// runCanaryProcessProbeScriptSkipping drives the probe with the test-only
// enumeration skip list, which deterministically reproduces the transient
// inventory miss that the post-loop recheck heals.
func runCanaryProcessProbeScriptSkipping(
	t *testing.T,
	expectedHoldPID int,
	expectedHoldStart uint64,
	expectedDriverPID int,
	expectedDriverStart uint64,
	skipPIDs string,
	fixtures ...procFixture,
) string {
	t.Helper()
	root := t.TempDir()
	for _, fixture := range fixtures {
		writeProcFixture(t, root, fixture)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", canaryProcessProbeScript,
		"mills-canary-process-probe",
		strconv.Itoa(workflow.CanaryHoldSeconds),
		strconv.Itoa(expectedHoldPID),
		strconv.FormatUint(expectedHoldStart, 10),
		strconv.Itoa(expectedDriverPID),
		strconv.FormatUint(expectedDriverStart, 10),
		root,
		skipPIDs,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("process probe script failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCanaryHoldProbeScriptMatchesOnlyExactLiveProcess(t *testing.T) {
	t.Parallel()
	duration := strconv.Itoa(workflow.CanaryHoldSeconds)
	tests := []struct {
		name     string
		fixtures []procFixture
		want     string
	}{
		{name: "none", want: "NOT_READY"},
		{
			name: "exact argv with completion wrapper parent",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", duration}},
			},
			want: "READY\t42\t4200\t" + duration + "\t41\t4100",
		},
		{
			name: "argv zero basename",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 43, ppid: 41, state: "S", argv: []string{"/usr/bin/sleep", duration}},
			},
			want: "READY\t43\t4300\t" + duration + "\t41\t4100",
		},
		{
			name: "pid one infinity excluded",
			fixtures: []procFixture{
				{pid: 1, ppid: 0, state: "S", argv: []string{"sleep", "infinity"}},
				{pid: 43, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 44, ppid: 43, state: "S", argv: []string{"sleep", duration}},
			},
			want: "READY\t44\t4400\t" + duration + "\t43\t4300",
		},
		{
			name: "probe shell substring excluded",
			fixtures: []procFixture{{
				pid: 45, state: "S",
				argv: []string{"bash", "-c", "scan /proc and look for sleep " + duration},
			}},
			want: "NOT_READY",
		},
		{
			name:     "wrong duration excluded",
			fixtures: []procFixture{{pid: 46, state: "S", argv: []string{"sleep", "91"}}},
			want:     "NOT_READY",
		},
		{
			name:     "extra argument excluded",
			fixtures: []procFixture{{pid: 47, state: "S", argv: []string{"sleep", duration, "extra"}}},
			want:     "NOT_READY",
		},
		{
			name:     "empty extra argument excluded",
			fixtures: []procFixture{{pid: 48, state: "S", argv: []string{"sleep", duration, ""}}},
			want:     "NOT_READY",
		},
		{
			name:     "zombie excluded",
			fixtures: []procFixture{{pid: 49, state: "Z", argv: []string{"sleep", duration}}},
			want:     "NOT_READY",
		},
		{
			name:     "sleep substring executable excluded",
			fixtures: []procFixture{{pid: 50, state: "S", argv: []string{"/usr/bin/not-sleep", duration}}},
			want:     "NOT_READY",
		},
		{
			name: "multiple exact processes are explicit",
			fixtures: []procFixture{
				{pid: 51, ppid: 1, state: "S", argv: []string{"sleep", duration}},
				{pid: 52, ppid: 1, state: "D", argv: []string{"/bin/sleep", duration}},
			},
			want: "AMBIGUOUS\t2",
		},
		{
			name: "hold without completion wrapper parent fails closed",
			fixtures: []procFixture{
				{pid: 53, ppid: 52, state: "S", argv: []string{"sleep", duration}},
			},
			want: "DRIVER_MISMATCH\t0\t52",
		},
		{
			name: "completion wrapper must be the hold parent",
			fixtures: []procFixture{
				{pid: 54, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 55, ppid: 53, state: "S", argv: []string{"sleep", duration}},
			},
			want: "DRIVER_MISMATCH\t1\t53",
		},
		{
			name: "multiple completion wrappers fail closed",
			fixtures: []procFixture{
				{pid: 56, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 57, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 58, ppid: 56, state: "S", argv: []string{"sleep", duration}},
			},
			want: "DRIVER_MISMATCH\t2\t56",
		},
		{
			name: "zombie completion wrapper fails closed",
			fixtures: []procFixture{
				{pid: 59, ppid: 1, state: "Z", argv: completionWrapperArgv(duration)},
				{pid: 60, ppid: 59, state: "S", argv: []string{"sleep", duration}},
			},
			want: "DRIVER_MISMATCH\t0\t59",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runCanaryHoldProbeScript(t, tt.fixtures...); got != tt.want {
				t.Fatalf("probe output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanaryProcessProbeScriptPreservesKnownStatesAndInventories(t *testing.T) {
	t.Parallel()
	duration := strconv.Itoa(workflow.CanaryHoldSeconds)
	tests := []struct {
		name     string
		fixtures []procFixture
		want     string
	}{
		{
			name: "original process pair is live",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			// The S1c v3 gate died here: CRASH B kills the driver, the
			// kernel reparents the hold to init mid-probe, and the old
			// snapshot rejected the PPID change — so the hold read as
			// state "S" but was missing from the live inventory, which
			// Evaluate correctly reported as an inconsistency. Orphaning
			// is expected during the crash window and must be inventoried.
			name: "hold orphaned to init mid-snapshot stays inventoried",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, statusPPID: 1, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			// Only reparenting TO init is explainable. A parent that
			// changes to another live PID means the two reads landed on
			// different processes, and must still fail closed.
			name: "hold reparented to an arbitrary pid fails closed",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, statusPPID: 77, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t0\tINVALID\t41\t4100\tS\t-\t41\t-",
		},
		{
			name: "known zombies survive empty cmdlines and replacements are inventoried",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "Z"},
				{pid: 42, ppid: 1, state: "Z"},
				{pid: 43, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 44, ppid: 43, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t4200\tZ\t41\t4100\tZ\t44\t43\t41,42",
		},
		{
			name: "different pid zombie is never hidden by empty cmdline",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", duration}},
				{pid: 88, ppid: 1, state: "Z"},
			},
			want: "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t88",
		},
		{
			name: "same pid with a different starttime remains visible",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", start: 4100, argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, state: "S", start: 9999, argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t9999\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			name: "missing originals remain explicit",
			fixtures: []procFixture{
				{pid: 43, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 44, ppid: 43, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t0\tMISSING\t41\t0\tMISSING\t44\t43\t-",
		},
		{
			name: "substrings and wrong duration are excluded",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: []string{"sh", "-c", "echo loom_agent_exit=$? sleep " + duration}},
				{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", "91"}},
			},
			want: "SAMPLE\t42\t4200\tS\t41\t4100\tS\t-\t-\t-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runCanaryProcessProbeScript(t, 42, 4200, 41, 4100, tt.fixtures...); got != tt.want {
				t.Fatalf("process probe output = %q, want %q", got, tt.want)
			}
		})
	}
}

// The S1c v5 gate died here: ~78s after CRASH B a reaper exit reparented the
// live driver mid-enumeration, the multi-read snapshot straddled the
// transition, and the driver vanished from the live inventory for exactly one
// sample while the known-identity read still bound the same (PID,starttime)
// alive in state S (run 1, sample 313: state="S" live=[]). The post-loop
// recheck must heal exactly that — an absent-but-identity-bound original —
// by re-running the FULL unmodified match contract, and must never admit a
// zombie, argv-mismatched, or identity-drifted process the loop excluded.
func TestCanaryProcessProbeScriptRecheckHealsEnumerationMiss(t *testing.T) {
	t.Parallel()
	duration := strconv.Itoa(workflow.CanaryHoldSeconds)
	livePair := []procFixture{
		{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
		{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", duration}},
	}
	tests := []struct {
		name     string
		skip     string
		fixtures []procFixture
		want     string
	}{
		{
			name:     "skipped live driver is restored by the recheck",
			skip:     "41",
			fixtures: livePair,
			want:     "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			name:     "skipped live hold is restored by the recheck",
			skip:     "42",
			fixtures: livePair,
			want:     "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			name:     "skipped live pair is restored by the recheck",
			skip:     "41,42",
			fixtures: livePair,
			want:     "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t41\t-",
		},
		{
			// The recheck runs the same argv contract as the loop; a
			// substring match the loop excludes stays excluded.
			name: "skipped argv-mismatched driver stays out of the inventory",
			skip: "41",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: []string{"sh", "-c", "echo loom_agent_exit=$? sleep " + duration}},
				{pid: 42, ppid: 41, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t4200\tS\t41\t4100\tS\t42\t-\t-",
		},
		{
			// A zombie original is identity-bound but not live; the
			// recheck's Z guards must refuse it.
			name: "skipped zombie driver stays out of the inventory",
			skip: "41",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "Z"},
				{pid: 43, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 44, ppid: 43, state: "S", argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t0\tMISSING\t41\t4100\tZ\t44\t43\t-",
		},
		{
			// Identity drift (same PID, different starttime) fails the
			// known-read guard, so the recheck never fires.
			name: "skipped identity-drifted hold stays out of the inventory",
			skip: "42",
			fixtures: []procFixture{
				{pid: 41, ppid: 1, state: "S", argv: completionWrapperArgv(duration)},
				{pid: 42, ppid: 41, state: "S", start: 9999, argv: []string{"sleep", duration}},
			},
			want: "SAMPLE\t42\t9999\tS\t41\t4100\tS\t-\t41\t-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runCanaryProcessProbeScriptSkipping(t, 42, 4200, 41, 4100, tt.skip, tt.fixtures...); got != tt.want {
				t.Fatalf("process probe output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProbeCanaryProcessesReturnsFailClosedStructuredSample(t *testing.T) {
	t.Parallel()
	h := New(Config{SpawnNS: "spawn-ns"})
	wantArgs := []string{
		"-n", "spawn-ns",
		"exec", "spawn-abc123",
		"-c", "devbox",
		"--", "bash", "-c", canaryProcessProbeScript,
		"mills-canary-process-probe",
		strconv.Itoa(workflow.CanaryHoldSeconds),
		"4321", "432100", "4320", "432000",
	}
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("kubectl args = %#v, want %#v", args, wantArgs)
		}
		return "SAMPLE\t4321\t432100\tZ\t4320\t432000\tS\t5001,5000\t4999\t4321,6000", nil
	}

	started := time.Now().UTC()
	sample, err := h.ProbeCanaryProcesses(context.Background(), "abc123", 4321, 432100, 4320, 432000)
	finished := time.Now().UTC()
	if err != nil {
		t.Fatalf("ProbeCanaryProcesses() error = %v", err)
	}
	want := CanaryProcessSample{
		PodName:              "spawn-abc123",
		HoldPID:              4321,
		HoldStartTimeTicks:   432100,
		HoldState:            "Z",
		DriverPID:            4320,
		DriverStartTimeTicks: 432000,
		DriverState:          "S",
		LiveHoldPIDs:         []int{5000, 5001},
		LiveDriverPIDs:       []int{4999},
		ZombiePIDs:           []int{4321, 6000},
	}
	observedAt := sample.ObservedAt
	sample.ObservedAt = time.Time{}
	if !reflect.DeepEqual(sample, want) {
		t.Fatalf("ProbeCanaryProcesses() sample = %+v, want %+v", sample, want)
	}
	if observedAt.Before(started) || observedAt.After(finished) {
		t.Fatalf("observed_at %s outside call window [%s, %s]", observedAt, started, finished)
	}
}

func TestProbeCanaryProcessesFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
	}{
		{name: "empty"},
		{name: "unknown marker", output: "READY\t12\t1200\tS\t11\t1100\tS\t12\t11\t-"},
		{name: "missing fields", output: "SAMPLE\t12\t1200\tS"},
		{name: "wrong hold pid", output: "SAMPLE\t13\t1200\tS\t11\t1100\tS\t12\t11\t-"},
		{name: "malformed hold starttime", output: "SAMPLE\t12\t+1200\tS\t11\t1100\tS\t12\t11\t-"},
		{name: "live hold with zero starttime", output: "SAMPLE\t12\t0\tS\t11\t1100\tS\t12\t11\t-"},
		{name: "missing hold with live starttime", output: "SAMPLE\t12\t1200\tMISSING\t11\t1100\tS\t-\t11\t-"},
		{name: "wrong driver pid", output: "SAMPLE\t12\t1200\tS\t10\t1100\tS\t12\t11\t-"},
		{name: "unknown hold state", output: "SAMPLE\t12\t1200\tQ\t11\t1100\tS\t12\t11\t-"},
		{name: "unknown driver state", output: "SAMPLE\t12\t1200\tS\t11\t1100\tQ\t12\t11\t-"},
		{name: "empty hold inventory", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t\t11\t-"},
		{name: "zero inventory pid", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t0\t11\t-"},
		{name: "noncanonical inventory pid", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t+12\t11\t-"},
		{name: "duplicate inventory pid", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t12,12\t11\t-"},
		{name: "malformed zombie inventory", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t12\t11\t+99"},
		{name: "multiple lines", output: "SAMPLE\t12\t1200\tS\t11\t1100\tS\t12\t11\t-\nSAMPLE\t12\t1200\tS\t11\t1100\tS\t12\t11\t-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := New(Config{})
			h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
				return tt.output, nil
			}
			if sample, err := h.ProbeCanaryProcesses(context.Background(), "abc", 12, 1200, 11, 1100); err == nil || !reflect.DeepEqual(sample, CanaryProcessSample{}) {
				t.Fatalf("ProbeCanaryProcesses() = (%+v, %v), want zero, error", sample, err)
			}
		})
	}

	t.Run("invalid identity does not exec", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			spawnID     string
			holdPID     int
			holdStart   uint64
			driverPID   int
			driverStart uint64
		}{
			{spawnID: " ", holdPID: 12, holdStart: 1200, driverPID: 11, driverStart: 1100},
			{spawnID: "abc", holdPID: 1, holdStart: 100, driverPID: 11, driverStart: 1100},
			{spawnID: "abc", holdPID: 12, driverPID: 11, driverStart: 1100},
			{spawnID: "abc", holdPID: 12, holdStart: 1200, driverPID: 1, driverStart: 100},
			{spawnID: "abc", holdPID: 12, holdStart: 1200, driverPID: 11},
			{spawnID: "abc", holdPID: 12, holdStart: 1200, driverPID: 12, driverStart: 1200},
		}
		for _, tt := range tests {
			h := New(Config{})
			h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
				t.Fatal("kubectl must not be called for invalid process identity")
				return "", nil
			}
			if _, err := h.ProbeCanaryProcesses(context.Background(), tt.spawnID, tt.holdPID, tt.holdStart, tt.driverPID, tt.driverStart); err == nil {
				t.Fatalf("ProbeCanaryProcesses(%q, %d/%d, %d/%d) error = nil",
					tt.spawnID, tt.holdPID, tt.holdStart, tt.driverPID, tt.driverStart)
			}
		}
	})

	t.Run("kubectl error", func(t *testing.T) {
		t.Parallel()
		h := New(Config{})
		h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
			return "forbidden", errors.New("RBAC denied")
		}
		if sample, err := h.ProbeCanaryProcesses(context.Background(), "abc", 12, 1200, 11, 1100); err == nil ||
			!reflect.DeepEqual(sample, CanaryProcessSample{}) || !strings.Contains(err.Error(), "RBAC denied") {
			t.Fatalf("ProbeCanaryProcesses() = (%+v, %v), want zero RBAC error", sample, err)
		}
	})
}

func TestProbeCanaryHoldUsesExactPodContainerAndPositionalDuration(t *testing.T) {
	t.Parallel()
	h := New(Config{SpawnNS: "spawn-ns"})
	wantArgs := []string{
		"-n", "spawn-ns",
		"exec", "spawn-abc123",
		"-c", "devbox",
		"--", "bash", "-c", canaryHoldProbeScript,
		"mills-canary-hold-probe", strconv.Itoa(workflow.CanaryHoldSeconds),
	}
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("kubectl args = %#v, want %#v", args, wantArgs)
		}
		return fmt.Sprintf("READY\t4321\t432100\t%d\t4320\t432000", workflow.CanaryHoldSeconds), nil
	}

	started := time.Now().UTC()
	observation, ready, err := h.ProbeCanaryHold(context.Background(), "abc123")
	finished := time.Now().UTC()
	if err != nil || !ready {
		t.Fatalf("ProbeCanaryHold() ready=%t error=%v", ready, err)
	}
	if observation.PodName != "spawn-abc123" || observation.PID != 4321 || observation.StartTimeTicks != 432100 ||
		observation.DriverPID != 4320 || observation.DriverStartTimeTicks != 432000 ||
		observation.Seconds != workflow.CanaryHoldSeconds || observation.ObservedAt.IsZero() {
		t.Fatalf("ProbeCanaryHold() observation = %+v", observation)
	}
	if observation.ObservedAt.Before(started) || observation.ObservedAt.After(finished) {
		t.Fatalf("observed_at %s outside call window [%s, %s]", observation.ObservedAt, started, finished)
	}
}

func TestProbeCanaryHoldNotReadyIsNormal(t *testing.T) {
	t.Parallel()
	h := New(Config{})
	h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
		return "NOT_READY\n", nil
	}
	observation, ready, err := h.ProbeCanaryHold(context.Background(), "abc")
	if err != nil || ready || observation != (CanaryHoldObservation{}) {
		t.Fatalf("ProbeCanaryHold() = (%+v, %t, %v), want zero, false, nil", observation, ready, err)
	}
}

func TestProbeCanaryHoldFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
	}{
		{name: "empty output"},
		{name: "unknown output", output: "something else"},
		{name: "ready missing fields", output: "READY"},
		{name: "ready extra fields", output: "READY\t12\t1200\t90\t11\t1100\textra"},
		{name: "not ready extra fields", output: "NOT_READY\t12"},
		{name: "nonnumeric pid", output: fmt.Sprintf("READY\tpid\t1200\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "noncanonical pid", output: fmt.Sprintf("READY\t+12\t1200\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "zero pid", output: fmt.Sprintf("READY\t0\t1200\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "pid one", output: fmt.Sprintf("READY\t1\t100\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "negative pid", output: fmt.Sprintf("READY\t-1\t100\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "zero hold starttime", output: fmt.Sprintf("READY\t12\t0\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "noncanonical hold starttime", output: fmt.Sprintf("READY\t12\t+1200\t%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "nonnumeric duration", output: "READY\t12\t1200\tninety\t11\t1100"},
		{name: "noncanonical duration", output: fmt.Sprintf("READY\t12\t1200\t+%d\t11\t1100", workflow.CanaryHoldSeconds)},
		{name: "wrong duration", output: fmt.Sprintf("READY\t12\t1200\t%d\t11\t1100", workflow.CanaryHoldSeconds+1)},
		{name: "nonnumeric driver pid", output: fmt.Sprintf("READY\t12\t1200\t%d\tdriver\t1100", workflow.CanaryHoldSeconds)},
		{name: "noncanonical driver pid", output: fmt.Sprintf("READY\t12\t1200\t%d\t+11\t1100", workflow.CanaryHoldSeconds)},
		{name: "driver pid one", output: fmt.Sprintf("READY\t12\t1200\t%d\t1\t100", workflow.CanaryHoldSeconds)},
		{name: "driver pid equals hold pid", output: fmt.Sprintf("READY\t12\t1200\t%d\t12\t1100", workflow.CanaryHoldSeconds)},
		{name: "zero driver starttime", output: fmt.Sprintf("READY\t12\t1200\t%d\t11\t0", workflow.CanaryHoldSeconds)},
		{name: "ambiguous one", output: "AMBIGUOUS\t1"},
		{name: "ambiguous noncanonical", output: "AMBIGUOUS\t+2"},
		{name: "ambiguous nonnumeric", output: "AMBIGUOUS\tmany"},
		{name: "driver mismatch missing field", output: "DRIVER_MISMATCH\t0"},
		{name: "driver mismatch noncanonical count", output: "DRIVER_MISMATCH\t+1\t11"},
		{name: "driver mismatch noncanonical parent", output: "DRIVER_MISMATCH\t1\t+11"},
		{name: "multiple output lines", output: fmt.Sprintf("READY\t12\t1200\t%d\t11\t1100\nNOT_READY", workflow.CanaryHoldSeconds)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := New(Config{})
			h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
				return tt.output, nil
			}
			observation, ready, err := h.ProbeCanaryHold(context.Background(), "abc")
			if err == nil || ready || observation != (CanaryHoldObservation{}) {
				t.Fatalf("ProbeCanaryHold() = (%+v, %t, %v), want zero, false, error", observation, ready, err)
			}
		})
	}

	t.Run("multiple exact pids", func(t *testing.T) {
		t.Parallel()
		h := New(Config{})
		h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
			return "AMBIGUOUS\t2", nil
		}
		_, ready, err := h.ProbeCanaryHold(context.Background(), "abc")
		if err == nil || ready || !strings.Contains(err.Error(), "2 exact") {
			t.Fatalf("ProbeCanaryHold() ready=%t error=%v, want explicit ambiguity error", ready, err)
		}
	})

	t.Run("kubectl exec or rbac error", func(t *testing.T) {
		t.Parallel()
		h := New(Config{})
		h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
			return "forbidden", errors.New("RBAC denied")
		}
		observation, ready, err := h.ProbeCanaryHold(context.Background(), "abc")
		if err == nil || ready || observation != (CanaryHoldObservation{}) ||
			!strings.Contains(err.Error(), "RBAC denied") {
			t.Fatalf("ProbeCanaryHold() = (%+v, %t, %v), want fail-closed RBAC error", observation, ready, err)
		}
	})

	t.Run("missing spawn id does not exec", func(t *testing.T) {
		t.Parallel()
		h := New(Config{})
		h.kubectlFn = func(_ context.Context, _ ...string) (string, error) {
			t.Fatal("kubectl must not be called for a missing spawn id")
			return "", nil
		}
		if _, ready, err := h.ProbeCanaryHold(context.Background(), " \t"); err == nil || ready {
			t.Fatalf("ProbeCanaryHold() ready=%t error=%v, want validation error", ready, err)
		}
	})
}
