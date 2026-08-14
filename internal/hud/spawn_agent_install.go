package hud

// This file owns agent CLI installation and cluster authentication material.
// It keeps credential projection and pinned installer generation together.

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/spawn"
)

// resolveAuthMode returns the cluster-credential path the spawn will use.
// Describes the *configured* auth path — the actual runtime fallback
// (e.g., an absent setup token) is reflected in pod telemetry, not here.
//
//   - claude-code, codex → cluster_oauth (Claude receives a native setup-token
//     env var; Codex mounts auth.json)
//   - gemini             → cluster_service_account (SA JSON mount from
//     cluster-agent-api-keys; falls through to GEMINI_API_KEY env if the
//     SA JSON key is absent)
//
// Reporting cluster_oauth here reflects operator intent; a follow-up slice can
// add pod-side AuthMode reporting for the runtime-actual value.
func resolveAuthMode(agentType string) spawn.AuthMode {
	switch agentType {
	case "gemini":
		return spawn.AuthModeClusterServiceAccount
	case "claude-code", "codex":
		return spawn.AuthModeClusterOAuth
	default:
		return ""
	}
}

// agentSecretEnvVars returns K8s secret env vars for the given agent type.
// Sources credentials from the cluster-scoped secret (ClusterAgentAPIKeysSecret)
// so pods never read the developer's Mac Keychain state.
func agentSecretEnvVars(agentType string) []backend.SecretEnvVar {
	secretName := ClusterAgentAPIKeysSecret
	switch agentType {
	case "claude-code":
		// CLAUDE_CODE_OAUTH_TOKEN is the officially documented headless auth path
		// (https://code.claude.com/docs/en/authentication, "Long-Lived Authentication
		// Token" section). Operators generate a 1-year token via `claude setup-token`
		// on a machine with an active Pro/Max/Team subscription, then set it on the
		// cluster-agent-auth secret under the claude-oauth-token key. API-key
		// fallback is opt-in only: without a valid setup token the CLI must fail
		// visibly, rather than silently charging the API-billing credential.
		vars := []backend.SecretEnvVar{
			{Name: "CLAUDE_CODE_OAUTH_TOKEN", SecretName: ClusterAgentAuthSecret, SecretKey: "claude-oauth-token"},
		}
		if os.Getenv("LOOM_SPAWN_CLAUDE_API_KEY_FALLBACK") == "1" {
			vars = append(vars, backend.SecretEnvVar{Name: "ANTHROPIC_API_KEY", SecretName: secretName, SecretKey: "ANTHROPIC_API_KEY"})
		}
		return vars
	case "codex":
		// Intentionally do NOT wire OPENAI_API_KEY / CODEX_API_KEY from the
		// cluster-agent-api-keys secret. The codex CLI treats those env vars
		// as an override that takes precedence over ~/.codex/auth.json, so a
		// placeholder value ("PLACEHOLDER" in clusters without a static API
		// key) silently breaks every codex spawn with `401 Unauthorized:
		// Incorrect API key provided: PLACEHOLDER`. Mills' canary kept
		// reaching status=completed with turn_count=1 and an empty MR
		// because every codex LLM call failed at the auth gate before the
		// agent could modify the workspace; only the diagnostic JSONL log
		// in MR !543 made the actual 401 stream visible.
		//
		// The OAuth flow via the mounted auth.json (`auth_mode: "chatgpt"`)
		// is the documented headless path for codex CLI 0.130+. Operators
		// who genuinely use API-key auth should put the key in auth.json's
		// `OPENAI_API_KEY` field (codex still honors it there) instead of
		// going through this env path.
		return nil
	case "gemini":
		return []backend.SecretEnvVar{
			{Name: "GEMINI_API_KEY", SecretName: secretName, SecretKey: "GEMINI_API_KEY"},
			{Name: "GOOGLE_API_KEY", SecretName: secretName, SecretKey: "GEMINI_API_KEY"},
		}
	default:
		return nil
	}
}

