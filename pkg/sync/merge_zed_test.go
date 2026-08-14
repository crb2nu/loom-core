package sync

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

const zedTestFragment = `{
  "context_servers": {
    "loom": {
      "command": "/opt/loom/bin/loom",
      "args": ["proxy", "--agent-hint", "zed"],
      "timeout": 600
    }
  }
}`

func zedServers(t *testing.T, merged []byte) map[string]map[string]any {
	t.Helper()
	var top struct {
		ContextServers map[string]map[string]any `json:"context_servers"`
	}
	if err := json.Unmarshal(merged, &top); err != nil {
		t.Fatalf("merged output is not valid JSON: %v\n%s", err, merged)
	}
	if top.ContextServers == nil {
		t.Fatalf("merged output has no context_servers key:\n%s", merged)
	}
	return top.ContextServers
}

func TestMergeZedContextServers_CreatesFileWhenMissing(t *testing.T) {
	merged, changed, err := MergeZedContextServers(nil, []byte(zedTestFragment))
	if err != nil {
		t.Fatalf("MergeZedContextServers: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for empty home settings")
	}
	servers := zedServers(t, merged)
	if servers["loom"]["command"] != "/opt/loom/bin/loom" {
		t.Errorf("loom command = %v, want /opt/loom/bin/loom", servers["loom"]["command"])
	}
}

func TestMergeZedContextServers_MigratesLegacyNestedEntry(t *testing.T) {
	// The pre-2026-07 hand-maintained shape: nested command object (dead in
	// current Zed) plus a loom-zed extension settings block.
	home := []byte(`{
  "theme": "One Dark",
  "context_servers": {
    "loom": {
      "command": {
        "path": "/old/bin/loom",
        "arguments": ["proxy"]
      },
      "settings": {
        "download": {"repo": "crb2nu/loom-core"}
      }
    }
  }
}`)
	merged, changed, err := MergeZedContextServers(home, []byte(zedTestFragment))
	if err != nil {
		t.Fatalf("MergeZedContextServers: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when migrating legacy entry")
	}
	servers := zedServers(t, merged)
	loom := servers["loom"]
	if loom["command"] != "/opt/loom/bin/loom" {
		t.Errorf("command = %v, want flat string", loom["command"])
	}
	if _, hasSettings := loom["settings"]; hasSettings {
		t.Error("legacy extension settings block should be dropped from the migrated entry")
	}
	// Other top-level keys are preserved.
	if !strings.Contains(string(merged), `"theme"`) {
		t.Errorf("theme key lost:\n%s", merged)
	}
}

func TestMergeZedContextServers_PreservesForeignServersAndComments(t *testing.T) {
	home := []byte(`// Zed settings
// with a second comment line
{
  "context_servers": {
    "github": {
      "command": "github-mcp",
      "args": []
    }
  },
  "agent": {"dock": "right"}
}`)
	merged, changed, err := MergeZedContextServers(home, []byte(zedTestFragment))
	if err != nil {
		t.Fatalf("MergeZedContextServers: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when adding loom entry")
	}
	out := string(merged)
	if !strings.Contains(out, "// Zed settings") || !strings.Contains(out, "second comment line") {
		t.Errorf("comments not preserved:\n%s", out)
	}
	servers := zedServers(t, mustStandardizeForTest(t, merged))
	if servers["github"]["command"] != "github-mcp" {
		t.Errorf("foreign github server modified: %v", servers["github"])
	}
	if servers["loom"]["command"] != "/opt/loom/bin/loom" {
		t.Errorf("loom entry missing/incorrect: %v", servers["loom"])
	}
	if !strings.Contains(out, `"agent"`) {
		t.Errorf("unrelated top-level key lost:\n%s", out)
	}
}

func TestMergeZedContextServers_NoChangeWhenInSync(t *testing.T) {
	home := []byte(`{
  "context_servers": {
    "loom": {
      "command": "/opt/loom/bin/loom",
      "args": ["proxy", "--agent-hint", "zed"],
      "timeout": 600
    }
  }
}`)
	merged, changed, err := MergeZedContextServers(home, []byte(zedTestFragment))
	if err != nil {
		t.Fatalf("MergeZedContextServers: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false for in-sync settings, got:\n%s", merged)
	}
}

func TestMergeZedContextServers_CarriesDisabledFlag(t *testing.T) {
	home := []byte(`{
  "context_servers": {
    "loom": {
      "enabled": false,
      "command": "/stale/loom",
      "args": ["proxy"]
    }
  }
}`)
	merged, _, err := MergeZedContextServers(home, []byte(zedTestFragment))
	if err != nil {
		t.Fatalf("MergeZedContextServers: %v", err)
	}
	servers := zedServers(t, merged)
	if enabled, ok := servers["loom"]["enabled"].(bool); !ok || enabled {
		t.Errorf("user's enabled:false should survive sync, got %v", servers["loom"]["enabled"])
	}
	if servers["loom"]["command"] != "/opt/loom/bin/loom" {
		t.Errorf("command should still be updated, got %v", servers["loom"]["command"])
	}
}

func TestMergeZedContextServers_RefusesUnparseableSettings(t *testing.T) {
	home := []byte(`{"context_servers": THIS IS NOT JSON`)
	if _, _, err := MergeZedContextServers(home, []byte(zedTestFragment)); err == nil {
		t.Fatal("expected error for unparseable settings.json; must never overwrite the user's file")
	}
}

// mustStandardizeForTest converts JSONC output back to plain JSON so the
// structural assertions can parse it.
func mustStandardizeForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	std, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	return std
}
