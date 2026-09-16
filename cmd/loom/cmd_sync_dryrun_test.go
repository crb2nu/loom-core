package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// snapshotTree returns every path under root, relative and sorted, so a test
// can assert that a command wrote nothing.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// TestSyncDryRunWritesNothing is the regression test for the defect where
// `loom sync <profile> --dry-run` silently synced to home. The flag was
// declared and unit-tested for registration, but runSyncCmd only ever passed
// it to the --all-projects propagation — the SyncToHome path ignored it, so a
// maintainer probing "what would change?" got a real write instead.
func TestSyncDryRunWritesNothing(t *testing.T) {
	// Read the canonical registries before chdir'ing away. Without a real
	// registry the sync bails on "missing or empty mcp_servers section" and
	// never reaches the write path — which would make this test pass against
	// the very bug it exists to catch.
	registries := map[string][]byte{}
	for _, rel := range []string{"mcp/context/registry.yaml", "mcp/context/skills-registry.yaml"} {
		body, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read canonical %s: %v", rel, err)
		}
		registries[rel] = body
	}

	home := t.TempDir()
	repo := t.TempDir()
	for rel, body := range registries {
		dst := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", home)
	t.Chdir(repo)

	// Pre-seed a home config so the check fails loudly if sync overwrites it,
	// not just if it creates something new.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(codexDir, "config.toml")
	const sentinelBody = "# sentinel — a dry run must not replace this\n"
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o644); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, home)

	cmd := newSyncCmd()
	cmd.SetArgs([]string{"codex", "--dry-run"})
	cmd.SetOut(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync codex --dry-run: %v", err)
	}

	after := snapshotTree(t, home)
	if len(before) != len(after) {
		t.Fatalf("dry run changed the home tree:\n before: %v\n after:  %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("dry run changed the home tree:\n before: %v\n after:  %v", before, after)
		}
	}

	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(body) != sentinelBody {
		t.Fatalf("dry run rewrote %s:\n%s", sentinel, body)
	}
}
