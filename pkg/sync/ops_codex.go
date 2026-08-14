// ops_codex.go — Codex platform sync: snapshot, ensure, and execpolicy-rules
// merge operations.
package sync

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crb2nu/loom/pkg/generator"
)

type codexConfigSnapshot struct {
	config []byte
}

func readCodexConfigSnapshot(homePath string) codexConfigSnapshot {
	return codexConfigSnapshot{
		config: readTOMLSnapshot(filepath.Join(homePath, "config.toml")),
	}
}

func ensureCodexConfigFiles(homePath string, snapshot codexConfigSnapshot) error {
	if err := ensureProfileTOMLFile(homePath, "codex", "config.toml", snapshot.config, []byte("[mcp_servers]\n")); err != nil {
		return fmt.Errorf("config.toml: %w", err)
	}
	return nil
}

// codexRulesHomeRel is the execpolicy rules file's path relative to ~/.codex.
// Codex scans rules/ next to active config layers and its TUI auto-appends
// approval rules to this exact file, so sync must merge — never copy — it.
// See https://learn.chatgpt.com/docs/agent-configuration/rules.md.
const codexRulesHomeRel = "rules/default.rules"

// syncCodexRulesGenerated merges the freshly generated loom-managed rules
// block at srcFile into the user-owned rules file at dstFile. The managed
// block (delimited by generator.CodexRulesBeginMarker/EndMarker) is replaced
// in place; everything outside it — user-authored rules and Codex TUI
// auto-appended approvals — is preserved. A destination without markers keeps
// its full content below the newly prepended block. The file is never
// overwritten wholesale (same regen-survival contract as syncZedGenerated and
// the flightdeck hooks merge).
func syncCodexRulesGenerated(srcFile, dstFile string) error {
	generated, err := os.ReadFile(srcFile)
	if err != nil {
		return fmt.Errorf("read generated rules: %w", err)
	}
	existing, err := os.ReadFile(dstFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", dstFile, err)
	}

	merged, changed := MergeMarkerBlock(existing, generated,
		generator.CodexRulesBeginMarker, generator.CodexRulesEndMarker)
	if !changed {
		fmt.Printf("Codex rules already up-to-date in %s\n", dstFile)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dstFile), 0755); err != nil {
		return err
	}
	if err := writeFileAtomic(dstFile, merged, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dstFile, err)
	}
	fmt.Printf("Merged loom-managed rules block into %s\n", dstFile)
	return nil
}

// MergeMarkerBlock replaces the marker-delimited managed block inside
// existing with the generated block, preserving all content outside the
// markers. Layout invariants:
//   - existing empty/missing → output is the generated block alone.
//   - markers present in order → the span from the begin-marker line through
//     the end-marker line (inclusive) is replaced.
//   - markers absent or malformed (missing/end-before-begin) → the generated
//     block is prepended and the entire existing content is kept below it;
//     a stale half-marker is user content now, never silently deleted.
//
// Returns the merged bytes and whether they differ from existing.
func MergeMarkerBlock(existing, generated []byte, beginMarker, endMarker string) ([]byte, bool) {
	generated = bytes.TrimRight(generated, "\n")
	generated = append(generated, '\n')
	if len(bytes.TrimSpace(existing)) == 0 {
		return generated, !bytes.Equal(existing, generated)
	}

	begin := bytes.Index(existing, []byte(beginMarker))
	end := bytes.Index(existing, []byte(endMarker))
	var merged []byte
	if begin >= 0 && end > begin {
		// Extend end to the end of the marker line (or EOF).
		lineEnd := end + len(endMarker)
		if nl := bytes.IndexByte(existing[lineEnd:], '\n'); nl >= 0 {
			lineEnd += nl + 1
		} else {
			lineEnd = len(existing)
		}
		merged = append(merged, existing[:begin]...)
		merged = append(merged, generated...)
		merged = append(merged, existing[lineEnd:]...)
	} else {
		merged = append(merged, generated...)
		merged = append(merged, '\n')
		merged = append(merged, existing...)
	}
	return merged, !bytes.Equal(existing, merged)
}
