package gates

import (
	"context"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// diff fixtures. newFileBlock/modifiedBlock mirror real `git diff` output
// shapes; the gate must key on `new file mode` / `--- /dev/null`, not on
// content.
func newFileBlock(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"new file mode 100644\n" +
		"index 0000000..1111111\n" +
		"--- /dev/null\n" +
		"+++ b/" + path + "\n" +
		"@@ -0,0 +1,2 @@\n+package x\n+var Y = 1\n"
}

func modifiedBlock(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1,1 +1,2 @@\n context\n+added\n"
}

func deletedBlock(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"deleted file mode 100644\n" +
		"index 1111111..0000000\n" +
		"--- a/" + path + "\n" +
		"+++ /dev/null\n" +
		"@@ -1,1 +0,0 @@\n-gone\n"
}

func TestFabricatedSlice_Evaluate(t *testing.T) {
	cases := []struct {
		name         string
		in           StageInput
		wantPass     bool
		wantSkip     bool
		wantTerminal bool
		wantReason   string
	}{
		{
			name: "modified plus new passes: the wiring edit is present",
			in: StageInput{DiffPatch: []byte(
				newFileBlock("pkg/mills/foo/new.go") + modifiedBlock("cmd/op/main.go"))},
			wantPass:   true,
			wantReason: FabricatedSliceReasonModifiesExisting,
		},
		{
			name:       "all-new non-test Go fails: the dead-merge signature",
			in:         StageInput{DiffPatch: []byte(newFileBlock("pkg/mills/foo/a.go") + newFileBlock("pkg/mills/foo/b.go"))},
			wantPass:   false,
			wantReason: FabricatedSliceReasonAllNew,
		},
		{
			name: "all-new with changelog fragment still fails: fragments do not wire code",
			in: StageInput{DiffPatch: []byte(
				newFileBlock("pkg/mills/foo/a.go") + newFileBlock("changelog.d/x.fixed.md"))},
			wantPass:   false,
			wantReason: FabricatedSliceReasonAllNew,
		},
		{
			name: "emit-flagged fabricated slice fails terminally: a retry replays the same plan",
			in: StageInput{
				DiffPatch: []byte(newFileBlock("pkg/mills/foo/a.go")),
				Item: &store.BacklogItem{Slices: []store.Slice{{
					Name: "s", Files: []string{"pkg/mills/foo/a.go"},
					Fabricated: true, MissingFiles: []string{"pkg/mills/foo/a.go"},
				}}},
			},
			wantPass:     false,
			wantTerminal: true,
			wantReason:   FabricatedSliceReasonEmitFlagged,
		},
		{
			name:       "all-new docs only passes: no Go source to strand",
			in:         StageInput{DiffPatch: []byte(newFileBlock("docs/runbook.md") + newFileBlock("changelog.d/y.added.md"))},
			wantPass:   true,
			wantReason: FabricatedSliceReasonBenignNewFiles,
		},
		{
			name:       "all-new test files pass: tests attach to existing packages without a wiring edit",
			in:         StageInput{DiffPatch: []byte(newFileBlock("pkg/mills/foo_test.go") + newFileBlock("pkg/mills/testdata/fixture.json"))},
			wantPass:   true,
			wantReason: FabricatedSliceReasonBenignNewFiles,
		},
		{
			name:       "deletion-only passes: touches a pre-existing file",
			in:         StageInput{DiffPatch: []byte(deletedBlock("pkg/mills/old.go"))},
			wantPass:   true,
			wantReason: FabricatedSliceReasonModifiesExisting,
		},
		{
			name:       "empty patch skips: shape unobservable",
			in:         StageInput{FilesChanged: []string{"pkg/mills/foo/a.go"}},
			wantPass:   true,
			wantSkip:   true,
			wantReason: FabricatedSliceReasonUnclassifiable,
		},
		{
			name:       "headerless patch skips: nothing to classify",
			in:         StageInput{DiffPatch: []byte("not a diff at all\n")},
			wantPass:   true,
			wantSkip:   true,
			wantReason: FabricatedSliceReasonUnclassifiable,
		},
		{
			name: "truncated patch skips: FilesChanged names more files than the patch shows",
			in: StageInput{
				DiffPatch:    []byte(newFileBlock("pkg/mills/foo/a.go")),
				FilesChanged: []string{"pkg/mills/foo/a.go", "cmd/op/main.go"},
			},
			wantPass:   true,
			wantSkip:   true,
			wantReason: FabricatedSliceReasonUnclassifiable,
		},
		{
			name: "bootstrapped project skips: a freshly-seeded repo is legitimately all-new",
			in: StageInput{
				DiffPatch:           []byte(newFileBlock("pkg/foo/a.go")),
				ProjectBootstrapped: true,
			},
			wantPass:   true,
			wantSkip:   true,
			wantReason: FabricatedSliceReasonBootstrapped,
		},
	}

	g := &FabricatedSlice{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.Evaluate(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if out.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (reasons=%v)", out.Pass, tc.wantPass, out.Reasons)
			}
			if out.Skip != tc.wantSkip {
				t.Errorf("Skip = %v, want %v (reasons=%v)", out.Skip, tc.wantSkip, out.Reasons)
			}
			if out.Terminal != tc.wantTerminal {
				t.Errorf("Terminal = %v, want %v (reasons=%v)", out.Terminal, tc.wantTerminal, out.Reasons)
			}
			if tc.wantReason != "" && !strings.Contains(strings.Join(out.Reasons, "\n"), tc.wantReason) {
				t.Errorf("Reasons %v missing code %q", out.Reasons, tc.wantReason)
			}
			if out.JudgedBy != "go" {
				t.Errorf("JudgedBy = %q, want \"go\"", out.JudgedBy)
			}
		})
	}
}

func TestFabricatedSlice_Name(t *testing.T) {
	if got := (&FabricatedSlice{}).Name(); got != "fabricated_slice" {
		t.Errorf("Name() = %q, want \"fabricated_slice\"", got)
	}
}

// TestFabricatedSlice_Registered pins the wiring: the gate must resolve from
// the default registry under the name the post_implement_gate bundle uses.
func TestFabricatedSlice_Registered(t *testing.T) {
	if _, err := Default().Get("fabricated_slice"); err != nil {
		t.Fatalf("default registry has no fabricated_slice gate: %v", err)
	}
}

func TestDiffFileEntries(t *testing.T) {
	patch := newFileBlock("pkg/a/new.go") + modifiedBlock("pkg/b/old.go") + deletedBlock("pkg/c/gone.go")
	entries := diffFileEntries([]byte(patch))
	if len(entries) != 3 {
		t.Fatalf("entries=%d, want 3: %+v", len(entries), entries)
	}
	want := map[string]bool{"pkg/a/new.go": true, "pkg/b/old.go": false, "pkg/c/gone.go": false}
	for _, e := range entries {
		isNew, ok := want[e.Path]
		if !ok {
			t.Errorf("unexpected entry path %q", e.Path)
			continue
		}
		if e.New != isNew {
			t.Errorf("entry %q New=%v, want %v", e.Path, e.New, isNew)
		}
	}
}
