package hud

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The S1c killtest identifies the completion wrapper by two exact substrings in
// its argv (pkg/mills/workflow/killtest/hold.go): `loom_agent_exit=$?;` and
// `; sleep <N>; loom_hold_exit=$?;`. Those must live ONLY in the injected
// hold.sh (read at runtime), never in the launcher or reaper argv — otherwise a
// second process would match the wrapper pattern and fail the gate
// ("found N wrappers"). This test is the guard on that invariant.
func TestSupervisorScriptsNeverCarryWrapperArgvSubstrings(t *testing.T) {
	forbidden := []string{"loom_agent_exit", "loom_hold_exit"}
	scripts := map[string]string{
		"launch":     supervisorLaunchCommand("/opt/loom/supervisor/spawn-abc", supervisorModeLaunch),
		"attach":     supervisorLaunchCommand("/opt/loom/supervisor/spawn-abc", supervisorModeAttach),
		"reaper(0)":  reaperScript("/opt/loom/supervisor/spawn-abc", 0),
		"reaper(60)": reaperScript("/opt/loom/supervisor/spawn-abc", 3600),
		"probe":      supervisorProbeScript("/opt/loom/supervisor/spawn-abc"),
	}
	for name, script := range scripts {
		for _, sub := range forbidden {
			if strings.Contains(script, sub) {
				t.Errorf("%s script must not contain killtest wrapper substring %q:\n%s", name, sub, script)
			}
		}
		// The hold's `sleep 90` must also never appear literally in these
		// scripts — only in hold.sh — so the killtest sees exactly one hold.
		if strings.Contains(script, "sleep 90") {
			t.Errorf("%s script must not contain a literal `sleep 90` hold: %s", name, script)
		}
	}
}

// The launch launcher must detach the reaper (setsid), off the exec stdio, and
// only launch it once (mkdir lock). These structural properties are what let
// the process pair survive a controller crash.
func TestSupervisorLaunchScriptDetachesReaper(t *testing.T) {
	launch := supervisorLaunchCommand("/opt/loom/supervisor/spawn-abc", supervisorModeLaunch)
	for _, want := range []string{
		"setsid sh ",                 // reaper is detached into its own session
		"reaper.sh",                  // launches the reaper file, not an inline string
		">/dev/null 2>&1 </dev/null", // reaper stdio detached from the exec stream
		"mkdir \"$D/launch.lock\"",   // idempotent, race-free single launch
		"reaper.pid",
	} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch script missing %q:\n%s", want, launch)
		}
	}
	// Attach mode must NEVER start a reaper.
	attach := supervisorLaunchCommand("/opt/loom/supervisor/spawn-abc", supervisorModeAttach)
	if strings.Contains(attach, "setsid") || strings.Contains(attach, "launch.lock") {
		t.Errorf("attach script must not launch a reaper:\n%s", attach)
	}
	// The reaper must linger (never `exit`) so it cannot orphan a zombie under
	// the pod's non-reaping `sleep infinity` PID 1.
	reaper := reaperScript("/opt/loom/supervisor/spawn-abc", 0)
	if !strings.Contains(reaper, "exec sleep infinity") {
		t.Errorf("reaper must linger via `exec sleep infinity`:\n%s", reaper)
	}
}

