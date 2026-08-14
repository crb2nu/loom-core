package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/generator"
)

const (
	testRulesBlockV1 = generator.CodexRulesBeginMarker + "\n" +
		"prefix_rule(\n    pattern = [\"git\"],\n    decision = \"allow\",\n)\n" +
		generator.CodexRulesEndMarker + "\n"
	testRulesBlockV2 = generator.CodexRulesBeginMarker + "\n" +
		"prefix_rule(\n    pattern = [\"git\"],\n    decision = \"allow\",\n)\n" +
		"prefix_rule(\n    pattern = [\"/usr/bin/git\"],\n    decision = \"allow\",\n)\n" +
		generator.CodexRulesEndMarker + "\n"
	// What the Codex TUI appends when the user approves a command — MUST
	// survive every merge.
	testUserAppended = "prefix_rule(pattern=[\"sleep\", \"2\"], decision=\"allow\")\n"
)

func TestMergeMarkerBlock(t *testing.T) {
	begin, end := generator.CodexRulesBeginMarker, generator.CodexRulesEndMarker

	t.Run("empty existing gets block alone", func(t *testing.T) {
		merged, changed := MergeMarkerBlock(nil, []byte(testRulesBlockV1), begin, end)
		if !changed || string(merged) != testRulesBlockV1 {
			t.Errorf("changed=%v merged=\n%s", changed, merged)
		}
	})

	t.Run("no markers prepends and preserves everything", func(t *testing.T) {
		existing := "# hand-written header\n" + testUserAppended
		merged, changed := MergeMarkerBlock([]byte(existing), []byte(testRulesBlockV1), begin, end)
		if !changed {
			t.Fatal("expected change")
		}
		out := string(merged)
		if !strings.HasPrefix(out, generator.CodexRulesBeginMarker) {
			t.Errorf("managed block must lead the file:\n%s", out)
		}
		for _, keep := range []string{"# hand-written header", testUserAppended} {
			if !strings.Contains(out, keep) {
				t.Errorf("existing content %q was lost:\n%s", keep, out)
			}
		}
	})

	t.Run("markers replaced in place, surroundings preserved", func(t *testing.T) {
		existing := "# user preamble kept above\n" + testRulesBlockV1 + testUserAppended
		merged, changed := MergeMarkerBlock([]byte(existing), []byte(testRulesBlockV2), begin, end)
		if !changed {
			t.Fatal("expected change")
		}
		out := string(merged)
		if !strings.Contains(out, `pattern = ["/usr/bin/git"]`) {
			t.Errorf("v2 rule not merged in:\n%s", out)
		}
		if !strings.HasPrefix(out, "# user preamble kept above\n") {
			t.Errorf("content above the block was lost:\n%s", out)
		}
		if !strings.Contains(out, testUserAppended) {
			t.Errorf("Codex-appended rule below the block was lost:\n%s", out)
		}
		if strings.Count(out, generator.CodexRulesBeginMarker) != 1 {
			t.Errorf("begin marker duplicated:\n%s", out)
		}
	})

	t.Run("idempotent when block unchanged", func(t *testing.T) {
		existing := testRulesBlockV1 + "\n" + testUserAppended
		merged, changed := MergeMarkerBlock([]byte(existing), []byte(testRulesBlockV1), begin, end)
		if changed {
			t.Errorf("unchanged block reported changed; merged=\n%s", merged)
		}
	})

	t.Run("malformed markers fall back to prepend, nothing deleted", func(t *testing.T) {
		// End marker before begin marker — never treat that span as managed.
		existing := generator.CodexRulesEndMarker + "\n" + testUserAppended + generator.CodexRulesBeginMarker + "\n"
		merged, changed := MergeMarkerBlock([]byte(existing), []byte(testRulesBlockV1), begin, end)
		if !changed {
			t.Fatal("expected change")
		}
		if !strings.Contains(string(merged), testUserAppended) {
			t.Errorf("malformed-marker content deleted:\n%s", merged)
		}
	})
}

// TestSyncCodexRulesGenerated covers the file-level merge: dest created when
// missing (incl. the rules/ dir), Codex-appended rules preserved on update,
// and no rewrite when already up to date.
func TestSyncCodexRulesGenerated(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gen", "default.rules")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(testRulesBlockV1), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "home", ".codex", "rules", "default.rules")

	if err := syncCodexRulesGenerated(src, dst); err != nil {
		t.Fatalf("initial merge: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dest not created: %v", err)
	}
	if string(data) != testRulesBlockV1 {
		t.Errorf("fresh dest = \n%s\nwant generated block", data)
	}

	// Codex TUI appends an approval rule out-of-band.
	if err := os.WriteFile(dst, append(data, []byte(testUserAppended)...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Regen with an updated managed block.
	if err := os.WriteFile(src, []byte(testRulesBlockV2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexRulesGenerated(src, dst); err != nil {
		t.Fatalf("update merge: %v", err)
	}
	data, _ = os.ReadFile(dst)
	out := string(data)
	if !strings.Contains(out, `pattern = ["/usr/bin/git"]`) || !strings.Contains(out, testUserAppended) {
		t.Errorf("update merge lost content:\n%s", out)
	}

	// Third run with identical block must not rewrite the file.
	before, _ := os.Stat(dst)
	if err := syncCodexRulesGenerated(src, dst); err != nil {
		t.Fatalf("noop merge: %v", err)
	}
	after, _ := os.Stat(dst)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("up-to-date file was rewritten")
	}
}
