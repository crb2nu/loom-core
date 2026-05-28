# RALPH Iteration Plan — HarvesterVMBackend env + SecretEnv propagation (Slice 2d.5)

- **Date**: 2026-05-27
- **Lineage**: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` (Slice 2). Slices 2a/2b/2c/2d shipped. Slice 2d.5 closes the env-drop gap that the Slice 2d MR description flagged: `HarvesterVMBackend.Start` ignores `opts.Env` + `opts.SecretEnv`, and the orchestrator's primary agent-exec call doesn't pass `ExecOpts.Env`, so a `harvester-vm` spawn currently boots a VM but the agent CLI runs without API keys.

## Review

- Roadmap milestone: `.loom/45`, Slice 2 "Mills opts in for `mills-canary-*` items"; Slice 2 acceptance ("1 successful end-to-end auto-merge of a canary item via harvester-vm") is blocked on this slice.
- Spec sections:
  - "Slice 2d.5 (next)" status line — added in the Slice 2d harvest commit (`5f53781e`).
- Prior decisions to preserve:
  - `agentSecretEnvVars(req.AgentType)` lives in `internal/hud/spawn.go:1762`; it returns `[]backend.SecretEnvVar` (K8s SecretKeyRefs, not values). For K8s pods the API server resolves the refs at pod-start; harvester-vm has no equivalent path.
  - `agentSecretMounts(req.AgentType)` returns file-style mounts (`.claude/oauth.json`, `.codex/auth.json`, `.gcp/sa.json`). **Out of scope for this slice** — gives a clean line: env→2d.5, files→2d.5b.
  - `HarvesterVMBackend.Exec` already honors `ExecOpts.Env` via an `export KEY=value;` shell prefix at [internal/devbox/backend/harvester_vm.go:421-427](internal/devbox/backend/harvester_vm.go:421). So Exec-time env passthrough Just Works once the orchestrator threads it through.
  - Cloud-init `write_files` is the standard mechanism for landing files on a VM at boot. Files under `/etc/profile.d/*.sh` are sourced by login shells; for non-interactive SSH (`bash -c`) sshd sources `/etc/profile` only when invoked with `-l`. The orchestrator's agent-exec call uses `bash -c <cmd>` (non-login), so the env file pattern alone is insufficient — Exec MUST also export the env via its shell prefix. The cloud-init file is belt-and-suspenders for out-of-band shells (humans SSHing in for debug) and for any future direct VM-side daemon.
- Known gaps NOT closed by this slice:
  - SecretMount (file-style auth like `~/.claude.auth/oauth.json`) → Slice 2d.5b.
  - End-to-end live auto-merge of a canary item on harvester-vm → Slice 2 acceptance kill-test.

## Riskiest assumption + kill-test

**Load-bearing assumption**: For the `gemini` agent type, env-only auth (`GEMINI_API_KEY` / `GOOGLE_API_KEY`) is sufficient to authenticate the agent CLI inside a Harvester VM — i.e., once Slice 2d.5 ships `ExecOpts.Env` with resolved secrets, a gemini spawn on harvester-vm can make a live LLM call. For `claude-code`, env-only auth (`ANTHROPIC_API_KEY` fallback path; `CLAUDE_CODE_OAUTH_TOKEN` is technically a SecretEnv too, deliverable via this slice) is sufficient. For `codex`, env-only auth is intentionally NOT used (`agentSecretEnvVars("codex")` returns nil — see [spawn.go:1798](internal/hud/spawn.go:1798) for the rationale); codex requires `~/.codex/auth.json` which is a SecretMount, so codex on harvester-vm stays blocked until Slice 2d.5b.

**Kill test** (post-merge, configured operator + Harvester reachable):
1. `POST /api/mobile/v1/agent/spawn` with `{"agent_type": "gemini", "substrate": "harvester-vm", "task_description": "echo hello", "project": "loom-core"}`.
2. `kubectl exec` into the spawn VM via the harvester-vm backend's SSH and run `env | grep -E "(GEMINI|GOOGLE)_API_KEY"` — expect both populated with non-empty values matching the cluster's `cluster-agent-api-keys` Secret.
3. `cat /etc/loom-spawn.env` — expect the same K=V pairs (shell-quoted).
4. Inspect the spawn log/telemetry: gemini CLI completes ≥1 turn with non-401 LLM responses.

~10 min including VM cold boot.

**Failure mode if wrong**: env reaches the VM via `/etc/loom-spawn.env` and via Exec's shell prefix, but the gemini CLI doesn't pick up `GEMINI_API_KEY` from the shell environment. Surface area: gemini CLI requires env-vars at a different process-tree level, OR Exec's shell prefix is sourced in a subshell that doesn't propagate to the spawned CLI. Mitigation: gemini docs explicitly list env-var auth as supported; precedent is the existing K8s spawns that succeed with the same env-var route. Risk is low.

**Status**: not run.

## Align

- Slice name: **HarvesterVMBackend env + SecretEnv propagation; orchestrator threads env through agent-exec**
- Scope in:
  1. **`internal/devbox/backend/backend.go`**: add `SecretResolver` interface
     ```go
     // SecretResolver translates SecretEnvVar refs (K8s Secret keys) into
     // plaintext values. K8sBackend implements this natively via its
     // Clientset; non-K8s backends accept it as an optional dependency.
     type SecretResolver interface {
         ResolveSecretEnv(ctx context.Context, secrets []SecretEnvVar) (map[string]string, error)
     }
     ```
  2. **`internal/devbox/backend/k8s.go`** (or `k8s_runtime.go`): implement on `K8sBackend`
     ```go
     func (k *K8sBackend) ResolveSecretEnv(ctx context.Context, secrets []SecretEnvVar) (map[string]string, error) {
         out := make(map[string]string, len(secrets))
         seen := map[string]string{} // SecretName → cached read
         for _, s := range secrets {
             cacheKey := s.SecretName
             // ... read Secret via k.clientset.CoreV1().Secrets(k.namespace).Get(...) ...
             // ... extract s.SecretKey value; skip if missing (Optional contract) ...
         }
         return out, nil
     }
     ```
     Honor the `Optional` semantics: a missing Secret or missing key is a no-op for that entry, not an error. Caches Secret reads by name so multiple SecretEnvVar entries pointing at the same Secret hit the API once.
  3. **`internal/devbox/backend/harvester_vm.go`**: `HarvesterVMBackendConfig` gains optional `SecretResolver SecretResolver` field. `NewHarvesterVMBackend` accepts it (nil-safe: nil resolver means harvester-vm spawns get only `opts.Env`, no Secret resolution).
  4. **`internal/devbox/backend/harvester_vm.go`** `Start`: when `opts.SecretEnv` is non-empty AND `h.cfg.SecretResolver != nil`, resolve to plaintext and merge into `opts.Env` (caller's `opts.Env` keys win on conflict). Log a warning when resolver is nil but SecretEnv was requested — surfaces misconfigured operators.
  5. **`internal/devbox/backend/harvester_vm_objects.go`** `buildVMManifest`: take a new `env map[string]string` parameter. Render it as a cloud-init `write_files` entry:
     ```yaml
     write_files:
       - path: /etc/loom-spawn.env
         owner: root:root
         permissions: '0644'
         content: |
           # Generated by HarvesterVMBackend; sourced by orchestrator Exec prefix.
           KEY1='value1'
           KEY2='value2'
     ```
     Values are shell-quoted (`shellQuote`-style); newline-bearing values rejected with an error (env vars don't legitimately contain `\n` and accepting them risks YAML-injection into the cloud-init manifest).
  6. **`internal/devbox/backend/harvester_vm.go`** `Exec`: prepend `set -a; . /etc/loom-spawn.env 2>/dev/null; set +a; ` to the shell command BEFORE the existing `export KEY=value;` loop. The file-source ensures any env baked in at Start is honored for any Exec, even if the caller's `ExecOpts.Env` is empty. `2>/dev/null` survives the file-not-exists case (first boot, file race).
  7. **`internal/hud/spawn.go`** `runSpawn` agent-exec call (around line 842): add `Env: env` (the same map already passed to Start) to the buffered `ExecOpts` literal. The streaming path is K8s-only; not changing.
  8. **Tests**:
     - `internal/devbox/backend/k8s_test.go` (or new `k8s_secret_resolver_test.go`): `TestK8sBackend_ResolveSecretEnv` with `fake.NewSimpleClientset` containing two Secrets covering: a) all keys present, b) missing Secret returns no error + skips, c) missing key in a present Secret returns no error + skips (Optional contract).
     - `internal/devbox/backend/harvester_vm_objects_test.go`: extend or add `TestBuildVMManifest_RendersEnvIntoCloudInit` table-driven case asserting userData contains `write_files` block with shell-quoted KEY='value' pairs.
     - `internal/devbox/backend/harvester_vm_objects_test.go`: `TestBuildVMManifest_RejectsNewlineEnv` — value containing `\n` returns an error.
     - `internal/devbox/backend/harvester_vm_test.go` (already has tests): unit test for `Start` env-resolution path using a fake `SecretResolver`; assert resolved env reaches the manifest. Skip if integration-heavy.
     - `internal/hud/spawn_test.go`: assert that the orchestrator passes `Env` through to ExecOpts when calling the buffered exec path. May need to extend the existing `recordingBackend` fake to capture ExecOpts.
- Scope out:
  - SecretMount (file-style) propagation → Slice 2d.5b.
  - Live kill-test execution (post-merge step, requires operator config + Harvester reachable).
  - GitOps wiring of `SPAWN_HARVESTER_*` env-vars in `platform/gitops` → separate MR.
  - SecretResolver wiring through the orchestrator's `initSpawnOrchestrator` — small change to pass `k8sBackend` itself (which implements the interface) into `HarvesterVMBackendConfig`. Including in this slice.
- Acceptance criteria:
  1. `go build ./...` clean.
  2. `go test ./internal/devbox/backend/... ./internal/hud/...` green.
  3. `golangci-lint run` clean on touched packages.
  4. `K8sBackend.ResolveSecretEnv` reads Secret values via Clientset; missing-key/missing-Secret cases return no error (Optional semantics).
  5. `buildVMManifest` with non-empty env produces userData containing a `write_files` entry for `/etc/loom-spawn.env` with shell-quoted KEY='value' pairs.
  6. `buildVMManifest` rejects env values containing newlines.
  7. `HarvesterVMBackend.Exec` shell prefix sources `/etc/loom-spawn.env` BEFORE applying `opts.Env` exports.
  8. `HarvesterVMBackend.Start` with `SecretResolver != nil` resolves `opts.SecretEnv` and merges into `opts.Env` before passing to `buildVMManifest`.
  9. `HarvesterVMBackend.Start` with `SecretResolver == nil` and non-empty `opts.SecretEnv` logs a warning and continues with only `opts.Env`.
  10. `runSpawn`'s buffered agent-exec call passes `Env: env` (verified via test).
  11. `initSpawnOrchestrator` wires `cfg.SecretResolver = k8sBackend` when constructing the HarvesterVMBackend.
- Dependencies/blockers:
  - Slice 2d shipped (provides the multi-backend init scaffolding + the `cfg.SpawnHarvester*` field set we hang `SecretResolver` off of internally).

## Land

- Worktree: `feat/mills-harvester-env-prop` from `main`.
- Planned file areas:
  - `internal/devbox/backend/backend.go` (SecretResolver interface)
  - `internal/devbox/backend/k8s.go` or `k8s_runtime.go` (ResolveSecretEnv method)
  - `internal/devbox/backend/k8s_test.go` (resolver unit test)
  - `internal/devbox/backend/harvester_vm.go` (config field; Start env resolution; Exec prefix update)
  - `internal/devbox/backend/harvester_vm_objects.go` (`buildVMManifest` env param + write_files render)
  - `internal/devbox/backend/harvester_vm_test.go` + `harvester_vm_objects_test.go` (manifest + resolver hookup tests)
  - `internal/hud/embed.go` (`SecretResolver: spawnBackend` line in HarvesterVMBackendConfig literal)
  - `internal/hud/spawn.go` (Env passthrough in agent-exec ExecOpts)
  - `internal/hud/spawn_test.go` (ExecOpts.Env passthrough test)
  - `.loom/123-iteration-plan-…` (this doc)
- Implementation order:
  1. SecretResolver interface + K8sBackend implementation + unit test.
  2. HarvesterVMBackend config field + Start env-resolution path.
  3. buildVMManifest env-render + tests.
  4. HarvesterVMBackend.Exec source-file prefix.
  5. embed.go wires resolver.
  6. spawn.go runSpawn Env passthrough + test.
  7. Devbox build + test + vet + lint.

## Prove

- Tests: `go test ./internal/devbox/backend/... -run "ResolveSecretEnv|BuildVMManifest|HarvesterVM" -v`; `go test ./internal/hud/... -run "ExecOpts|Env" -v`; full `./internal/devbox/... ./internal/hud/...`.
- `go build ./...`.
- `go vet ./internal/devbox/... ./internal/hud/...`.
- `golangci-lint run ./internal/devbox/backend/... ./internal/hud/...`.
- Pre-commit hooks pass.
- CI green.

## Handoff/Harvest

- Docs on land:
  - `.loom/00-index.md` Mills VM substrate one-liner: append "Slice 2d.5 shipped YYYY-MM-DD as `<sha>` ([!MR]); next: Slice 2d.5b (SecretMount → cloud-init write_files) → Slice 2e (mcp-devbox multi-backend init) → Slice 2 acceptance kill-test".
  - `.loom/45-…` Slice 2d.5 status line: change "(next)" → "✅ SHIPPED YYYY-MM-DD as `<sha>` ([!MR])".
- Agent-context entries (post-merge):
  - decision: "SecretResolver is a backend-level interface, not an orchestrator concern. K8sBackend implements it natively via its existing Clientset. Non-K8s backends accept it as optional config. Rationale: Secret resolution is fundamentally a K8s API operation; lifting it to the orchestrator would require either threading the K8s Clientset through public APIs or duplicating the Get-Secret logic. A narrow interface lets HarvesterVMBackend stay testable in isolation (pass a fake resolver) while reusing K8sBackend's already-wired Clientset."
  - finding: "Cloud-init `write_files` lands /etc/loom-spawn.env in the VM at boot, but non-login SSH (the orchestrator's `bash -c ...` pattern) won't auto-source it. HarvesterVMBackend.Exec compensates by prepending `set -a; . /etc/loom-spawn.env 2>/dev/null; set +a;` to every shell command. This makes env delivery work for both orchestrator-driven Exec AND humans `ssh ubuntu@VM` for debug."
  - question: "Should SecretMount file-content delivery (Slice 2d.5b) also go via cloud-init `write_files`, or via a one-shot scp-after-boot? Cloud-init keeps the VM declaratively complete; scp keeps Secret values out of the VirtualMachine manifest stored in etcd. The latter avoids etcd Secret-at-rest concerns but adds a Start-time SSH round-trip. To decide in 2d.5b planning."
- Next-slice candidates:
  - **Slice 2d.5b**: SecretMount (file-style auth) → cloud-init `write_files` of base64-decoded content. Required for codex on harvester-vm; nice-to-have for claude-code (OAuth JSON path).
  - **Slice 2e**: `cmd/mcp-devbox` multi-backend init.
  - **Slice 2 acceptance**: kill-test live on a configured operator.

## Sources

- Spec: `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` § "Slice 2d.5 (next)" (added 2026-05-27 in `5f53781e`).
- Slice 2d plan: `.loom/122-iteration-plan-mills-spawn-substrate-slice2d-2026-05-27.md`.
- Slice 2d code: commits `88bc0bcc` ([!568](https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/568)).
- HarvesterVMBackend env-drop: `internal/devbox/backend/harvester_vm.go:199` (Start signature) + `internal/devbox/backend/harvester_vm_objects.go:58` (buildVMManifest signature — no env consumer).
- HarvesterVMBackend.Exec env honors: `internal/devbox/backend/harvester_vm.go:421-427` (shell prefix).
- K8sBackend buildPodSpec env consumer (reference for shape): `internal/devbox/backend/k8s_objects.go:14-29`.
- Orchestrator agent-exec env-drop: `internal/hud/spawn.go:842-848`.
- agentSecretEnvVars: `internal/hud/spawn.go:1762-1807`.
