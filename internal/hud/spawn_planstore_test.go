package hud

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolvePlanStoreWSURL covers the default, override, and disable paths.
func TestResolvePlanStoreWSURL(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("SPAWN_PLAN_STORE_WS_URL", "")
		if got := resolvePlanStoreWSURL(); got != defaultPlanStoreWSURL {
			t.Fatalf("default = %q, want %q", got, defaultPlanStoreWSURL)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("SPAWN_PLAN_STORE_WS_URL", "ws://example:9000/ws")
		if got := resolvePlanStoreWSURL(); got != "ws://example:9000/ws" {
			t.Fatalf("override = %q", got)
		}
	})
	for _, off := range []string{"disabled", "OFF", "Disabled"} {
		t.Run("disabled/"+off, func(t *testing.T) {
			t.Setenv("SPAWN_PLAN_STORE_WS_URL", off)
			if got := resolvePlanStoreWSURL(); got != "" {
				t.Fatalf("disabled(%q) = %q, want empty", off, got)
			}
		})
	}
}

// TestResolveSpawnLoomImage covers default + env override.
func TestResolveSpawnLoomImage(t *testing.T) {
	t.Setenv("SPAWN_LOOM_IMAGE", "")
	if got := resolveSpawnLoomImage(); got != defaultSpawnLoomImage {
		t.Fatalf("default = %q, want %q", got, defaultSpawnLoomImage)
	}
	t.Setenv("SPAWN_LOOM_IMAGE", "registry.harbor.lan/mcp/loom-core:20260625-013914")
	if got := resolveSpawnLoomImage(); got != "registry.harbor.lan/mcp/loom-core:20260625-013914" {
		t.Fatalf("override = %q", got)
	}
}

// TestLoomBinaryCopyLines: COPY layer present by default, gated off when
// the feature is disabled, and respects the image override.
func TestLoomBinaryCopyLines(t *testing.T) {
	t.Run("default-on", func(t *testing.T) {
		t.Setenv("SPAWN_PLAN_STORE_WS_URL", "")
		t.Setenv("SPAWN_LOOM_IMAGE", "")
		got := loomBinaryCopyLines()
		want := "COPY --from=" + defaultSpawnLoomImage + " /usr/local/bin/loom /usr/local/bin/loom"
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("disabled-empty", func(t *testing.T) {
		t.Setenv("SPAWN_PLAN_STORE_WS_URL", "disabled")
		if got := loomBinaryCopyLines(); got != "" {
			t.Fatalf("disabled should suppress COPY, got %q", got)
		}
	})
}

// TestGenerateDockerfile_BundlesLoom asserts the generated Dockerfile copies
// the loom binary before the USER switch, for every agent type.
func TestGenerateDockerfile_BundlesLoom(t *testing.T) {
	t.Setenv("SPAWN_PLAN_STORE_WS_URL", "")
	t.Setenv("SPAWN_LOOM_IMAGE", "")
	o := &SpawnOrchestrator{}
	for _, agentType := range []string{"codex", "claude-code", "gemini"} {
		df, err := o.generateDockerfile(t.TempDir(), agentType)
		if err != nil {
			t.Fatalf("%s: %v", agentType, err)
		}
		s := string(df)
		copyIdx := strings.Index(s, "COPY --from="+defaultSpawnLoomImage+" /usr/local/bin/loom")
		userIdx := strings.Index(s, "USER agent")
		if copyIdx < 0 {
			t.Fatalf("%s: missing loom COPY:\n%s", agentType, s)
		}
		if userIdx < 0 || copyIdx > userIdx {
			t.Fatalf("%s: loom COPY must precede USER agent:\n%s", agentType, s)
		}
	}
}

// TestLoomMCPServerTOML asserts the codex stanza shape and that the URL round
// trips through the args array.
func TestLoomMCPServerTOML(t *testing.T) {
	url := "ws://mcp-agent-context.loom-hub.svc.cluster.local:8080/ws"
	got := loomMCPServerTOML(url)
	for _, want := range []string{
		"[mcp_servers.loom]",
		`command = "loom"`,
		`args = ["proxy","--ws-backend","` + url + `"]`,
		"startup_timeout_sec = 30",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("toml missing %q:\n%s", want, got)
		}
	}
}

// TestLoomMCPServerJSON asserts the claude/gemini JSON shape parses and carries
// the proxy argv.
func TestLoomMCPServerJSON(t *testing.T) {
	url := "ws://host:8080/ws"
	doc := loomMCPServerJSON(url)

	var parsed struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("invalid json %q: %v", doc, err)
	}
	loom, ok := parsed.MCPServers["loom"]
	if !ok {
		t.Fatalf("no loom server in %q", doc)
	}
	if loom.Command != "loom" {
		t.Fatalf("command = %q", loom.Command)
	}
	want := []string{"proxy", "--ws-backend", url}
	if strings.Join(loom.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v want %v", loom.Args, want)
	}

	// Inner fragment embeds into an existing object and yields valid JSON.
	combined := `{"permissions":{"allow_all":true},` + loomMCPServerJSONInner(url) + `}`
	if err := json.Unmarshal([]byte(combined), &parsed); err != nil {
		t.Fatalf("inner fragment produced invalid json %q: %v", combined, err)
	}
	if _, ok := parsed.MCPServers["loom"]; !ok {
		t.Fatalf("inner fragment lost loom server: %q", combined)
	}
}