func TestParseSupervisorProbe(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    supervisorProbe
		wantErr bool
	}{
		{"alive no outcome", "PROBE\tpresent\talive\tnone\n", supervisorProbe{Found: true, ReaperAlive: true}, false},
		{"dead with outcome", "PROBE\tpresent\tdead\t0\n", supervisorProbe{Found: true, OutcomePresent: true, OutcomeExit: 0}, false},
		{"alive with nonzero outcome", "PROBE\tpresent\talive\t7\n", supervisorProbe{Found: true, ReaperAlive: true, OutcomePresent: true, OutcomeExit: 7}, false},
		{"absent", "PROBE\tabsent\tdead\tnone\n", supervisorProbe{}, false},
		{"malformed outcome word", "PROBE\tpresent\talive\tmalformed\n", supervisorProbe{Found: true, ReaperAlive: true}, false},
		{"leading noise then probe", "warning: something\nPROBE\tpresent\talive\tnone\n", supervisorProbe{Found: true, ReaperAlive: true}, false},
		{"no probe line", "garbage output\n", supervisorProbe{}, true},
		{"wrong field count", "PROBE\tpresent\talive\n", supervisorProbe{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSupervisorProbe(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseSupervisorProbe = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifySupervisorProbe(t *testing.T) {
	cases := []struct {
		name  string
		probe supervisorProbe
		want  supervisedRecoveryAction
	}{
		{"reaper alive, no outcome -> reattach", supervisorProbe{Found: true, ReaperAlive: true}, supervisedReattach},
		{"outcome present -> collect", supervisorProbe{Found: true, OutcomePresent: true, OutcomeExit: 0}, supervisedCollect},
		{"outcome wins over lingering reaper -> collect", supervisorProbe{Found: true, ReaperAlive: true, OutcomePresent: true, OutcomeExit: 3}, supervisedCollect},
		{"dead, no outcome -> redrive", supervisorProbe{Found: true}, supervisedRedrive},
		{"absent -> redrive", supervisorProbe{}, supervisedRedrive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySupervisorProbe(tc.probe); got != tc.want {
				t.Fatalf("classifySupervisorProbe(%+v) = %v, want %v", tc.probe, got, tc.want)
			}
		})
	}
}

// runLauncher runs a launcher script under /bin/sh with an optional extra PATH
// dir (for fake setsid/sleep bins) and returns its exit code + combined output.
func runLauncher(t *testing.T, script, extraPathDir string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	if extraPathDir != "" {
		cmd.Env = append(os.Environ(), "PATH="+extraPathDir+":"+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run launcher: %v\n%s", err, out)
	return -1, string(out)
}

// The attach launcher must COLLECT an already-recorded outcome immediately,
// reporting it via the out-of-band stdout marker and exiting 0 — the "finished
// while the controller was down" case. Includes outcome=231: a real recorded
// agent exit equal to the legacy launch-mode orphan sentinel MUST be delivered
// as an outcome (the sentinel and outcome channels are disjoint by design).
func TestSupervisorAttachCollectsRecordedOutcome(t *testing.T) {
	for _, code := range []string{"0", "7", "231", "232"} {
		t.Run("outcome="+code, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "outcome"), []byte(code+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			exit, out := runLauncher(t, supervisorLaunchCommand(dir, supervisorModeAttach), "")
			if exit != 0 {
				t.Fatalf("attach collect exit = %d, want 0 (marker carries the outcome)\n%s", exit, out)
			}
			wantMarker := supervisorMarkerPrefix + "outcome " + code
			if !strings.Contains(out, wantMarker) {
				t.Fatalf("attach output missing marker %q:\n%s", wantMarker, out)
			}
			kind, gotCode, ok := parseSupervisorMarkerLine(wantMarker)
			if !ok || kind != supervisorMarkerOutcome || itoa(gotCode) != code {
				t.Fatalf("marker round-trip = kind %v code %d ok %v, want outcome %s", kind, gotCode, ok, code)
			}
		})
	}
}

// The attach launcher must report the out-of-band orphan marker (exit 0) when
// the reaper is dead and no outcome was recorded (died mid-flight) so recovery
// re-drives — never an exit-code sentinel that could collide with an outcome.
func TestSupervisorAttachOrphanWhenReaperDeadNoOutcome(t *testing.T) {
	dir := t.TempDir()
	// A PID that is guaranteed not to exist so `kill -0` reports dead.
	if err := os.WriteFile(filepath.Join(dir, "reaper.pid"), []byte("2147480000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runLauncher(t, supervisorLaunchCommand(dir, supervisorModeAttach), "")
	if exit != 0 {
		t.Fatalf("attach orphan exit = %d, want 0 (marker carries the status)\n%s", exit, out)
	}
	if !strings.Contains(out, supervisorMarkerPrefix+"orphan") {
		t.Fatalf("attach output missing orphan marker:\n%s", out)
	}
}

// A recorded-but-malformed outcome must fail closed via the malformed marker.
func TestSupervisorAttachMalformedOutcome(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "outcome"), []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exit, out := runLauncher(t, supervisorLaunchCommand(dir, supervisorModeAttach), "")
	if exit != 0 {
		t.Fatalf("attach malformed exit = %d, want 0 (marker carries the status)\n%s", exit, out)
	}
	if !strings.Contains(out, supervisorMarkerPrefix+"malformed") {
		t.Fatalf("attach output missing malformed marker:\n%s", out)
	}
}

func TestParseSupervisorMarkerLine(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantKind supervisorMarkerKind
		wantCode int
		wantOK   bool
	}{
		{"outcome zero", supervisorMarkerPrefix + "outcome 0", supervisorMarkerOutcome, 0, true},
		{"outcome sentinel-valued 231", supervisorMarkerPrefix + "outcome 231", supervisorMarkerOutcome, 231, true},
		{"orphan", supervisorMarkerPrefix + "orphan", supervisorMarkerOrphan, 0, true},
		{"malformed", supervisorMarkerPrefix + "malformed", supervisorMarkerMalformed, 0, true},
		{"non-canonical outcome fails closed", supervisorMarkerPrefix + "outcome 007", supervisorMarkerMalformed, 0, true},
		{"negative outcome fails closed", supervisorMarkerPrefix + "outcome -1", supervisorMarkerMalformed, 0, true},
		{"agent output not a marker", `{"type":"item.completed"}`, supervisorMarkerNone, 0, false},
		{"reaper diagnostic not a marker", "LOOM-SUPERVISOR-DIAG outcome write failed", supervisorMarkerNone, 0, false},
		{"unknown marker body forwarded", supervisorMarkerPrefix + "something-else", supervisorMarkerNone, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, code, ok := parseSupervisorMarkerLine(tc.line)
			if kind != tc.wantKind || code != tc.wantCode || ok != tc.wantOK {
				t.Fatalf("parseSupervisorMarkerLine(%q) = (%v, %d, %v), want (%v, %d, %v)",
					tc.line, kind, code, ok, tc.wantKind, tc.wantCode, tc.wantOK)
			}
		})
	}
}

// End-to-end: the launch launcher starts the detached reaper, which runs the
// injected hold.sh, records its exit code durably, and the launcher returns it.
// Fake `setsid` (exec passthrough) and `sleep` (no-op) keep the reaper's
// detach + linger observable on the host without leaving a real `sleep infinity`.
// The timeoutSec>0 variants also exercise the `timeout`-wrapped reaper branch
// via a fake `timeout` that drops the `-k 10 <sec>` prefix and execs the rest.
func TestSupervisorLaunchRunsReaperAndRecordsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holdBody   string
		timeoutSec int
		wantExit   int
	}{
		{"success", "printf 'agentline\\n'", 0, 0},
		{"agent failure", "printf 'agentline\\n'; exit 5", 0, 5},
		{"success under timeout wrapper", "printf 'agentline\\n'", 1800, 0},
		{"agent failure under timeout wrapper", "printf 'agentline\\n'; exit 6", 1800, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := t.TempDir()
			writeFakeBin(t, binDir, "setsid", "#!/bin/sh\nexec \"$@\"\n")
			writeFakeBin(t, binDir, "sleep", "#!/bin/sh\nexit 0\n")
			if tc.timeoutSec > 0 {
				// Args arrive as: -k 10 <sec> sh -c <script>. Drop the first
				// three and exec the command so the reaper's timeout branch is
				// the code path under test.
				writeFakeBin(t, binDir, "timeout", "#!/bin/sh\nshift 3\nexec \"$@\"\n")
			}

			// Simulate injectSupervisorAssets: write hold.sh + reaper.sh.
			if err := os.WriteFile(filepath.Join(dir, "hold.sh"), []byte(tc.holdBody+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "reaper.sh"), []byte(reaperScript(dir, tc.timeoutSec)), 0o755); err != nil {
				t.Fatal(err)
			}

			got, out := runLauncher(t, supervisorLaunchCommand(dir, supervisorModeLaunch), binDir)
			if got != tc.wantExit {
				t.Fatalf("launch exit = %d, want %d\n%s", got, tc.wantExit, out)
			}
			outcome, err := os.ReadFile(filepath.Join(dir, "outcome"))
			if err != nil {
				t.Fatalf("read outcome: %v", err)
			}
			if strings.TrimSpace(string(outcome)) != itoa(tc.wantExit) {
				t.Fatalf("recorded outcome = %q, want %d", outcome, tc.wantExit)
			}
			log, err := os.ReadFile(filepath.Join(dir, "agent.log"))
			if err != nil {
				t.Fatalf("read agent.log: %v", err)
			}
			if !strings.Contains(string(log), "agentline") {
				t.Fatalf("agent.log missing wrapper output: %q", log)
			}
		})
	}
}

