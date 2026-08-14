package backend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddDirToTar(t *testing.T) {
	// Create test directory structure.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755)
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)

	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0644)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("//js"), 0644)
	os.WriteFile(filepath.Join(dir, "test.pyc"), []byte("bytecode"), 0644)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	excludes := []string{
		".git",
		"node_modules",
	}

	var totalBytes int64
	err := addDirToTar(tw, dir, "/workspace/services/project", excludes, &totalBytes, MaxSyncBytes, &topFiles{})
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	// Extract and verify contents.
	files := extractTarEntries(t, buf.Bytes())

	// Should have: project dir, src dir, go.mod, src/main.go.
	// Should NOT have: .git/*, node_modules/*, test.pyc (binary excluded).
	if _, ok := files["workspace/services/project/go.mod"]; !ok {
		t.Error("expected go.mod in tar")
	}
	if _, ok := files["workspace/services/project/src/main.go"]; !ok {
		t.Error("expected src/main.go in tar")
	}
	if _, ok := files["workspace/services/project/.git/HEAD"]; ok {
		t.Error(".git should be excluded")
	}
	if _, ok := files["workspace/services/project/node_modules/pkg/index.js"]; ok {
		t.Error("node_modules should be excluded")
	}
	if _, ok := files["workspace/services/project/test.pyc"]; ok {
		t.Error(".pyc files should be excluded")
	}
}

func TestAddDirToTar_ExcludesAgentState(t *testing.T) {
	// Agent/editor tooling state (notably .claude/worktrees, each a full
	// source checkout) must not be synced into a build sandbox — it is never a
	// build input and routinely pushes the tar payload past the size cap.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.MkdirAll(filepath.Join(dir, ".claude", "worktrees", "wt-1", "cmd"), 0755)
	os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	os.MkdirAll(filepath.Join(dir, ".gemini"), 0755)

	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, ".claude", "worktrees", "wt-1", "cmd", "big.go"), []byte("package main // huge checkout"), 0644)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, ".codex", "auth.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, ".gemini", "config.toml"), []byte(""), 0644)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	if err := addDirToTar(tw, dir, "/workspace/services/project", defaultSyncExcludes, &totalBytes, MaxSyncBytes, &topFiles{}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	files := extractTarEntries(t, buf.Bytes())

	if _, ok := files["workspace/services/project/src/main.go"]; !ok {
		t.Error("expected real source src/main.go in tar")
	}
	for _, p := range []string{
		"workspace/services/project/.claude/worktrees/wt-1/cmd/big.go",
		"workspace/services/project/.claude/settings.json",
		"workspace/services/project/.codex/auth.json",
		"workspace/services/project/.gemini/config.toml",
	} {
		if _, ok := files[p]; ok {
			t.Errorf("agent state %q should be excluded from sync", p)
		}
	}
}

func TestAddDirToTar_MaxSizeExceeded(t *testing.T) {
	dir := t.TempDir()

	// Create a file larger than the limit.
	data := make([]byte, 1024)
	os.WriteFile(filepath.Join(dir, "big.txt"), data, 0644)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	err := addDirToTar(tw, dir, "/workspace", nil, &totalBytes, 512, &topFiles{})
	if err == nil {
		t.Error("expected error for exceeding max size")
	}
	tw.Close()
	gw.Close()
}

func TestAddDirToTar_MultipleSourceDirs(t *testing.T) {
	// Simulate syncing project + dep.
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "services", "myproject")
	libDir := filepath.Join(tmpDir, "libs", "mylib")

	os.MkdirAll(projectDir, 0755)
	os.MkdirAll(libDir, 0755)
	os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(libDir, "lib.go"), []byte("package mylib"), 0644)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	excludes := []string{".git"}
	var totalBytes int64

	dirs := []SyncDir{
		{LocalPath: projectDir, RemotePath: "/workspace/services/myproject"},
		{LocalPath: libDir, RemotePath: "/workspace/libs/mylib"},
	}

	for _, d := range dirs {
		err := addDirToTar(tw, d.LocalPath, d.RemotePath, excludes, &totalBytes, MaxSyncBytes, &topFiles{})
		if err != nil {
			t.Fatal(err)
		}
	}

	tw.Close()
	gw.Close()

	files := extractTarEntries(t, buf.Bytes())

	if _, ok := files["workspace/services/myproject/main.go"]; !ok {
		t.Error("expected main.go from project")
	}
	if _, ok := files["workspace/libs/mylib/lib.go"]; !ok {
		t.Error("expected lib.go from lib")
	}
}

