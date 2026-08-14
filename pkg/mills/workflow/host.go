package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Host constants. HostInterpreterVersion is pinned on a run; replay refuses if
// it differs. HostMaxFanout / HostMaxIter are the host resource ceiling: script
// -supplied fan_out_width / max_iter are clamped to these regardless of input.
const (
	HostInterpreterVersion = "starlark-go@v0.0.0-20260708150628-5395d018f003"
	HostMaxFanout          = 8
	HostMaxIter            = 64
)

// FrameKind tags a scope-stack frame.
type FrameKind int

const (
	// FrameRoot is the single top-level frame.
	FrameRoot FrameKind = iota
	// FrameLoop is one loop_until_dry iteration.
	FrameLoop
	// FramePar is one parallel() branch.
	FramePar
)

// Frame is one path segment of a structured step key. dupCounts is per-frame
// (NOT global): it counts prior calls sharing the same (primKind, callHash)
// within THIS frame only. That per-frame, args-keyed counter is what makes the
// key drift-tolerant — inserting/removing an unrelated sibling call does not
// shift any other sibling's key.
type Frame struct {
	Kind      FrameKind
	Label     string // loop label
	Iter      int    // loop iteration index
	CallSite  string // parallel() call-site id (stable per script location)
	BranchKey int    // parallel branch index, assigned BEFORE goroutines run
	dupCounts map[string]int
}

func (f Frame) segment() string {
	switch f.Kind {
	case FrameRoot:
		return "root"
	case FrameLoop:
		return fmt.Sprintf("loop:%s#%d", f.Label, f.Iter)
	case FramePar:
		return fmt.Sprintf("par:%s/b%d", f.CallSite, f.BranchKey)
	}
	return "?"
}

// ScopeStack is PER execution path. The root execution owns one stack;
// parallel() FORKS a child stack per branch (copying the parent frame slice by
// value, then pushing the branch frame). Goroutines never share a stack or a
// dupCounts map, so key derivation is pure and lock-free per goroutine.
type ScopeStack struct {
	frames []Frame
}

// NewRootStack returns a stack with only the ROOT frame.
func NewRootStack() *ScopeStack {
	return &ScopeStack{frames: []Frame{{Kind: FrameRoot, dupCounts: map[string]int{}}}}
}

// Fork copies the parent path by value and pushes a child branch frame with a
// FRESH dupCounts map. Used by parallel() so each branch derives keys in
// isolation regardless of goroutine finish order.
func (s *ScopeStack) Fork(child Frame) *ScopeStack {
	child.dupCounts = map[string]int{}
	cp := make([]Frame, len(s.frames), len(s.frames)+1)
	copy(cp, s.frames)
	return &ScopeStack{frames: append(cp, child)}
}

// Push adds a frame (used by loop_until_dry per iteration).
func (s *ScopeStack) Push(f Frame) {
	f.dupCounts = map[string]int{}
	s.frames = append(s.frames, f)
}

// Pop removes the top frame.
func (s *ScopeStack) Pop() { s.frames = s.frames[:len(s.frames)-1] }

// LeafKey derives the full, drift-tolerant step_key for a call in the TOP
// frame. The leaf is "<primKind>~<callHash[:16]>#<dupOrdinal>" where dupOrdinal
// disambiguates ONLY calls with identical (primKind, callHash) within this
// frame. For distinct args dupOrdinal is always 0, so unrelated inserts/removes
// leave sibling keys byte-identical (the core drift-tolerance property).
func (s *ScopeStack) LeafKey(primKind, callHash string) string {
	top := &s.frames[len(s.frames)-1]
	dupKey := primKind + "|" + callHash
	dup := top.dupCounts[dupKey]
	top.dupCounts[dupKey] = dup + 1

	segs := make([]string, 0, len(s.frames)+1)
	for _, f := range s.frames {
		segs = append(segs, f.segment())
	}
	leaf := primKind + "~" + callHash[:16] + "#" + strconv.Itoa(dup)
	segs = append(segs, leaf)
	return strings.Join(segs, "/")
}

