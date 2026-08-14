package flexinfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardedHost is the FlexInfer proxy FQDN that must appear in exactly one
// place in production Go code: the DefaultProxyURL constant in defaults.go.
//
// Written as a runtime concatenation so this guard does not trip over its
// own source text -- scanFile skips defaults.go and this file by name, but
// keeping the literal un-spelled here means the guard stays correct even if
// the exemption list is edited.
var guardedHost = "flexinfer-proxy." + "flexinfer-system.svc.cluster.local"

// exemptFiles are the only production files allowed to spell guardedHost.
// Repo-root-relative, slash-separated.
var exemptFiles = map[string]bool{
	"pkg/flexinfer/defaults.go":            true,
	"pkg/flexinfer/defaults_guard_test.go": true,
}

// TestNoHardcodedFlexInferProxyURL fails when a Go file outside pkg/flexinfer
// hardcodes the FlexInfer proxy FQDN.
//
// This guard exists because of a specific, twice-repeated failure. The proxy
// URL and the embeddings-1536 alias were copy-pasted into pkg/pm,
// pkg/agentcontext (default chain, normalize block, and provider fallback),
// and pkg/codebase. When Morph was retired on 2026-08-13 the fix had to be
// applied per-copy, and two rounds (!1584, !1589) shipped before every copy
// was found -- each round looked complete because the copies it did find were
// all repaired. A grep-based guard turns "did we find them all?" from a
// judgment call into a build failure.
//
// Test files are not scanned. Several deliberately embed the literal URL:
// pkg/mills/pipeline and pkg/mills/audit match real observed error strings
// (pinning those to a constant would make the test tautological with the
// classifier), and pkg/pm/config_test.go asserts the resolved default
// independently, which is the check that would catch an unintended edit to
// defaults.go itself.
func TestNoHardcodedFlexInferProxyURL(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", ".worktrees", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if exemptFiles[rel] {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), guardedHost) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("FlexInfer proxy URL hardcoded outside pkg/flexinfer in %d file(s):\n  %s\n\n"+
			"Import github.com/crb2nu/loom/pkg/flexinfer and use flexinfer.DefaultProxyURL / "+
			"flexinfer.DefaultEmbedBaseURL instead. Keep the call site's own env chain; pass the "+
			"constant only as the terminal fallback. Doc comments should name the constant rather "+
			"than repeat the FQDN, so they cannot go stale independently.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestDefaultsAreSelfConsistent pins the relationships between the exported
// defaults. DefaultEmbedBaseURL is a const expression over DefaultProxyURL, so
// this mainly guards against someone flattening it back into a literal that
// then drifts from the proxy URL.
func TestDefaultsAreSelfConsistent(t *testing.T) {
	if DefaultProxyURL != "http://"+guardedHost {
		t.Errorf("DefaultProxyURL = %q, want http://%s", DefaultProxyURL, guardedHost)
	}
	if strings.HasSuffix(DefaultProxyURL, "/") {
		t.Errorf("DefaultProxyURL = %q must not have a trailing slash", DefaultProxyURL)
	}
	if want := DefaultProxyURL + "/v1"; DefaultEmbedBaseURL != want {
		t.Errorf("DefaultEmbedBaseURL = %q, want %q", DefaultEmbedBaseURL, want)
	}
	// The dimension in the alias is contractual: Qdrant collections written
	// by agent-context, pm, and codebase are all sized 1536.
	if DefaultEmbedModel != "embeddings-1536" {
		t.Errorf("DefaultEmbedModel = %q, want embeddings-1536 "+
			"(collection vector size is 1536; changing the alias to a "+
			"differently-sized model causes a dimension-mismatch storm)", DefaultEmbedModel)
	}
	if !strings.HasPrefix(RetiredMorphModel, RetiredMorphModelPrefix) {
		t.Errorf("RetiredMorphModel %q must match RetiredMorphModelPrefix %q",
			RetiredMorphModel, RetiredMorphModelPrefix)
	}
	if !strings.Contains(RetiredMorphBaseURL, RetiredMorphHost) {
		t.Errorf("RetiredMorphBaseURL %q must contain RetiredMorphHost %q",
			RetiredMorphBaseURL, RetiredMorphHost)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