// TestSupervisorLaunchFailsFastWhenStateDirUncreatable pins the guard for the
// 2026-07-19 /opt/loom EACCES wedge: when the launcher cannot mkdir the state
// dir it must exit supervisorStateDirExit immediately (launch) or emit the
// orphan marker (attach) — never fall through into the outcome wait loop,
// which would spin silently for the full exec deadline.
func TestSupervisorLaunchFailsFastWhenStateDirUncreatable(t *testing.T) {
	// Block mkdir with a regular file on the path (ENOTDIR) rather than a
	// read-only parent (EACCES): CI runs the suite as root, for whom mode
	// bits do not block mkdir — the EACCES variant silently succeeded there
	// and the launcher hung in the outcome wait loop until the test timeout.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(blocker, "spawn-x")

	got, out := runLauncher(t, supervisorLaunchCommand(stateDir, supervisorModeLaunch), "")
	if got != supervisorStateDirExit {
		t.Fatalf("launch exit = %d, want %d\n%s", got, supervisorStateDirExit, out)
	}
	if !strings.Contains(out, "cannot create state dir") {
		t.Fatalf("launch output missing diagnostic: %q", out)
	}

	got, out = runLauncher(t, supervisorLaunchCommand(stateDir, supervisorModeAttach), "")
	if got != 0 {
		t.Fatalf("attach exit = %d, want 0\n%s", got, out)
	}
	if !strings.Contains(out, supervisorMarkerPrefix+"orphan") {
		t.Fatalf("attach output missing orphan marker: %q", out)
	}
}

func writeFakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