// canonicalJSON emits bytes with recursively sorted map keys and stable number
// formatting so arg ordering never changes the hash.
func canonicalJSON(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeCanon(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// writeCanon is the recursive canonical encoder (CARRY-FORWARD #1). It walks
// maps and slices, sorting map keys, so structurally equal arguments serialise
// identically regardless of insertion order, and structurally different
// arguments never collide. Scalars fall through to encoding/json which produces
// a stable representation.
func writeCanon(b *strings.Builder, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(kb)
			b.WriteByte(':')
			if err := writeCanon(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanon(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		jb, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(jb)
	}
	return nil
}

// canonicalCallHash = sha256(primName + 0x00 + canonicalJSON(args)). The 0x00
// separator prevents prim+args ambiguity.
func canonicalCallHash(primName string, args map[string]any) string {
	cj, _ := canonicalJSON(args)
	h := sha256.New()
	h.Write([]byte(primName))
	h.Write([]byte{0x00})
	h.Write(cj)
	return hex.EncodeToString(h.Sum(nil))
}

// QuarantineError freezes a single step on a call_hash mismatch (or a prior
// quarantine). The run is frozen; the step is NOT re-executed and siblings are
// NOT aborted.
type QuarantineError struct {
	StepKey string
	Want    string // recorded call_hash
	Got     string // re-derived call_hash
}

func (e *QuarantineError) Error() string {
	return fmt.Sprintf("QUARANTINE step=%s want_hash=%s got_hash=%s (run frozen)", e.StepKey, e.Want, e.Got)
}

// VersionDriftError refuses a replay whose pinned interpreter version differs
// from the host's.
type VersionDriftError struct {
	Pinned string
	Host   string
}

func (e *VersionDriftError) Error() string {
	return fmt.Sprintf("REFUSE replay: interpreter_version drift pinned=%s host=%s", e.Pinned, e.Host)
}

// Status is the lifecycle of a journaled step as seen by the runtime. It maps
// onto store.WorkflowStepStatus in the DAO adapter.
type Status string

const (
	// StatusPending is written BEFORE the effect executes (crash-safety: a
	// pending with no success = interrupted, must re-run, not double-run).
	StatusPending Status = "pending"
	// StatusSuccess is the upsert written AFTER the effect, carrying the
	// memoized result blob. Replay short-circuits on this.
	StatusSuccess Status = "success"
	// StatusQuarantined marks a step frozen by a non-determinism tripwire
	// (call_hash mismatch). The run is frozen; the step never re-executes.
	StatusQuarantined Status = "quarantined"
)

// Record is one journal entry as the runtime sees it, keyed by (RunID,
// StepKey). The DAO adapter translates between this and store.WorkflowStep.
//
// CallHash is the idempotency key recorded INSIDE the step so replay can detect
// non-determinism (a recorded step that re-derives different args). EffectSeq
// is the success-row ordinal at record time (for audit). InterpreterVersion is
// pinned on the run and checked at replay start.
//
// SpawnID + CostSource + CostUSD carry the provenance a real effect executor
// (S6-min: a WorkerRunner-backed agent() call) attaches to the terminal step.
// The default stub leaves them zero, so the legacy path is byte-identical.
type Record struct {
	RunID              string
	StepKey            string
	PrimName           string
	CallHash           string
	Status             Status
	ResultBlob         []byte
	EffectSeq          int64
	InterpreterVersion string

	// SpawnID is the durable spawn handle the effect produced (agent() →
	// WorkerResult.SpawnID). Empty for non-spawn effects and the stub.
	SpawnID string
	// CostSource is the provenance token (real|estimated|unavailable) of
	// CostUSD. Empty when no cost is known.
	CostSource string
	// CostUSD is the effect's reported cost. Zero for the stub.
	CostUSD float64
}

// Journal is the durable read-through store the host funnels every effect
// through. Get returns the prior record for (runID, stepKey) if any. Append
// upserts by (runID, stepKey): the pending -> success transition overwrites in
// place, which is what makes the store idempotent on replay. SuccessCount
// reports how many real effects have fired (success rows), replacing the
// spike's in-memory atomic counter.
type Journal interface {
	Get(runID, stepKey string) (Record, bool, error)
	Append(r Record) error
	SuccessCount(runID string) (int64, error)
}

// EffectHost owns the run, the journal, and the live effect stub. live-vs-replay
// is NOT a mode flag: it is decided per-call by whether journal.Get returns ok.
// The "effect counter" is the journal's count of success rows, so dropping and
// rebuilding the host never resets it.
type EffectHost struct {
	RunID              string
	InterpreterVersion string // pinned on the run; checked at replay start
	Journal            Journal

	// liveFn is the STUB effect. It receives the primitive kind, the canonical
	// args, and the freshly-allocated success-row sequence number, and returns a
	// deterministic result value. Overridable so a test can inject
	// goroutine-scheduling jitter into branch effects.
	liveFn func(primKind string, args map[string]any, seq int64) any

	// effectExec, when non-nil, SUPERSEDES liveFn on the LIVE path. It is the
	// S6-min seam where a real executor (a WorkerRunner-backed agent() / a gate
	// evaluator) runs the actual side-effect. Unlike liveFn it receives the
	// derived stepKey (so it can derive a deterministic per-step idempotency
	// key) and returns an EffectResult carrying both the memoized value and the
	// provenance (spawn id, cost) the terminal step records. Nil keeps the
	// default stub path byte-identical with the spike, so existing tests are
	// untouched.
	effectExec func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error)
}

// EffectResult is what a real effect executor returns to the host. Value is the
// memoized result the journal serialises (and replay returns verbatim);
// SpawnID/CostSource/CostUSD are the provenance recorded on the terminal step.
type EffectResult struct {
	Value      any
	SpawnID    string
	CostSource string
	CostUSD    float64
}

// SetEffectExec installs the real effect executor (S6-min). Passing nil reverts
// to the default stub (liveFn) path. Defined as an exported setter so the
// WorkflowInterpreter (runtime.go) can wire agent()/gate() to a WorkerRunner
// without reaching into unexported fields from another file's perspective —
// though same-package, this keeps the seam explicit and documented.
func (h *EffectHost) SetEffectExec(fn func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error)) {
	h.effectExec = fn
}

// NewEffectHost builds a host bound to the supplied journal. The default stub
// effect returns "<primKind>#<seq>".
func NewEffectHost(runID string, j Journal) *EffectHost {
	return &EffectHost{
		RunID:              runID,
		InterpreterVersion: HostInterpreterVersion,
		Journal:            j,
		liveFn: func(primKind string, args map[string]any, seq int64) any {
			return fmt.Sprintf("%s#%d", primKind, seq)
		},
	}
}

// EffectCount reports how many real effects have fired (durable success rows).
func (h *EffectHost) EffectCount() (int64, error) { return h.Journal.SuccessCount(h.RunID) }

// checkVersion enforces the interpreter-version pin.
func (h *EffectHost) checkVersion() error {
	if h.InterpreterVersion != HostInterpreterVersion {
		return &VersionDriftError{Pinned: h.InterpreterVersion, Host: HostInterpreterVersion}
	}
	return nil
}

// readThrough is the single chokepoint every effect builtin funnels through.
// Algorithm (record-before-effect):
//  1. derive callHash + stepKey (pure, lock-free per goroutine)
//  2. defensive version recheck
//  3. journal.Get -> REPLAY (success/quarantined) or RE-RUN (pending) or LIVE (!ok)
//     REPLAY: hash mismatch => QUARANTINE (freeze step, no exec, no mass-abort);
//     prior quarantined => stay frozen; success => decode cached blob, no new
//     success row.
//     RE-RUN: a recorded but still-PENDING step is an INTERRUPTED effect (the
//     process crashed after the pending append but before the success append).
//     It must NOT short-circuit as a cached success — the success blob is empty.
//     It falls through to the LIVE/executor path so the effect re-runs. This is
//     exactly-once-safe because the real executor (S6-min) keys the spawn on a
//     deterministic idempotency key (run+step+args) and re-attaches via Resume,
//     so the re-run dedupes to the same spawn rather than double-dispatching.
//     LIVE: append pending BEFORE effect; run executor/liveFn; append success
//     (which advances the durable success count).
func (h *EffectHost) readThrough(stack *ScopeStack, primKind string, args map[string]any) (any, error) {
	if err := h.checkVersion(); err != nil {
		return nil, err
	}
	callHash := canonicalCallHash(primKind, args)
	stepKey := stack.LeafKey(primKind, callHash)

	prior, ok, err := h.Journal.Get(h.RunID, stepKey)
	if err != nil {
		return nil, err
	}
	if ok {
		// Determinism tripwire applies to ANY recorded step (incl. pending).
		if prior.CallHash != callHash {
			if aerr := h.Journal.Append(Record{
				RunID:    h.RunID,
				StepKey:  stepKey,
				PrimName: primKind,
				CallHash: prior.CallHash,
				Status:   StatusQuarantined,
			}); aerr != nil {
				return nil, aerr
			}
			return nil, &QuarantineError{StepKey: stepKey, Want: prior.CallHash, Got: callHash}
		}
		if prior.Status == StatusQuarantined {
			return nil, &QuarantineError{StepKey: stepKey, Want: prior.CallHash, Got: callHash}
		}
		if prior.Status == StatusSuccess {
			// REPLAY -- short-circuit on the durable success blob, NO new row.
			var out any
			_ = json.Unmarshal(prior.ResultBlob, &out)
			return out, nil
		}
		// prior.Status == StatusPending: interrupted effect. Fall through to the
		// LIVE path to RE-RUN it (exactly-once via the executor's resume/keying).
		// The pending row already exists, so AppendStep below is an idempotent
		// no-op advance; the success append then completes it.
	}

	// LIVE path -- record-before-effect.
	if err := h.Journal.Append(Record{
		RunID:              h.RunID,
		StepKey:            stepKey,
		PrimName:           primKind,
		CallHash:           callHash,
		Status:             StatusPending,
		InterpreterVersion: h.InterpreterVersion,
	}); err != nil {
		return nil, err
	}
	// The success-row count BEFORE this effect commits is its ordinal seq; the
	// completed append below increments the durable count.
	seq, err := h.Journal.SuccessCount(h.RunID)
	if err != nil {
		return nil, err
	}
	seq++

	// Real-executor seam (S6-min): when effectExec is wired, it runs the live
	// side-effect (spawn an agent, evaluate a gate) and returns provenance the
	// terminal step records. A non-nil error from the executor is propagated
	// WITHOUT a success append — the pending row stays recoverable, so a
	// transient spawn failure re-runs on the next tick rather than being frozen.
	var (
		result              any
		spawnID, costSource string
		costUSD             float64
	)
	if h.effectExec != nil {
		er, eerr := h.effectExec(stepKey, primKind, args, seq)
		if eerr != nil {
			return nil, eerr
		}
		result = er.Value
		spawnID = er.SpawnID
		costSource = er.CostSource
		costUSD = er.CostUSD
	} else {
		result = h.liveFn(primKind, args, seq)
	}

	blob, _ := json.Marshal(result)
	if err := h.Journal.Append(Record{
		RunID:              h.RunID,
		StepKey:            stepKey,
		PrimName:           primKind,
		CallHash:           callHash,
		Status:             StatusSuccess,
		ResultBlob:         blob,
		EffectSeq:          seq,
		InterpreterVersion: h.InterpreterVersion,
		SpawnID:            spawnID,
		CostSource:         costSource,
		CostUSD:            costUSD,
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// clampFanout / clampIter apply the host ceiling. They run identically on BOTH
// live and replay (before fan-out) so derived step keys match.
func clampFanout(req int) int {
	if req > HostMaxFanout {
		return HostMaxFanout
	}
	if req < 0 {
		return 0
	}
	return req
}

func clampIter(req int) int {
	if req > HostMaxIter {
		return HostMaxIter
	}
	if req < 0 {
		return 0
	}
	return req
}
