package backend

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// defaultSyncExcludes are patterns excluded from tar-pipe workspace sync.
// These match the exclude patterns from scripts/devbox-sync.sh.
var defaultSyncExcludes = []string{
	"._*",
	".git",
	"node_modules",
	// pnpm's content-addressed package store. Same category as node_modules
	// — a package manager's cache, never a build input — but project-local
	// rather than hoisted, so the node_modules rule misses it. It holds
	// ~55 MB under loom-core's HUD frontend.
	".pnpm-store",
	".devbox-build",
	"vendor",
	".direnv",
	".build",
	"__pycache__",
	".cache",
	".venv",
	".mypy_cache",
	".pytest_cache",
	".ruff_cache",
	".DS_Store",
	".pyc",
	"bin",
	".loom",
	".worktrees",
	// Agent/editor tooling state — never a build input, and the worktree
	// roots under these (e.g. .claude/worktrees, each a full source checkout)
	// routinely push the tar-pipe payload past the size cap. Excluding them
	// keeps the sync bounded regardless of how much agent state accumulates.
	".claude",
	".opencode",
	".codex",
	".gemini",
	".antigravity",
	".kilocode",
	".cursor",
	".aider.chat.history.md",
	".swiftpm",
	"xcuserdata",
	"dist",
	".next",
	".sandbox-policy.json",
	"coverage.out",
	"gosec-report.json",
	// Go/lint build caches that some projects keep project-local.
	".gocache",
	".go",
	".go-build",
	".gotmp",
	".golangci-lint-cache",
	// Temporary/generated directories.
	".tmp",
	"tmp",
}

// MaxSyncBytes is the default maximum uncompressed tar size (200 MB).
const MaxSyncBytes int64 = 200 * 1024 * 1024

