// Package workflow is the deterministic-replay runtime seed for the Mills
// durable workflow engine (Layer 3). It ports the proven S1 kill-test spike
// (feat/mills-workflow-killtest-spike) into the main module, backing its
// append-only step journal with the merged store.WorkflowDAO instead of an
// in-memory map.
//
// An imperative workflow script runs on an embedded Starlark-Go interpreter and
// is DETERMINISTICALLY REPLAYABLE from the durable step journal: recorded
// effect-calls return their cached result WITHOUT re-executing; only the first
// un-recorded call runs live. Step keys are structured and drift-tolerant so
// inserting or removing an unrelated sibling call never shifts another sibling's
// key. The interpreter universe is capability-confined by construction (no
// fs/net/clock/random and load() disabled).
//
// Layout mirrors the spike: interp.go (interpreter + universe), host.go
// (scope-stack keys, call-hash, read-through), journal_dao.go (DAO adapter).
package workflow

import (
	"fmt"
	"sort"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// stackLocalKey is the thread-local key under which each Starlark thread carries
// its OWN ScopeStack. The root thread carries the root stack; every parallel()
// branch goroutine creates a fresh thread carrying its forked branch stack. This
// is what lets one set of builtins derive per-execution-path keys without any
// shared mutable counter.
const stackLocalKey = "workflow.scopestack"

// fileOptions allows top-level control flow and while so the workflow script may
// use straight-line statements and loops at module level. The universe is still
// whitelist-by-construction (no os/time/random; load disabled via
// thread.Load=nil), so relaxing parse-time control flow does not weaken
// capability confinement.
var fileOptions = &syntax.FileOptions{
	While:           true,
	TopLevelControl: true,
}

// threadStack fetches the calling thread's ScopeStack.
func threadStack(t *starlark.Thread) *ScopeStack {
	if v := t.Local(stackLocalKey); v != nil {
		return v.(*ScopeStack)
	}
	// Should never happen: every thread is seeded before execution.
	panic("workflow: thread has no scope stack")
}

// starlarkArgsToGo converts positional+keyword Starlark args into a canonical
// Go map[string]any for hashing. Positionals are keyed "_0","_1",...; kwargs by
// name. Values are converted via starToGo.
func starlarkArgsToGo(args starlark.Tuple, kwargs []starlark.Tuple) map[string]any {
	out := make(map[string]any, len(args)+len(kwargs))
	for i, a := range args {
		out[fmt.Sprintf("_%d", i)] = starToGo(a)
	}
	for _, kv := range kwargs {
		k, _ := starlark.AsString(kv[0])
		out[k] = starToGo(kv[1])
	}
	return out
}

// starToGo recursively converts a Starlark value to a JSON-canonicalizable Go
// value so list/dict args hash STABLY and STRUCTURALLY.
//
// CARRY-FORWARD #1: the spike collapsed every non-scalar value to its
// .String() form (spike host.go:55-71). That is fragile — two structurally
// identical dicts whose Go-level string rendering differs (e.g. element order,
// or a nested list inside a dict) would hash differently, and a structurally
// different value whose string happened to coincide would hash the same. Here
// lists and dicts are walked recursively into []any / map[string]any, which the
// canonical encoder (host.go) then sorts and serialises deterministically.
func starToGo(v starlark.Value) any {
	switch t := v.(type) {
	case starlark.String:
		return string(t)
	case starlark.Bool:
		return bool(t)
	case starlark.Int:
		i, _ := t.Int64()
		return i
	case starlark.Float:
		return float64(t)
	case starlark.NoneType:
		return nil
	case *starlark.List:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			out = append(out, starToGo(t.Index(i)))
		}
		return out
	case starlark.Tuple:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, starToGo(e))
		}
		return out
	case *starlark.Dict:
		out := make(map[string]any, t.Len())
		for _, item := range t.Items() {
			// item is a 2-tuple (key, value). Keys are canonicalised to their
			// Starlark string form so non-string keys (ints, bools) still hash
			// deterministically and the canonical encoder can sort them.
			k := keyString(item[0])
			out[k] = starToGo(item[1])
		}
		return out
	case *starlark.Set:
		// Sets are unordered; map each element through starToGo, then sort the
		// resulting list by its canonical encoding so iteration order never
		// affects the hash.
		out := make([]any, 0, t.Len())
		for e := range t.Elements() {
			out = append(out, starToGo(e))
		}
		sort.Slice(out, func(i, k int) bool {
			ci, _ := canonicalJSON(out[i])
			ck, _ := canonicalJSON(out[k])
			return string(ci) < string(ck)
		})
		return out
	default:
		// Functions and other opaque values are not hashable args; collapse to
		// their string form for stability (they are never effect inputs).
		return v.String()
	}
}

// keyString renders a dict key as a stable map key. String keys map to their
// raw value; any other key uses its Starlark representation.
func keyString(v starlark.Value) string {
	if s, ok := starlark.AsString(v); ok {
		if _, isStr := v.(starlark.String); isStr {
			return s
		}
	}
	return v.String()
}