// agentSecretMounts returns K8s secret volume mounts for credential files
// that the agent CLI reads from disk. All sources come from cluster-scoped
// secrets; no developer-Mac state is ever mounted.
//
//   - Codex: mounts cluster-agent-auth's codex-auth-json at
//     /root/.codex/auth.json when populated. Codex CLI reads this file
//     natively and falls back to $OPENAI_API_KEY.
//   - Gemini: mounts the service-account JSON from cluster-agent-api-keys
//     at /root/.gcp/sa.json. GOOGLE_APPLICATION_CREDENTIALS env pointing
//     at the file is set by runSpawn.
//
// All mounts are k8s-Optional (see buildPodSpec): a missing secret or key
// results in an absent file. Claude uses its native setup-token environment
// variable rather than a mounted OAuth file.
func agentSecretMounts(agentType string) []backend.SecretMount {
	switch agentType {
	case "codex":
		// Stage the OAuth file at /home/agent/.codex.auth/ (NOT
		// /home/agent/.codex/) so the injected config.toml and the
		// symlink to auth.json can coexist in /home/agent/.codex/
		// without the secret-volume mount shadowing injectAgentConfig's
		// writes. injectAgentConfig creates /home/agent/.codex/auth.json
		// as a symlink to the staging mount so kubelet-propagated secret
		// updates reach the CLI transparently.
		return []backend.SecretMount{
			{
				SecretName: ClusterAgentAuthSecret,
				MountPath:  AgentHomeDir + "/.codex.auth",
				Items: []backend.SecretMountItem{
					{Key: "codex-auth-json", Path: "auth.json"},
				},
			},
		}
	case "gemini":
		return []backend.SecretMount{
			{
				SecretName: ClusterAgentAPIKeysSecret,
				MountPath:  GeminiSAMountPath,
				Items: []backend.SecretMountItem{
					{Key: GeminiSAKeyName, Path: GeminiSAFilename},
				},
			},
		}
	default:
		return nil
	}
}

// codexVersionPattern bounds the SPAWN_CODEX_VERSION override to an npm
// semver / prerelease-shaped token. resolveCodexVersion interpolates the value
// into a root `npm install -g @openai/codex@<version>` at image-build time, so
// it must never carry shell metacharacters; an override that fails this pattern
// is ignored in favor of the compiled default rather than risking injection.
var codexVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// resolveCodexVersion returns the @openai/codex version to install in the
// spawn-runtime image / on the harvester-vm substrate. SPAWN_CODEX_VERSION lets
// operators clear a future model-version gate (the HTTP 400 "requires a newer
// version of Codex" class that broke every gpt-5.6 spawn on 2026-07-18) WITHOUT
// a loom-core rebuild — the same env-flip-not-rebuild resilience resolveCodexModel
// gives the model id. Because the value is baked into the Dockerfile, changing
// it changes agentRuntimeBuildTag's hash and triggers a fresh spawn-runtime
// image build. An unset or malformed override falls back to the compiled-in
// codexVersion default (see codexVersionPattern for why malformed is rejected).
func resolveCodexVersion() string {
	if v := strings.TrimSpace(os.Getenv("SPAWN_CODEX_VERSION")); v != "" && codexVersionPattern.MatchString(v) {
		return v
	}
	return codexVersion
}

// agentCLIInstallLines returns Dockerfile RUN lines to install the agent CLI
// with pinned versions for reproducible builds.
func agentCLIInstallLines(agentType string) string {
	const ensureNPM = `RUN if ! command -v npm >/dev/null 2>&1; then \
    if command -v apk >/dev/null 2>&1; then \
      apk add --no-cache nodejs npm; \
    elif command -v apt-get >/dev/null 2>&1; then \
      apt-get update && apt-get install -y --no-install-recommends nodejs npm && rm -rf /var/lib/apt/lists/*; \
    else \
      echo "npm is required to install agent CLI" >&2; exit 1; \
    fi; \
  fi`

	switch agentType {
	case "claude-code":
		return fmt.Sprintf("%s\nRUN npm install -g @anthropic-ai/claude-code@%s", ensureNPM, claudeCodeVersion)
	case "codex":
		return fmt.Sprintf("%s\nRUN npm install -g @openai/codex@%s", ensureNPM, resolveCodexVersion())
	case "gemini":
		return fmt.Sprintf("%s\nRUN npm install -g @google/gemini-cli@%s", ensureNPM, geminiVersion)
	default:
		return ""
	}
}

