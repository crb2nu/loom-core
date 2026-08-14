// ops_zed.go — Zed-specific sync operations.
//
// The zed profile stages a generated context_servers.json fragment in the
// repo (.zed/). Sync merges that fragment into the user-owned
// ~/.config/zed/settings.json instead of copying, because settings.json
// carries the user's editor configuration and possibly JSONC comments.
package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// zedLegacyHomeDir is the pre-2026-07 sync destination. Zed core never read
// the mcp.json emitted there; it is cleaned up on sync.
var zedLegacyHomeDir = filepath.Join("Library", "Application Support", "Zed")

// syncZedGenerated merges the generated context_servers fragment at srcFile
// into the Zed settings.json at dstFile. The settings file is created when
// missing; when present it is patched in place (comments and foreign keys
// preserved). An unparseable settings file aborts the sync — it is never
// overwritten wholesale.
func syncZedGenerated(srcFile, dstFile string) error {
	fragmentData, err := os.ReadFile(srcFile)
	if err != nil {
		return fmt.Errorf("read generated fragment: %w", err)
	}
	homeData, err := os.ReadFile(dstFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", dstFile, err)
	}

	merged, changed, err := MergeZedContextServers(homeData, fragmentData)
	if err != nil {
		return fmt.Errorf("merge Zed context servers into %s: %w", dstFile, err)
	}
	if !changed {
		fmt.Printf("Zed context servers already up-to-date in %s\n", dstFile)
		return nil
	}
	if err := writeFileAtomic(dstFile, merged, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dstFile, err)
	}
	fmt.Printf("Merged context servers into %s\n", dstFile)
	return nil
}

// cleanupZedLegacyGenerated removes stale mcp.json artifacts from the old
// emission paths: the repo staging dir (.zed/mcp.json) and the legacy home
// destination (~/Library/Application Support/Zed/mcp.json).
func cleanupZedLegacyGenerated(repoPath, homeDir string) {
	stale := []string{filepath.Join(repoPath, "mcp.json")}
	if homeDir != "" {
		stale = append(stale, filepath.Join(homeDir, zedLegacyHomeDir, "mcp.json"))
	}
	for _, path := range stale {
		if !Exists(path) {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not remove stale Zed mcp.json %s: %v\n", path, err)
		} else {
			fmt.Printf("Removed stale Zed mcp.json (Zed never reads it): %s\n", path)
		}
	}
}

// pullZedGenerated extracts the context_servers block from a home Zed
// settings.json (JSONC tolerated) and writes it as a repo staging fragment.
func pullZedGenerated(srcFile, dstFile string) error {
	homeData, err := os.ReadFile(srcFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcFile, err)
	}
	servers, err := zedExistingContextServers(homeData)
	if err != nil {
		return err
	}
	if servers == nil {
		return fmt.Errorf("%s has no %s key", srcFile, zedContextServersKey)
	}
	out, err := marshalOrderedSettings(map[string]json.RawMessage{
		zedContextServersKey: mustMarshal(rawMap(servers)),
	})
	if err != nil {
		return err
	}
	return writeFileAtomic(dstFile, out, 0o644)
}

// zedGeneratedInSync reports whether the home settings.json already contains
// the generated context server entries (used for drift status; the two files
// are never byte-identical by design).
func zedGeneratedInSync(repoFile, homeFile string) bool {
	fragmentData, err := os.ReadFile(repoFile)
	if err != nil {
		return false
	}
	homeData, err := os.ReadFile(homeFile)
	if err != nil {
		return false
	}
	_, changed, err := MergeZedContextServers(homeData, fragmentData)
	return err == nil && !changed
}

func mustMarshal(v any) json.RawMessage {
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}