// Deprecated: SyncWorkspace uses the legacy tar-pipe sync mode, which streams
// local directories into a pod via SPDY exec. This mode is being replaced by
// the sandbox.Controller unified interface with WebSocket exec support.
// Gate behind DEVBOX_SYNC_MODE=tar-pipe to use; will be removed in a future version.
//
// SyncWorkspace streams local directories into a running pod via exec.
// It creates a tar.gz archive spanning all dirs (each placed at its RemotePath)
// and pipes it into `tar xzf - -C /` inside the pod.
func (k *K8sBackend) SyncWorkspace(ctx context.Context, podName string, dirs []SyncDir, extraExcludes []string, maxBytes int64) error {
	if len(dirs) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = MaxSyncBytes
	}

	excludes := slices.Concat(defaultSyncExcludes, extraExcludes)

	// Create tar.gz in memory.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	var top topFiles
	for _, d := range dirs {
		if err := addDirToTar(tw, d.LocalPath, d.RemotePath, excludes, &totalBytes, maxBytes, &top); err != nil {
			tw.Close()
			gw.Close()
			return fmt.Errorf("tar %s: %w", d.LocalPath, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}

	// Pipe into pod via SPDY exec.
	return k.pipeTarIntoPod(ctx, podName, buf.Bytes())
}

// pipeTarIntoPod streams a tar.gz payload into a pod and extracts it.
func (k *K8sBackend) pipeTarIntoPod(ctx context.Context, podName string, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(k.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "devbox",
			Command:   []string{"tar", "xzf", "-", "-C", "/"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	// Tar-pipe sync always uses SPDY because stdin piping over WebSocket
	// requires bidirectional stream support that differs from exec semantics.
	executor, err := remotecommand.NewSPDYExecutor(k.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create sync executor: %w", err)
	}

	// Stdout MUST be drained. PodExecOptions requests a stdout stream
	// (Stdout: true), and stdin/stdout share the SPDY connection's
	// flow-control window. Without a Stdout reader here, the undrained
	// stdout stream fills the window once the payload exceeds the initial
	// flow-control budget, which back-pressures the stdin send so `tar xzf`
	// never receives the full archive, never exits, and StreamWithContext
	// deadlocks until the context timeout. Small payloads fit in the initial
	// window and slip through, so the bug only manifests on large workspaces
	// (e.g. loom-core), presenting as a ~timeout-long hang on otherwise
	// healthy nodes. io.Discard drains stdout without retaining it.
	var stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(payload),
		Stdout: io.Discard,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("sync workspace: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// topFilesKept is how many of the largest files an over-limit error names.
const topFilesKept = 5

type syncFile struct {
	path string
	size int64
}

// topFiles tracks the largest files seen during a walk so that an over-limit
// error can say what actually filled the payload. The previous error gave a
// total and nothing else, so diagnosing it meant re-walking the tree by hand
// to find the offenders.
type topFiles struct {
	files []syncFile
}

func (t *topFiles) add(path string, size int64) {
	if len(t.files) >= topFilesKept && size <= t.files[len(t.files)-1].size {
		return
	}
	t.files = append(t.files, syncFile{path: path, size: size})
	slices.SortFunc(t.files, func(a, b syncFile) int { return cmp.Compare(b.size, a.size) })
	if len(t.files) > topFilesKept {
		t.files = t.files[:topFilesKept]
	}
}

func (t *topFiles) summary() string {
	if len(t.files) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(t.files))
	for _, f := range t.files {
		parts = append(parts, fmt.Sprintf("%s (%.1f MB)", f.path, float64(f.size)/(1024*1024)))
	}
	return strings.Join(parts, ", ")
}

// addDirToTar walks localDir and adds files to the tar writer with paths
// rooted at remotePath. Skips excluded patterns and enforces a max size.
func addDirToTar(tw *tar.Writer, localDir, remotePath string, excludes []string, totalBytes *int64, maxBytes int64, top *topFiles) error {
	return filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}

		// Check if this entry matches an exclude pattern.
		base := d.Name()
		if matchesExclude(base, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary files by extension.
		if !d.IsDir() && isBinaryExcluded(base) {
			return nil
		}

		// Compute the path inside the tar.
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return nil
		}
		// Strip the leading "./" from remotePath join.
		tarPath := filepath.Join(remotePath, rel)
		// Ensure forward slashes for tar.
		tarPath = filepath.ToSlash(tarPath)
		// Strip leading "/" so tar paths are relative (extracted with -C /).
		tarPath = strings.TrimPrefix(tarPath, "/")

		info, err := d.Info()
		if err != nil {
			return nil
		}

		// Skip symlinks, devices, etc.
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}
		header.Name = tarPath

		if info.IsDir() {
			header.Name += "/"
			return tw.WriteHeader(header)
		}

		// Compiled executables are build outputs, never build inputs.
		if isCompiledBinary(path, info) {
			return nil
		}

		// Enforce max size.
		top.add(rel, info.Size())
		*totalBytes += info.Size()
		if *totalBytes > maxBytes {
			return fmt.Errorf("sync payload exceeds %d MB limit; largest files: %s — "+
				"exclude build artifacts with DEVBOX_SYNC_EXCLUDES, or raise DEVBOX_MAX_SYNC_SIZE_MB",
				maxBytes/(1024*1024), top.summary())
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return nil // skip unreadable files
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
}

func matchesExclude(base string, excludes []string) bool {
	for _, pattern := range excludes {
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := filepath.Match(pattern, base); err == nil && ok {
				return true
			}
			continue
		}
		if base == pattern {
			return true
		}
	}
	return false
}

// binaryMagics are the leading bytes of compiled executables.
//
// `go build` drops extensionless binaries into the repo root — `loom`,
// `mcp-devbox`, `loom-mills-operator`, `custom-server` — which no name- or
// extension-based rule can tell apart from source files. In loom-core they
// total ~200 MB on their own, so rather than being "caught by the size limit"
// they were the reason the limit tripped, and a routine `make build` silently
// broke every subsequent devbox_exec. Sniffing the magic number identifies
// them whatever they are named.
var binaryMagics = [][]byte{
	{0x7f, 'E', 'L', 'F'},    // ELF (Linux)
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit, little-endian
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32-bit, little-endian
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal ("fat") binary
	{'M', 'Z'},               // PE (Windows)
}

// isCompiledBinary reports whether path holds a compiled executable. Only
// files carrying an execute bit are opened, which keeps this to a handful of
// 4-byte reads per sync and means ordinary source files are never touched.
// Shell scripts are executable too, but start with "#!", so they are kept.
func isCompiledBinary(path string, info fs.FileInfo) bool {
	if info.Mode().Perm()&0o111 == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var hdr [4]byte
	n, err := f.Read(hdr[:])
	if err != nil && n == 0 {
		return false
	}
	for _, magic := range binaryMagics {
		if n >= len(magic) && bytes.Equal(hdr[:len(magic)], magic) {
			return true
		}
	}
	return false
}

// isBinaryExcluded returns true for file extensions that indicate compiled
// binaries or object files that should be excluded from workspace sync.
// Extensionless binaries are handled by isCompiledBinary, which sniffs
// content rather than names.
func isBinaryExcluded(name string) bool {
	ext := filepath.Ext(name)
	switch ext {
	case ".pyc", ".pyo", ".o", ".a", ".so", ".dylib", ".dll", ".exe", ".test":
		return true
	}
	return false
}
