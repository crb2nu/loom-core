package hud

// This file owns Dockerfile and runtime-image generation for spawned agents.
// Keep image construction helpers cohesive with their deterministic build tags.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/internal/devbox/detect"
)

// generateDockerfile builds a lean agent runtime image. Spawned agents get
// project source through the runtime workspace init container, and quality
// gates run in later Mills stages, so the spawn image should not install
// project-specific lint/security toolchains during planning/implementation.
func (o *SpawnOrchestrator) generateDockerfile(projectDir, agentType string) ([]byte, error) {
	if _, err := detect.Fingerprint(projectDir); err != nil {
		o.logger.Warn("project detection failed; using generic agent runtime image", "dir", projectDir, "error", err)
	}
	df := agentRuntimeDockerfile()
	// Bundle the loom binary so the spawned agent can reach the in-cluster
	// Plan Store via `loom proxy --ws-backend`. COPY --from runs as root,
	// before the USER agent switch. Skipped when plan-store wiring is disabled.
	if loomLines := loomBinaryCopyLines(); loomLines != "" {
		df = append(df, []byte("\n"+loomLines+"\n")...)
	}
	if cliLines := agentCLIInstallLines(agentType); cliLines != "" {
		df = append(df, []byte("\n"+cliLines+"\n")...)
	}
	// Switch to the non-root agent user *after* the CLI install layer so
	// `npm install -g` can write to /usr/local/lib/node_modules. The
	// runtime CMD in agentRuntimeDockerfile keeps the pod alive as the
	// agent user; injectAgentConfig + claude exec all run as uid 1000.
	df = append(df, []byte(agentRuntimeUserSuffix())...)
	return df, nil
}

// agentRuntimeDockerfile returns the shared base for spawned agent pods.
//
// Why the non-root user: the claude CLI refuses `--dangerously-skip-permissions`
// when the effective uid is 0 ("cannot be used with root/sudo privileges for
// security reasons"), and Mills launches every claude-code spawn with that
// flag. Spawned agents must run as a non-root user with a writable $HOME so
// claude can stash its `.claude.json` profile.
//
// The base stays as root so the agent CLI install layer
// (agentCLIInstallLines) can run `npm install -g`, which needs to write
// to /usr/local/lib/node_modules. The trailing USER agent + HOME switch is
// appended by generateDockerfile *after* the install layer.
// go/gofmt are symlinked into /usr/local/bin because the golang image only
// exposes them via its ENV PATH (/usr/local/go/bin). Vendor agent CLIs
// sanitize or rebuild the environment for the shells they spawn, so tools
// that exist only through image ENV vanish from the agent's PATH — the
// escalation #272–#278 spawns reported "go is not installed" while
// /usr/local/go/bin/go sat right there (#277 found it by absolute path).
// /usr/local/bin is on every default PATH, ENV or not; the harvester-vm
// installer (agentCLIInstallShell's ensureGo) already relies on the same
// symlink trick.
func agentRuntimeDockerfile() []byte {
	return []byte(fmt.Sprintf(`FROM golang:%s-alpine
RUN apk add --no-cache ca-certificates git make bash curl nodejs npm python3 \
 && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
 && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
 && adduser -D -u 1000 -h /home/agent agent \
 && mkdir -p /workspace /opt/loom \
 && chown -R agent:agent /workspace /home/agent /opt/loom
WORKDIR /workspace
CMD ["sleep", "infinity"]
`, goVersion))
}

// agentRuntimeUserSuffix returns the trailing Dockerfile lines that flip
// the image from root to the agent user. It is appended *after* the agent
// CLI install layer (which needs root for `npm install -g`).
func agentRuntimeUserSuffix() string {
	return "USER agent\nENV HOME=/home/agent\n"
}

// loomBinaryCopyLines returns the Dockerfile COPY layer that bundles the loom
// binary from the loom-core image into the spawn-runtime image. The binary
// powers the `loom proxy --ws-backend` bridge that injectAgentConfig wires the
// spawned agent's MCP client to. Returns "" when plan-store wiring is disabled
// (SPAWN_PLAN_STORE_WS_URL=disabled), so the loom image is not pulled at build
// time when the feature is off.
func loomBinaryCopyLines() string {
	if resolvePlanStoreWSURL() == "" {
		return ""
	}
	return fmt.Sprintf("COPY --from=%s /usr/local/bin/loom /usr/local/bin/loom", resolveSpawnLoomImage())
}

// loomMCPProxyArgs is the argv the spawned agent's MCP client launches to bridge
// to the in-cluster Plan Store: `loom proxy --ws-backend <ws-url>`.
func loomMCPProxyArgs(wsURL string) []string {
	return []string{"proxy", "--ws-backend", wsURL}
}

// loomMCPServerTOML renders the codex config.toml [mcp_servers.loom] stanza.
// Values are JSON-marshaled so the (controlled) URL is always validly quoted.
func loomMCPServerTOML(wsURL string) string {
	args, _ := json.Marshal(loomMCPProxyArgs(wsURL))
	var b strings.Builder
	b.WriteString("[mcp_servers.loom]\n")
	b.WriteString("command = \"loom\"\n")
	b.WriteString(fmt.Sprintf("args = %s\n", string(args)))
	b.WriteString("startup_timeout_sec = 30\n")
	return b.String()
}

// loomMCPServerJSONInner renders the `"mcpServers":{...}` fragment (no outer
// braces) for embedding into an existing JSON settings object (e.g. gemini).
func loomMCPServerJSONInner(wsURL string) string {
	server := map[string]any{
		"loom": map[string]any{
			"command": "loom",
			"args":    loomMCPProxyArgs(wsURL),
		},
	}
	inner, _ := json.Marshal(server)
	return `"mcpServers":` + string(inner)
}

// loomMCPServerJSON renders a complete `{"mcpServers":{...}}` document
// (e.g. claude-code project .mcp.json).
func loomMCPServerJSON(wsURL string) string {
	return "{" + loomMCPServerJSONInner(wsURL) + "}"
}

func agentRuntimeBuildTag(agentType string, dockerfile []byte) string {
	sum := sha256.Sum256(dockerfile)
	safeAgent := sanitizeImageTagComponent(agentType)
	if safeAgent == "" {
		safeAgent = "agent"
	}
	return fmt.Sprintf("spawn-runtime-%s:%x", safeAgent, sum[:8])
}

func sanitizeImageTagComponent(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
