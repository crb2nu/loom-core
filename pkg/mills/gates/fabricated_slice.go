package gates

import (
	"bufio"
	"bytes"
	"context"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	FabricatedSliceReasonModifiesExisting = "fabricated_slice.modifies_existing"
	FabricatedSliceReasonBenignNewFiles   = "fabricated_slice.benign_new_files"
	FabricatedSliceReasonAllNew           = "fabricated_slice.all_new_go_files"
	FabricatedSliceReasonEmitFlagged      = "fabricated_slice.emit_time_fabricated"
	FabricatedSliceReasonUnclassifiable   = "fabricated_slice.diff_unclassifiable"
	FabricatedSliceReasonBootstrapped     = "fabricated_slice.bootstrapped_project"
)

// FabricatedSlice fails when the implement stage's diff carries the
// fabricated-slice signature: EVERY file in the diff is newly created, zero
// pre-existing files are modified, and at least one of the new files is a
// non-test Go source file. New Go code that no existing file imports merges
// dead by construction — wiring a new file into the build requires editing an
// existing consumer — so an all-new diff is either a fabricated plan (the
// council declared paths that never existed and the implement run invented
// files to satisfy them) or an incomplete slice whose wiring edit is missing.
// Neither may proceed to review: a 2026-08 sweep of first-parent merges found
// 17 psl-plan-council merges that landed 27 dead Go files exactly this way,
// each one green through every other gate (the scope envelope is BUILT from
// the fabricated paths, so scope can never catch this class).
//
// The verdict is computed from the diff alone, so the gate covers every
// slice-minting path (council mutator, plan-slice emitter, Spinning Room,
// agent-authored plan_slice stages) with no repo access. When the emitter's
// grounding hook additionally stamped the slice Fabricated at emit time —
// every declared file absent from the repo tree — the failure is Terminal:
// re-running implement replays the same fabricated plan and cannot change
// the outcome, so the run escalates immediately instead of burning retries.
//
// Deliberate carve-outs: bootstrapped projects (a freshly-seeded repo is
// legitimately all-new), diffs whose per-file classification is unavailable
// or visibly truncated (skip, never guess), and all-new diffs containing no
// non-test Go source (docs, fixtures, changelog fragments attach to existing
// trees without a wiring edit).
type FabricatedSlice struct{}

// Name identifies the gate in persistence + logs.
func (g *FabricatedSlice) Name() string { return "fabricated_slice" }

// Evaluate classifies the implement diff's added-vs-modified shape.
func (g *FabricatedSlice) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if in.ProjectBootstrapped {
		return codedSkip(FabricatedSliceReasonBootstrapped,
			"target project is bootstrapped; a freshly-seeded repo is legitimately all-new"), nil
	}
	if len(in.DiffPatch) == 0 {
		return codedSkip(FabricatedSliceReasonUnclassifiable,
			"no diff patch captured; added-vs-modified shape is unobservable"), nil
	}
	entries := diffFileEntries(in.DiffPatch)
	if len(entries) == 0 {
		return codedSkip(FabricatedSliceReasonUnclassifiable,
			"diff patch carries no per-file headers; added-vs-modified shape is unobservable"), nil
	}
	// A capped capture can drop whole files from the tail of the patch while
	// the numstat-derived FilesChanged still names them. Judging the visible
	// prefix would let a truncated-away modification read as "all new".
	if len(in.FilesChanged) > len(entries) {
		return codedSkip(FabricatedSliceReasonUnclassifiable,
			"diff patch appears truncated (fewer file headers than files changed); refusing to classify"), nil
	}

	var newGo []string
	for _, e := range entries {
		if !e.New {
			return codedPass(FabricatedSliceReasonModifiesExisting), nil
		}
		if strings.HasSuffix(e.Path, ".go") && !looksLikeTestFile(e.Path) {
			newGo = append(newGo, e.Path)
		}
	}
	if len(newGo) == 0 {
		return codedPass(FabricatedSliceReasonBenignNewFiles), nil
	}

	out := Outcome{JudgedBy: "go"}
	for _, p := range newGo {
		out.Reasons = append(out.Reasons,
			"["+FabricatedSliceReasonAllNew+"] newly created non-test Go file with zero modifications to existing files — nothing can reach it: "+p)
	}
	out.Reasons = append(out.Reasons,
		"["+FabricatedSliceReasonAllNew+"] every file in the diff is newly created; new Go code merges dead unless the same change wires it into an existing consumer")
	if sliceFabricatedAtEmit(in.Item) {
		out.Terminal = true
		out.Reasons = append(out.Reasons,
			"["+FabricatedSliceReasonEmitFlagged+"] emit-time grounding found every declared slice file absent from the repo; a retry replays the same fabricated plan — the plan needs re-grounding")
	}
	return out, nil
}

// sliceFabricatedAtEmit reports whether the emitter's grounding hook marked
// any of the item's slices as fabricated (every declared file absent at the
// grounding revision).
func sliceFabricatedAtEmit(item *store.BacklogItem) bool {
	if item == nil {
		return false
	}
	for _, s := range item.Slices {
		if s.Fabricated {
			return true
		}
	}
	return false
}

// diffFileEntry is one file's contribution to a unified diff, reduced to the
// only fact this gate needs: whether the file existed before the change.
type diffFileEntry struct {
	Path string
	New  bool
}

// diffFileEntries parses a git unified diff into per-file entries. A file is
// New when its block carries `new file mode` or a `--- /dev/null` old side;
// modifications, deletions, and renames all touch a pre-existing path and
// stay New=false. Paths are taken from the `+++ b/` line when present (it
// survives more diff dialects than the `diff --git` header), falling back to
// the header's b-side. Parsing is line-anchored and tolerant: an unrecognized
// block yields no entry rather than a wrong one.
func diffFileEntries(patch []byte) []diffFileEntry {
	var entries []diffFileEntry
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	cur := -1
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path := ""
			// Header shape: `diff --git a/<path> b/<path>`. Repo paths here
			// are space-free in practice; a path that defeats this split is
			// refined by the `+++ b/` line below.
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				path = line[i+len(" b/"):]
			}
			entries = append(entries, diffFileEntry{Path: path})
			cur = len(entries) - 1
		case cur >= 0 && strings.HasPrefix(line, "new file mode "):
			entries[cur].New = true
		case cur >= 0 && strings.HasPrefix(line, "--- /dev/null"):
			entries[cur].New = true
		case cur >= 0 && strings.HasPrefix(line, "+++ b/"):
			if p := strings.TrimSpace(strings.TrimPrefix(line, "+++ b/")); p != "" {
				entries[cur].Path = p
			}
		}
	}
	// Drop headerless artifacts (a block whose path never resolved).
	kept := entries[:0]
	for _, e := range entries {
		if strings.TrimSpace(e.Path) != "" {
			kept = append(kept, e)
		}
	}
	return kept
}
