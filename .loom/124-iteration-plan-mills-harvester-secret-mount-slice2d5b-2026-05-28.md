# RALPH Iteration Plan — HarvesterVMBackend SecretMount propagation (Slice 2d.5b)

- **Date**: 2026-05-28
- **Lineage**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` (Slice 2). Slice 2d.5 shipped env+SecretEnv propagation. Slice 2d.5b closes the SecretMount drop so file-style auth (claude OAuth JSON, codex OAuth JSON, gemini SA JSON) reaches the Harvester VM at boot — required to unblock `codex` on harvester-vm (the agent has no env-var auth path) and useful for `claude-code` (OAuth JSON preferred over `ANTHROPIC_API_KEY` fallback).

## Review

- Roadmap milestone: `.loom/45`, Slice 2 "Mills opts in for `mills-canary-*` items"; Slice 2 acceptance ("1 successful end-to-end auto-merge of a canary item via harvester-vm") is one step closer once codex+claude can auth via OAuth files.
- Spec sections:
  - "Slice 2d.5b (next)" status line — added in `5b2c2787` (Slice 2d.5 harvest).
- Prior slices that this builds on:
  - **2d** (`88bc0bcc`): multi-backend init scaffolding + per-spawn substrate routing.
  - **2d.5** (`0d6f8a03`): `SecretResolver` interface + `K8sBackend.ResolveSecretEnv` + `HarvesterVMBackendConfig.SecretResolver` field + `buildVMManifest(opts, cfg, sshPubKey, env)` cloud-init env file rendering. **This slice extends the same pattern from env-vars to files.**
- Concrete prior-art to mirror:
  - `K8sBackend.ResolveSecretEnv` ([internal/devbox/backend/k8s.go:262](internal/devbox/backend/k8s.go:262)) — Optional semantics + per-SecretName caching.
  - `renderCloudInitEnvBlock` ([internal/devbox/backend/harvester_vm_objects.go:263](internal/devbox/backend/harvester_vm_objects.go:263)) — single `write_files:` YAML block with indent-aware content rendering.
  - `agentSecretMounts(agentType)` ([internal/hud/spawn.go:1835](internal/hud/spawn.go:1835)) — already populated upstream on `StartOpts.SecretMounts`; this slice consumes it on the harvester-vm side.
- Known gaps NOT closed by this slice (deliberate — narrow line):
  - **Home-dir mismatch** (`/home/agent` mountpaths vs. SSH user `ubuntu`'s `/home/ubuntu`). The K8s spawn pod runs as user `agent` with `HOME=/home/agent`; agentSecretMounts uses `AgentHomeDir + "/.claude.auth"` etc. (e.g., `/home/agent/.claude.auth/oauth.json`). The Harvester VM's SSH user is `ubuntu` by default. This slice writes the files at the literal K8s-style path and owns them by `ubuntu:ubuntu` so the SSH session can read them, but the agent CLIs look for auth under `$HOME` which on the VM is `/home/ubuntu`, not `/home/agent`. **Slice 2d.5c** closes this gap (either by setting `HOME=/home/agent` on the VM, by symlinking `/home/agent` → `/home/ubuntu`, or by remapping mountpaths at orchestrator boundary). End-to-end agent auth on harvester-vm is blocked on 2d.5c.
  - **injectAgentConfig** (spawn.go:990-1050) writes `~/.codex/config.toml` etc. inside the running container/VM. The same home-dir issue applies. Slice 2d.5c addresses this too.
  - End-to-end live auto-merge of a canary item on harvester-vm → Slice 2 acceptance kill-test, which needs 2d.5c done first.

## Riskiest assumption + kill-test

**Load-bearing assumption**: cloud-init v24.x (shipped in the pre-baked Ubuntu 24.04 `mills-devbox-base` image — see Slice 1.5 memo) honors the `write_files[].encoding: b64` directive by base64-decoding the `content` field before writing the target file, yielding byte-for-byte the original payload. This is the standard `cloud-init` contract documented at <https://cloudinit.readthedocs.io/en/latest/reference/modules.html#write-files> ("encoding" field, values include `b64` for base64), and we already lean on the same `write_files` module for `/etc/loom-spawn.env` in Slice 2d.5 (without encoding, since env values are plain ASCII). The new bet is purely about the `encoding: b64` codepath for binary-safe content.

**Kill test** (post-merge, configured operator + Harvester reachable):

1. Pick any small binary blob (e.g., 64 bytes from `/dev/urandom`).
2. Build a synthetic `StartOpts.SecretMounts` referencing a test Secret containing that blob keyed as `binary-test`, mount path `/tmp/loom-mount-test`, item path `payload.bin`.
3. `POST /api/mobile/v1/agent/spawn` (or invoke `HarvesterVMBackend.Start` directly via integration test) to provision a VM with the manifest.
4. SSH in: `md5sum /tmp/loom-mount-test/payload.bin` and compare to the host-side `md5sum` of the source blob.
5. Equal → assumption passed.

~10 min including VM cold boot.

**Failure mode if wrong**: the file lands on the VM but content is mangled (cloud-init writes the literal base64 string instead of decoded bytes, or strips trailing `=`, or applies extra newline). Detection: any non-trivial Secret value (an OAuth JSON) becomes invalid JSON and the agent CLI fails to authenticate at exec time with a parse error — easy to diagnose. Mitigation: cloud-init `encoding: b64` is in widespread production use (every major distro uses it for kernel-config + cert delivery); the risk is low. Backup plan: switch to `encoding: gz+b64` (also documented) or fall back to a one-shot `scp`-after-boot pattern.

**Status**: not run.

## Align

- Slice name: **HarvesterVMBackend `SecretMount` propagation; cloud-init `write_files` delivers Secret-backed files at boot**
- Scope in:
  1. **`internal/devbox/backend/backend.go`**: add `ResolvedSecretFile` type + extend `SecretResolver` interface
     ```go
     // ResolvedSecretFile is one file's worth of Secret content, with its
     // destination path inside the sandbox already computed by the resolver
     // (typically filepath.Join(SecretMount.MountPath, item.Path)).
     type ResolvedSecretFile struct {
         Path    string // absolute path inside the sandbox
         Content []byte // raw bytes; binary-safe
         Mode    string // POSIX mode (default "0600")
     }

     type SecretResolver interface {
         ResolveSecretEnv(ctx context.Context, secrets []SecretEnvVar) (map[string]string, error)
         ResolveSecretMounts(ctx context.Context, mounts []SecretMount) ([]ResolvedSecretFile, error)
     }
     ```
  2. **`internal/devbox/backend/k8s.go`**: implement `ResolveSecretMounts` on `K8sBackend`. Mirror `ResolveSecretEnv` cache structure (per-SecretName cache). For each SecretMount, iterate Items; emit one `ResolvedSecretFile{Path: filepath.Join(m.MountPath, it.Path), Content: secret.Data[it.Key], Mode: "0600"}`. Missing Secret or missing Key → skip silently (Optional semantics — matches K8s `SecretVolumeSource.Optional=true` already set in `buildPodSpec`).
  3. **`internal/devbox/backend/harvester_vm.go`**: new `resolveStartMounts(ctx, opts) ([]ResolvedSecretFile, error)` helper alongside `resolveStartEnv`. Same nil-safe semantics: nil `cfg.SecretResolver` + non-empty `opts.SecretMounts` → warn-log + return nil + continue.
  4. **`internal/devbox/backend/harvester_vm.go`** `Start`: call `resolveStartMounts`, thread the result as a new `files []ResolvedSecretFile` parameter to `buildVMManifest`.
  5. **`internal/devbox/backend/harvester_vm_objects.go`** `buildVMManifest(opts, cfg, sshPubKey, env, files)`: new `files` parameter. The existing `renderCloudInitEnvBlock` is renamed/extended to a single `renderCloudInitWriteFiles(env, files)` that produces ONE `write_files:` YAML block containing both the env file entry AND one entry per `ResolvedSecretFile`. Binary content uses `encoding: b64`. Files are owned `ubuntu:ubuntu` (so SSH user can read) with mode = `file.Mode`. Sorted by path for deterministic output.
  6. **`internal/devbox/backend/harvester_vm_objects.go`** `renderCloudInitWriteFiles` returns empty string when both inputs are empty, matching today's behavior. Newline-rejection from `renderCloudInitEnvBlock` is preserved (env-only).
  7. **Tests**:
     - `internal/devbox/backend/k8s_test.go`: `TestK8sBackend_ResolveSecretMounts` — table-driven, covering happy path with two SecretMounts → two Secrets, missing Secret → skipped, missing Key → skipped, binary content roundtrip (Secret.Data byte slice → ResolvedSecretFile.Content equal), per-SecretName caching (PrependReactor counts Get calls).
     - `internal/devbox/backend/harvester_vm_objects_test.go`: `TestBuildVMManifest_RendersSecretFilesIntoCloudInit` — asserts userData contains one `write_files:` block with both the env entry AND `path: /home/agent/.claude.auth/oauth.json`, `encoding: b64`, base64-encoded content. `TestBuildVMManifest_EmptyEnvAndFilesOmitsWriteFiles`. `TestBuildVMManifest_FilesOnlyNoEnv` — works without env. `TestBuildVMManifest_OwnerIsSSHUser` — file owner uses `cfg.SSHUser:cfg.SSHUser`.
     - `internal/devbox/backend/harvester_vm_test.go`: extend Start-path test to assert mounts are resolved via a fake `SecretResolver` and reach the manifest.
- Scope out:
  - Home-dir remap (`/home/agent` → `/home/ubuntu`) → Slice 2d.5c.
  - `injectAgentConfig` home-dir awareness on harvester-vm → Slice 2d.5c.
  - Live kill-test execution (post-merge step requires operator config + Harvester reachable).
- Acceptance criteria:
  1. `go build ./...` clean.
  2. `go test ./internal/devbox/backend/... ./internal/hud/...` green.
  3. `golangci-lint run` clean on touched packages.
  4. `K8sBackend.ResolveSecretMounts` reads each referenced Secret via Clientset; missing-Secret + missing-Key cases return no error (Optional semantics); per-SecretName cache verified.
  5. `buildVMManifest` with non-empty `files` produces userData containing a `write_files` entry per file, each with `encoding: b64` and base64-encoded `content`, owner `<SSHUser>:<SSHUser>`, mode from `ResolvedSecretFile.Mode` (default `0600`).
  6. `buildVMManifest` merges env entry + file entries into a SINGLE `write_files:` block (cloud-init parses each top-level key once).
  7. `HarvesterVMBackend.Start` with `SecretResolver != nil` resolves `opts.SecretMounts` and merges them into the manifest before VM create.
  8. `HarvesterVMBackend.Start` with `SecretResolver == nil` and non-empty `opts.SecretMounts` logs a warning and continues without files (parity with SecretEnv behavior).
  9. Existing `TestBuildVMManifest_RendersEnvIntoCloudInit` etc. still pass (env-only path unchanged in observable output).
- Dependencies/blockers:
  - Slice 2d.5 shipped (provides `SecretResolver` interface + `cfg.SecretResolver` field + the `renderCloudInitEnvBlock` helper we extend).

## Land

- Worktree: `.worktrees/feat-mills-harvester-secret-mount/` on branch `feat/mills-harvester-secret-mount` from `main` (already allocated).
- Planned file areas:
  - `internal/devbox/backend/backend.go` (`ResolvedSecretFile` type, interface extension)
  - `internal/devbox/backend/k8s.go` (`ResolveSecretMounts` method)
  - `internal/devbox/backend/k8s_test.go` (resolver unit test)
  - `internal/devbox/backend/harvester_vm.go` (`resolveStartMounts` helper; Start threading)
  - `internal/devbox/backend/harvester_vm_objects.go` (`renderCloudInitWriteFiles` rename/extension; `buildVMManifest` signature)
  - `internal/devbox/backend/harvester_vm_objects_test.go` (new file-render tests)
  - `internal/devbox/backend/harvester_vm_test.go` (Start path test, if light enough)
  - `.loom/124-iteration-plan-…` (this doc)
- Implementation order:
  1. `ResolvedSecretFile` + interface extension + `K8sBackend.ResolveSecretMounts` + unit test.
  2. `renderCloudInitWriteFiles` extension (env + files in one block) + manifest tests.
  3. `buildVMManifest` signature change + every call site updated.
  4. `HarvesterVMBackend.resolveStartMounts` + Start threading.
  5. `go build ./... && go vet ./... && go test ./internal/devbox/backend/... && golangci-lint run`.

## Prove

- Targeted tests: `go test ./internal/devbox/backend/... -run "ResolveSecretMounts|BuildVMManifest|HarvesterVM|RenderCloudInit" -v`
- Broader: `go test ./internal/devbox/... ./internal/hud/...`
- `go build ./...`
- `go vet ./internal/devbox/... ./internal/hud/...`
- `golangci-lint run ./internal/devbox/backend/... ./internal/hud/...`
- Pre-commit hooks pass.
- CI green.

## Handoff/Harvest

- Docs on land:
  - `.loom/00-index.md` Mills VM substrate one-liner: append "Slice 2d.5b shipped YYYY-MM-DD as `<sha>` ([!MR]); next: Slice 2d.5c (home-dir remap so agent CLIs find the mounted auth files) → Slice 2 acceptance kill-test".
  - `.loom/45-…` Slice 2d.5b status line: change "(next)" → "✅ SHIPPED YYYY-MM-DD as `<sha>` ([!MR])"; append Slice 2d.5c "(next)" entry.
- Agent-context entries (post-merge):
  - decision: "SecretMount resolution rides on the same `SecretResolver` interface as SecretEnv. Single interface keeps the `HarvesterVMBackendConfig.SecretResolver` field doing one job (delegating Secret reads to a backend that has Clientset access). Alternative considered: a separate `SecretMountResolver` interface — rejected because callers always have both kinds of secrets to resolve and one interface keeps the wiring trivial."
  - decision: "`encoding: b64` chosen over `gz+b64` for the first cut. Compression saves bytes on multi-KB OAuth JSON but adds a moving part; current SecretMounts top out at ~2KB which fits comfortably in the cloud-init userData (16KB+ envelope on KubeVirt). Revisit if we ever mount larger payloads (kernel configs, service-account chains)."
  - finding: "Cloud-init lands files at the literal K8s MountPath but the Harvester VM's `ubuntu` user has `HOME=/home/ubuntu`, not `/home/agent` (where `agentSecretMounts` targets). Files are reachable by path but the agent CLI's `~/.codex/auth.json` lookup misses them. Slice 2d.5c needs to either set `HOME=/home/agent` on the VM (create the user via cloud-init), symlink `/home/agent` → `/home/ubuntu`, or remap mountpaths at the orchestrator boundary."
  - question: "Slice 2d.5c approach — create `agent` user via cloud-init `users:` stanza (clean, matches K8s) vs. symlink `/home/agent` → `/home/ubuntu` (one-line, less invasive). Decide in 2d.5c planning. The user-creation path likely wins because it also makes `injectAgentConfig` paths work without remap."
- Next-slice candidates:
  - **Slice 2d.5c**: home-dir parity (`agent` user on the VM) so agent CLIs can read mounted files at their expected `$HOME/.…` paths.
  - **Slice 2e**: `cmd/mcp-devbox` multi-backend init.
  - **Slice 2 acceptance**: live end-to-end auto-merge kill-test on harvester-vm.

## Sources

- Spec: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` § "Slice 2d.5b (next)" (added 2026-05-27 in `5b2c2787`).
- Slice 2d.5 plan: `.loom/123-iteration-plan-mills-spawn-substrate-slice2d5-2026-05-27.md`.
- Slice 2d.5 code: commit `0d6f8a03` ([!570](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/570)).
- `agentSecretMounts`: [internal/hud/spawn.go:1835](internal/hud/spawn.go:1835).
- K8s SecretMount → SecretVolumeSource render: [internal/devbox/backend/k8s_objects.go:59](internal/devbox/backend/k8s_objects.go:59) (reference for what's being replaced on the VM side).
- Cloud-init `write_files` docs: <https://cloudinit.readthedocs.io/en/latest/reference/modules.html#write-files>.
- HarvesterVMBackend Start (post-2d.5): [internal/devbox/backend/harvester_vm.go:210](internal/devbox/backend/harvester_vm.go:210).
- buildVMManifest (post-2d.5): [internal/devbox/backend/harvester_vm_objects.go:73](internal/devbox/backend/harvester_vm_objects.go:73).
- renderCloudInitEnvBlock (post-2d.5): [internal/devbox/backend/harvester_vm_objects.go:263](internal/devbox/backend/harvester_vm_objects.go:263).
