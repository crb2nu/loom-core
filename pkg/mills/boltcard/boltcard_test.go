package boltcard

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestBoltSpecHeadlineUTF8(t *testing.T) {
	got := SpecHeadline("\n  " + strings.Repeat("界", 170) + "\nignored")
	if len([]rune(got)) != 160 || !ValidUTF8(got) {
		t.Fatalf("headline is %d runes, valid=%v", len([]rune(got)), ValidUTF8(got))
	}
}

func TestBoltDiffPrecedenceAndPatchCount(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n-old\n+new\n+more\n"
	got := DiffFromStages([]*store.StageResult{{Artifacts: map[string]any{"diff_patch": patch}}})
	if got.Source != "patch_count" || got.Files != 1 || got.Added != 2 || got.Removed != 1 {
		t.Fatalf("patch diff = %+v", got)
	}
	got = DiffFromStages([]*store.StageResult{{Artifacts: map[string]any{"diff_patch": patch, "files_changed": 3.0, "lines_added": 8.0, "lines_removed": 5.0}}})
	if got.Source != "artifacts" || got.Files != 3 || got.Added != 8 || got.Removed != 5 {
		t.Fatalf("artifact diff = %+v", got)
	}
}
