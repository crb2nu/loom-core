// Vendor-CLI resolution for the standalone driver bundle.
//
// Both SDKs default to locating a platform-native CLI binary inside their own
// npm package tree (claude-agent-sdk 0.3.x resolves an optional
// @anthropic-ai/claude-agent-sdk-<platform> package; codex-sdk resolves
// vendored binaries under @openai/codex). That lookup is relative to the
// SDK module's own location, so it can never succeed for this driver: the
// spawn pod runs the bare esbuild bundle at /opt/loom/spawn-driver.js with
// no node_modules beside it. Without an explicit path, claude-agent-sdk
// 0.3.x throws "Native CLI binary for <platform> not found" on every live
// spawn (dry-run smokes never reach the lookup).
//
// The pods DO have the CLIs: the orchestrator's install layer guarantees
// `claude` / `codex` on PATH (spawn.go agentCLIInstallLines, `command -v`
// guarded `npm install -g`). So the driver resolves the executable from
// PATH and hands it to the SDK explicitly.

import { accessSync, constants } from "node:fs";
import { delimiter, join } from "node:path";

// resolveExecutableFromPath returns the first executable named `name` on
// PATH, or the `envOverride` variable's value verbatim when set (an escape
// hatch for images that park the CLI outside PATH). Returns undefined when
// nothing is found so callers can fall back to the SDK's own lookup —
// correct for dev setups where the SDK's optional platform package is
// actually installed.
export function resolveExecutableFromPath(
  name: string,
  envOverride: string,
): string | undefined {
  const override = process.env[envOverride];
  if (override) return override;
  for (const dir of (process.env.PATH ?? "").split(delimiter)) {
    if (!dir) continue;
    const candidate = join(dir, name);
    try {
      accessSync(candidate, constants.X_OK);
      return candidate;
    } catch {
      // Not present or not executable in this PATH entry; keep scanning.
    }
  }
  return undefined;
}