// effectBuiltin builds one read-through effect primitive (agent/gate/ctx_now/
// ctx_uuid). It reads the calling thread's stack so parallel branches scope
// correctly.
func (h *EffectHost) effectBuiltin(kind string) *starlark.Builtin {
	return starlark.NewBuiltin(kind, func(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		goArgs := starlarkArgsToGo(args, kwargs)
		res, err := h.readThrough(threadStack(t), kind, goArgs)
		if err != nil {
			return nil, err
		}
		return starlark.String(fmt.Sprintf("%v", res)), nil
	})
}

// builtinParallel implements parallel(fns, fan_out_width=None). Branch keys are
// assigned in SLICE ORDER before any goroutine starts; each goroutine owns a
// fresh thread + forked stack. No shared atomic counter hands out branch ids, so
// finish order never affects keys.
func (h *EffectHost) builtinParallel(callSite string) *starlark.Builtin {
	return starlark.NewBuiltin("parallel", func(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var fns *starlark.List
		fanOut := -1 // -1 => use len(fns)
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "fns", &fns, "fan_out_width?", &fanOut); err != nil {
			return nil, err
		}

		// Collect branch callables in slice order.
		branchFns := make([]starlark.Value, 0, fns.Len())
		for i := 0; i < fns.Len(); i++ {
			branchFns = append(branchFns, fns.Index(i))
		}
		requested := len(branchFns)
		if fanOut >= 0 && fanOut < requested {
			requested = fanOut
		}
		width := clampFanout(requested) // host ceiling, symmetric live+replay

		parent := threadStack(t)
		childStacks := make([]*ScopeStack, width)
		for j := 0; j < width; j++ { // branchKey = j, FIXED here pre-launch
			childStacks[j] = parent.Fork(Frame{Kind: FramePar, CallSite: callSite, BranchKey: j})
		}

		var wg sync.WaitGroup
		errs := make([]error, width)
		for j := 0; j < width; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				// Fresh thread per goroutine; seed ITS stack before exec begins.
				bt := &starlark.Thread{Name: fmt.Sprintf("%s/b%d", h.RunID, j)}
				bt.Load = nil
				bt.SetLocal(stackLocalKey, childStacks[j])
				_, errs[j] = starlark.Call(bt, branchFns[j], nil, nil)
			}(j)
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return nil, e
			}
		}
		return starlark.None, nil
	})
}

// builtinLoopUntilDry implements loop_until_dry(body, max_iter=None). Each
// iteration pushes its own LOOP frame (iter index in the segment) so a divergent
// iteration count never shifts sibling keys. The body returns truthy when the
// queue is "dry" (stop). max_iter is clamped to the host ceiling.
func (h *EffectHost) builtinLoopUntilDry(label string) *starlark.Builtin {
	return starlark.NewBuiltin("loop_until_dry", func(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var body starlark.Value
		maxIter := HostMaxIter
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "body", &body, "max_iter?", &maxIter); err != nil {
			return nil, err
		}
		maxIter = clampIter(maxIter) // host ceiling, symmetric live+replay

		stack := threadStack(t)
		ran := 0
		for i := 0; i < maxIter; i++ {
			stack.Push(Frame{Kind: FrameLoop, Label: label, Iter: i})
			ret, err := starlark.Call(t, body, starlark.Tuple{starlark.MakeInt(i)}, nil)
			stack.Pop()
			if err != nil {
				return nil, err
			}
			ran++
			if ret != nil && bool(ret.Truth()) {
				break // dry
			}
		}
		return starlark.MakeInt(ran), nil
	})
}

// buildUniverse returns the EXACTLY-seven predeclared names. No os/time/random,
// no load. Capability confinement is by construction. merge is journaled like
// every effect; its executor seam fails closed when unconfigured (S6-full).
func (h *EffectHost) buildUniverse() starlark.StringDict {
	return starlark.StringDict{
		"agent":          h.effectBuiltin("agent"),
		"gate":           h.effectBuiltin("gate"),
		"merge":          h.effectBuiltin("merge"),
		"ctx_now":        h.effectBuiltin("ctx_now"),
		"ctx_uuid":       h.effectBuiltin("ctx_uuid"),
		"parallel":       h.builtinParallel("p0"),
		"loop_until_dry": h.builtinLoopUntilDry("drain"),
	}
}

// UniverseNames returns the sorted list of predeclared names exposed to a
// script. The whole point of capability confinement is that this is EXACTLY the
// 7 effect builtins — nothing else. Tests assert against it so a future leak
// (adding os/time/random to the universe) is caught structurally, not just by
// the accident that a leaked name happens to error for an unrelated reason.
func (h *EffectHost) UniverseNames() []string {
	u := h.buildUniverse()
	names := make([]string, 0, len(u))
	for k := range u {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Run executes (first run OR replay — identical code path) the workflow script.
// The interpreter-version pin is checked ONCE up front so a drifted replay
// refuses before ANY effect runs. The root thread carries the root ScopeStack;
// thread.Load=nil disables load(); the universe contains only the 6 builtins.
func (h *EffectHost) Run(script string) error {
	if err := h.checkVersion(); err != nil {
		return err // version-pin REFUSE up front
	}
	thread := &starlark.Thread{Name: h.RunID}
	thread.Load = nil // any load() => "load not implemented"
	thread.SetLocal(stackLocalKey, NewRootStack())
	_, err := starlark.ExecFileOptions(fileOptions, thread, "workflow.star", script, h.buildUniverse())
	return err
}