func TestIsBinaryExcluded(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"main.go", false},
		{"lib.py", false},
		{"test.pyc", true},
		{"test.pyo", true},
		{"lib.so", true},
		{"lib.dylib", true},
		{"main.test", true},
		{"README.md", false},
	}

	for _, tt := range tests {
		got := isBinaryExcluded(tt.name)
		if got != tt.want {
			t.Errorf("isBinaryExcluded(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// extractTarEntries decompresses and extracts a tar.gz, returning a map of
// path→content for all regular files.
func extractTarEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()

	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	files := make(map[string]string)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			content, _ := io.ReadAll(tr)
			files[hdr.Name] = string(content)
		case tar.TypeDir:
			files[hdr.Name] = ""
		}
	}

	return files
}

// TestAddDirToTar_ExcludesCompiledBinaries pins the fix for the sync cap:
// `make build` drops extensionless Go binaries in the repo root, which are
// build outputs and dwarf the source tree. They must not reach the sandbox,
// while executable shell scripts (also +x) must.
func TestAddDirToTar_ExcludesCompiledBinaries(t *testing.T) {
	dir := t.TempDir()

	machO := append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 512)...)
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 512)...)
	if err := os.WriteFile(filepath.Join(dir, "loom"), machO, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-devbox"), elf, 0755); err != nil {
		t.Fatal(err)
	}
	// Executable, but a script: must survive.
	if err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// A binary magic in a NON-executable file is data, not a build output.
	if err := os.WriteFile(filepath.Join(dir, "fixture.bin"), machO, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	if err := addDirToTar(tw, dir, "/workspace", nil, &totalBytes, MaxSyncBytes, &topFiles{}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	files := extractTarEntries(t, buf.Bytes())
	for _, excluded := range []string{"workspace/loom", "workspace/mcp-devbox"} {
		if _, ok := files[excluded]; ok {
			t.Errorf("compiled binary %q should be excluded from sync", excluded)
		}
	}
	for _, kept := range []string{"workspace/build.sh", "workspace/fixture.bin", "workspace/main.go"} {
		if _, ok := files[kept]; !ok {
			t.Errorf("%q should be synced", kept)
		}
	}
}

func TestAddDirToTar_ExcludesPnpmStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frontend", ".pnpm-store", "v10"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend", ".pnpm-store", "v10", "blob"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend", "app.ts"), []byte("export {}"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	if err := addDirToTar(tw, dir, "/workspace", defaultSyncExcludes, &totalBytes, MaxSyncBytes, &topFiles{}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	files := extractTarEntries(t, buf.Bytes())
	if _, ok := files["workspace/frontend/.pnpm-store/v10/blob"]; ok {
		t.Error(".pnpm-store should be excluded from sync")
	}
	if _, ok := files["workspace/frontend/app.ts"]; !ok {
		t.Error("source file should be synced")
	}
}

// TestAddDirToTar_MaxSizeErrorNamesOffenders: the old error reported only a
// total, so finding what filled the payload meant walking the tree by hand.
func TestAddDirToTar_MaxSizeErrorNamesOffenders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hefty.bin"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	err := addDirToTar(tw, dir, "/workspace", nil, &totalBytes, 512, &topFiles{})
	tw.Close()
	gw.Close()

	if err == nil {
		t.Fatal("expected error for exceeding max size")
	}
	for _, want := range []string{"hefty.bin", "DEVBOX_SYNC_EXCLUDES", "DEVBOX_MAX_SYNC_SIZE_MB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestTopFiles_KeepsLargestInOrder(t *testing.T) {
	var top topFiles
	for i, f := range []struct {
		path string
		size int64
	}{
		{"a", 10}, {"b", 500}, {"c", 3}, {"d", 900}, {"e", 40}, {"f", 700}, {"g", 1},
	} {
		top.add(f.path, f.size)
		if len(top.files) > topFilesKept {
			t.Fatalf("after %d adds, kept %d files, want <= %d", i+1, len(top.files), topFilesKept)
		}
	}
	if got := top.files[0].path; got != "d" {
		t.Errorf("largest = %q, want \"d\"", got)
	}
	if got := top.files[1].path; got != "f" {
		t.Errorf("second = %q, want \"f\"", got)
	}
	if summary := top.summary(); !strings.Contains(summary, "d (") {
		t.Errorf("summary should name the largest file, got %q", summary)
	}
}