// agentCLIInstallShell returns a guarded, idempotent shell snippet that ensures
// the Go toolchain and the agent CLI are installed on a substrate whose Build
// does not bake them into a runtime image. The K8s backend gets Go from its
// golang base image (agentRuntimeDockerfile) and the CLI from agentCLIInstallLines
// (Dockerfile RUN) at Build time; the harvester-vm backend has a no-op Build
// against one stock Ubuntu cloud image, so the orchestrator passes this snippet
// as StartOpts.AgentCLIInstallCmd and the VM backend runs it over SSH at Start.
// Versions stay pinned to the same consts as agentCLIInstallLines + the
// spawn-runtime image (goVersion). The `command -v <tool>` guards make each
// install a fast no-op when the tool is already present (curated base image or
// reused VM), and it uses sudo because the VM's SSH user installs to system
// prefixes (/usr/local, the npm prefix). Returns "" for unknown agent types
// (the VM backend then skips install entirely).
func agentCLIInstallShell(agentType string) string {
	// DPkg::Lock::Timeout makes apt WAIT for the dpkg lock rather than failing
	// instantly. On the harvester-vm path this snippet runs over SSH at Start,
	// racing cloud-init's first-boot `apt-get install qemu-guest-agent`, which
	// holds /var/lib/dpkg/lock-frontend; without the wait the install aborts with
	// `Could not get lock /var/lib/dpkg/lock-frontend` (Mills A2 probe 2026-06-18).
	// Harmless on the Dockerfile/k8s path (agentCLIInstallLines has its own
	// build-isolated ensureNPM with no lock contention).
	const ensureNPM = `if ! command -v npm >/dev/null 2>&1; then sudo apt-get -o DPkg::Lock::Timeout=300 update && sudo DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y nodejs npm; fi`
	// ensureGo installs the pinned Go toolchain when absent so a Mills implement
	// spawn on the harvester-vm substrate can `go build`/`go test` its own Go
	// changes — the same self-verify the k8s backend gets for free from its
	// golang base image (agentRuntimeDockerfile). The stock Ubuntu cloud image
	// boots with no Go, so without this a Go spawn hits "go: not found" (the
	// explicit out-of-scope follow-up from the k8s build-env fix, !843).
	//
	// Installs the official linux tarball to /usr/local/go and symlinks
	// go/gofmt into /usr/local/bin (on the agent's default PATH for later
	// execs). The pinned goVersion matches go.mod + CI so `go build` never
	// triggers a GOTOOLCHAIN auto-download. The `command -v go` guard makes it a
	// fast no-op when Go is already present (curated image or reused VM). curl
	// is bootstrapped the same way ensureNPM bootstraps npm, with the same
	// DPkg::Lock::Timeout wait against cloud-init's racing first-boot apt.
	// dpkg --print-architecture yields Go's arch token directly (amd64/arm64).
	ensureGo := fmt.Sprintf(`if ! command -v go >/dev/null 2>&1; then if ! command -v curl >/dev/null 2>&1; then sudo apt-get -o DPkg::Lock::Timeout=300 update && sudo DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y curl ca-certificates; fi; goarch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"; curl -fsSL "https://go.dev/dl/go%s.linux-${goarch}.tar.gz" -o /tmp/loom-go.tgz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/loom-go.tgz && sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go && sudo ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt && rm -f /tmp/loom-go.tgz; fi`, goVersion)
	switch agentType {
	case "claude-code":
		return fmt.Sprintf(`%s; if ! command -v claude >/dev/null 2>&1; then %s; sudo npm install -g @anthropic-ai/claude-code@%s; fi`, ensureGo, ensureNPM, claudeCodeVersion)
	case "codex":
		return fmt.Sprintf(`%s; if ! command -v codex >/dev/null 2>&1; then %s; sudo npm install -g @openai/codex@%s; fi`, ensureGo, ensureNPM, resolveCodexVersion())
	case "gemini":
		return fmt.Sprintf(`%s; if ! command -v gemini >/dev/null 2>&1; then %s; sudo npm install -g @google/gemini-cli@%s; fi`, ensureGo, ensureNPM, geminiVersion)
	default:
		return ""
	}
}
