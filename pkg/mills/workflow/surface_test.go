package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Interpreter replay-compatibility surface (S6-full CI deploy-safety gate).
//
// A journal written by one binary must replay on the next: the universe names,
// the canonical-args encoding, the step-key scheme, the idempotency-key
// derivation, and the interpreter engine itself are all load-bearing for that
// contract. This test renders each of them into one deterministic text blob and
// compares it byte-for-byte against testdata/interp_surface.golden. Changing
// any of them forces a golden update, and a golden diff trips
// scripts/ci/check_workflow_drain_gate.sh — which demands the in-flight
// imperative runs be drained (and the MR carry [workflow-drain-confirmed])
// before the change ships. Version-pin drift on a live run is a hard
// abort-and-escalate; the gate exists so it never fires in practice.
//
// Regenerate deliberately: UPDATE_INTERP_SURFACE=1 go test ./pkg/mills/workflow -run TestInterpreterSurface
// ---------------------------------------------------------------------------

const surfaceGoldenPath = "testdata/interp_surface.golden"

// surfaceProbeArgs is a fixed nested fixture exercising every canonicalizer
// branch: sorted map keys, nested list/map, null, bool, int and float scalars.
func surfaceProbeArgs() map[string]any {
	return map[string]any{
		"z": map[string]any{"k": 2},
		"a": []any{1, map[string]any{"c": nil, "b": "x"}, true, 2.5},
	}
}

// surfaceProbeScript touches all 7 universe builtins, a duplicate call (dup
// counter), nested parallel branches, and a loop frame, so the rendered step
// keys witness the whole key-derivation scheme.
const surfaceProbeScript = `
agent("a")
agent("a")
gate("g")
merge("m")
ctx_now()
ctx_uuid()

def b0(): agent("w")
def b1(): agent("w")
parallel([b0, b1], fan_out_width=2)

def drain(i):
    agent("d")
    return True

loop_until_dry(drain, max_iter=3)
`

func renderInterpreterSurface(t *testing.T) string {
	t.Helper()

	st := newTestStore(t)
	h := newHost(t, st, "surface-probe")

	var mu sync.Mutex
	var stepKeys []string
	h.SetEffectExec(func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error) {
		mu.Lock()
		stepKeys = append(stepKeys, stepKey)
		mu.Unlock()
		return EffectResult{Value: "v", CostSource: "real"}, nil
	})
	if err := h.Run(surfaceProbeScript); err != nil {
		t.Fatalf("surface probe script failed: %v", err)
	}
	// Parallel branches record concurrently; sort for a deterministic listing.
	sort.Strings(stepKeys)

	canon, err := canonicalJSON(surfaceProbeArgs())
	if err != nil {
		t.Fatalf("canonical json probe: %v", err)
	}

	var erFields []string
	et := reflect.TypeOf(EffectResult{})
	for i := 0; i < et.NumField(); i++ {
		f := et.Field(i)
		erFields = append(erFields, f.Name+":"+f.Type.String())
	}
	sort.Strings(erFields)

	var b strings.Builder
	b.WriteString("# Interpreter replay-compatibility surface. A diff in this file trips the\n")
	b.WriteString("# CI drain gate (scripts/ci/check_workflow_drain_gate.sh): drain in-flight\n")
	b.WriteString("# imperative workflow runs, then add [workflow-drain-confirmed] to the MR.\n")
	b.WriteString("# Regenerate: UPDATE_INTERP_SURFACE=1 go test ./pkg/mills/workflow -run TestInterpreterSurface\n")
	fmt.Fprintf(&b, "interpreter_version=%s\n", HostInterpreterVersion)
	fmt.Fprintf(&b, "universe=%s\n", strings.Join(h.UniverseNames(), " "))
	fmt.Fprintf(&b, "host_max_fanout=%d\n", HostMaxFanout)
	fmt.Fprintf(&b, "host_max_iter=%d\n", HostMaxIter)
	fmt.Fprintf(&b, "canonical_json=%s\n", canon)
	fmt.Fprintf(&b, "call_hash=%s\n", canonicalCallHash("agent", surfaceProbeArgs()))
	fmt.Fprintf(&b, "idempotency_key=%s\n", DeriveStepIdempotencyKey("run-fixed", "root/agent~0123456789abcdef#0", surfaceProbeArgs()))
	fmt.Fprintf(&b, "effect_result=%s\n", strings.Join(erFields, " "))
	b.WriteString("step_keys:\n")
	for _, k := range stepKeys {
		fmt.Fprintf(&b, "  %s\n", k)
	}
	return b.String()
}

// TestInterpreterSurfaceGolden pins the replay-compatibility surface.
func TestInterpreterSurfaceGolden(t *testing.T) {
	got := renderInterpreterSurface(t)

	if os.Getenv("UPDATE_INTERP_SURFACE") != "" {
		if err := os.MkdirAll(filepath.Dir(surfaceGoldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(surfaceGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", surfaceGoldenPath)
		return
	}

	want, err := os.ReadFile(surfaceGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_INTERP_SURFACE=1)", surfaceGoldenPath, err)
	}
	if string(want) != got {
		t.Fatalf("interpreter replay-compatibility surface drifted from %s.\n"+
			"This surface defines whether journals written by the CURRENT binary replay on the NEXT one.\n"+
			"If the change is deliberate: (1) drain in-flight imperative workflow runs before deploy\n"+
			"(GET /api/mills/safety/quiescence must show active_workflow_runs=0, or pause them),\n"+
			"(2) regenerate with UPDATE_INTERP_SURFACE=1, (3) add [workflow-drain-confirmed] to the MR.\n"+
			"--- golden ---\n%s\n--- current ---\n%s", surfaceGoldenPath, want, got)
	}
}

// TestInterpreterVersionMatchesGoMod closes the silent-bump hole: bumping
// go.starlark.net in go.mod without bumping HostInterpreterVersion would let a
// NEW engine replay journals pinned by the OLD one without tripping the
// version-pin refusal. The constant must always encode the built engine.
func TestInterpreterVersionMatchesGoMod(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	goMod := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "go.mod")
	data, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*go\.starlark\.net\s+(\S+)`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("go.starlark.net not found in go.mod")
	}
	want := "starlark-go@" + m[1]
	if HostInterpreterVersion != want {
		t.Fatalf("HostInterpreterVersion=%q but go.mod builds go.starlark.net %s (want %q).\n"+
			"Bump the constant with the module so version-pinned replays refuse across the engine change,\n"+
			"then follow the drain gate (see TestInterpreterSurfaceGolden).", HostInterpreterVersion, m[1], want)
	}
}
