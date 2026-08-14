// proxy_repo_config.go — repo-local proxy overrides (.loom/proxy.yaml).
//
// Platforms whose MCP config is home-level (Antigravity reads a single
// ~/.gemini/config/mcp_config.json) cannot vary `loom proxy` flags per
// repository, so a repo that needs a different tool profile (e.g.
// icc-project-workspaces needs icc-core while every other repo keeps
// antigravity-core) has no vendor-side hook. The proxy instead resolves a
// repo-local override file at startup from its working directory — the
// workspace the IDE opened, the same cwd assumption proxy heartbeats
// already rely on for git namespace inference (see proxy_heartbeat.go).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// repoProxyOverrideFile is the repo-relative override file resolved by
// walking up from the proxy's working directory.
const repoProxyOverrideFile = ".loom/proxy.yaml"

// repoProxyOverrideMaxWalk bounds the upward directory walk so a proxy
// spawned deep inside a non-repo tree cannot scan the whole filesystem.
const repoProxyOverrideMaxWalk = 12

// proxyToolProfileEnv overrides the tool profile for a single session,
// beating both the repo override file and the CLI flag. Escape hatch for
// manual sessions ("run this one session with icc-core").
const proxyToolProfileEnv = "LOOM_PROXY_TOOL_PROFILE"

// repoProxyConfig is the schema of .loom/proxy.yaml.
//
//	tool_profile: icc-core        # optional: override for every agent hint
//	max_tools: 0                  # optional: 0 = profile default
//	agents:                       # optional: per --agent-hint overrides
//	  antigravity:
//	    tool_profile: icc-core
type repoProxyConfig struct {
	ToolProfile string                            `yaml:"tool_profile"`
	MaxTools    int                               `yaml:"max_tools"`
	Agents      map[string]repoProxyAgentOverride `yaml:"agents"`
}

type repoProxyAgentOverride struct {
	ToolProfile string `yaml:"tool_profile"`
	MaxTools    int    `yaml:"max_tools"`
}

// knownProxyToolProfiles gates overrides: an unknown profile name would fall
// through filterProxyTools unshaped and expose the full unfiltered tool list,
// which breaks 100-tool-ceiling clients — worse than ignoring the override.
var knownProxyToolProfiles = map[string]struct{}{
	proxyToolProfileAntigravityCore: {},
	proxyToolProfileLLMCore:         {},
	proxyToolProfileICCCore:         {},
}

// applyProxyProfileOverrides resolves the effective tool profile and cap.
// Precedence: LOOM_PROXY_TOOL_PROFILE env > repo agents.<hint> > repo
// top-level > the values passed on the command line. When an override
// changes the profile, the cap resets to the override's max_tools (0 =
// let resolveProxyToolFilter derive the new profile's default) so a CLI
// cap paired with the old profile is not carried over.
func applyProxyProfileOverrides(agentHint, toolProfile string, maxTools int) (string, int) {
	if env := strings.ToLower(strings.TrimSpace(os.Getenv(proxyToolProfileEnv))); env != "" {
		if _, ok := knownProxyToolProfiles[env]; ok {
			if env != strings.ToLower(strings.TrimSpace(toolProfile)) {
				fmt.Fprintf(os.Stderr, "loom proxy: tool profile %q from %s\n", env, proxyToolProfileEnv)
			}
			return env, 0
		}
		fmt.Fprintf(os.Stderr, "loom proxy: ignoring unknown tool profile %q from %s\n", env, proxyToolProfileEnv)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return toolProfile, maxTools
	}
	cfg, path := loadRepoProxyConfig(cwd)
	if cfg == nil {
		return toolProfile, maxTools
	}

	profile, limit := cfg.ToolProfile, cfg.MaxTools
	if hint := strings.ToLower(strings.TrimSpace(agentHint)); hint != "" {
		if agent, ok := cfg.Agents[hint]; ok && strings.TrimSpace(agent.ToolProfile) != "" {
			profile, limit = agent.ToolProfile, agent.MaxTools
		}
	}
	profile = strings.ToLower(strings.TrimSpace(profile))
	if profile == "" {
		return toolProfile, maxTools
	}
	if _, ok := knownProxyToolProfiles[profile]; !ok {
		fmt.Fprintf(os.Stderr, "loom proxy: ignoring unknown tool profile %q from %s\n", profile, path)
		return toolProfile, maxTools
	}
	if profile != strings.ToLower(strings.TrimSpace(toolProfile)) || limit != maxTools {
		fmt.Fprintf(os.Stderr, "loom proxy: tool profile %q (max-tools %d) from %s\n", profile, limit, path)
	}
	return profile, limit
}

// loadRepoProxyConfig walks up from dir looking for .loom/proxy.yaml and
// parses the first hit. Returns (nil, "") when no file exists; a file that
// exists but fails to parse is reported on stderr and ignored.
func loadRepoProxyConfig(dir string) (*repoProxyConfig, string) {
	for range repoProxyOverrideMaxWalk {
		path := filepath.Join(dir, repoProxyOverrideFile)
		if data, err := os.ReadFile(path); err == nil {
			var cfg repoProxyConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				fmt.Fprintf(os.Stderr, "loom proxy: ignoring malformed %s: %v\n", path, err)
				return nil, ""
			}
			return &cfg, path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil, ""
}
