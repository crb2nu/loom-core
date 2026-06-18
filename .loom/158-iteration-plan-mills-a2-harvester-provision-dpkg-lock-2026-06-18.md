# RALPH Iteration — Mills A2: harvester-vm provision dpkg-lock race (2026-06-18)

## Review

- North-star (`autonomous_merges_24h`) still **0**. Critical path = Phase A
  Slice A2 (first end-to-end autonomous merge on harvester-vm).
  Roadmap: `.loom/126-plan-mills-full-vision-roadmap-2026-06-01.md`.
- All prior A2 code blockers are landed **and deployed** as of today's image
  (operator `20260618-183012`, mobile-hud `loom-core:20260618-182958`):
  workspace+CLI provision (`b4a0485d`), codex `< /dev/null` (`75a89996`),
  telemetry persist (`5fc4dd75`), `nonempty_diff` gate (`4bb853a7`), model pin
  `gpt-5.5` (`!640`/`4615a810`), plan_slice spawn-readiness (`e6989682`),
  DEBT-073 canary scope allowlist (`fbd7faa4`). `/api/mills/capabilities` =
  `autonomy_ready: true`, all green. No A2 re-run had been done against the
  fully-fixed binary.

## Live A2 probes this session (isolated single-shot spawns on harvester-vm,
## codex, no prod flip, no MR — Stage-1 de-risk before any gitops flip)

- **Probe 1** `spawn-535f9751f6dc`: provisioned OK (3m4s), codex emitted
  `thread.started` + `turn.started` then **exit 1 in 6.6s**, `turn_count=1`,
  0 tokens, **no error event on stdout, only the harmless `Reading additional
  input from stdin...` on stderr**.
- **Probe 2** `spawn-b5b17bf8e67e`: failed earlier, at **provisioning**:
  `provision script exited 100 after 12s: Could not get lock
  /var/lib/dpkg/lock-frontend ... held by process 1332 (apt-get)`.

## Root-cause work (what was RULED OUT for probe 1)

Reproduced the spawn faithfully on a parity diagnostic VM (`millsdiag`, same
`default/lan10g` NAD, same `longhorn-image-mc9ph` base image, real cluster
codex auth.json, codex `0.130.0`):

- **codex stdin** — `< /dev/null` gives EXIT=0 locally; the stderr line is
  informational. NOT the cause.
- **model access** — `--model gpt-5.5` EXIT=0; deprecated `gpt-5.3-codex`
  emits a *structured* `error`+`turn.failed` (4 stdout lines), unlike probe 1.
- **token / account** — cluster codex token valid ~150h; `GET
  chatgpt.com/backend-api/codex/models` returns **HTTP 200** and lists
  `gpt-5.5` (`prefer_websockets:true`).
- **codex version** — `0.130.0` works locally (EXIT=0).
- **network egress** — diag VM reaches every AI endpoint (npmjs 200, openai
  401, chatgpt 403, anthropic 404, ws-upgrade 405); no block.
- **IPv6** — chatgpt.com resolves AAAA-only and forced `curl -6` fails (no v6
  route) while `curl -4` works (403); BUT codex completes **8/8** on the diag
  VM regardless (happy-eyeballs), incl. inside a cloned loom-core repo.
- **env file** — VM sources only `AGENT_ID/SPAWN_ID/NAMESPACE/DEVBOX_BACKEND`.
- **cwd / auth split-brain** — codex works in the cloned repo; loom-hub's
  `cluster-agent-auth` has no codex key, so the spawn mounts the valid devbox
  token (pod ns = devbox).

→ codex itself works on an identical VM under faithful conditions. Probe 1's
clean exit-1 (no error event) is **not** stdin/model/token/network/IPv6/cwd/
auth. Leading remaining theory: a transient during that 6.6s window. Left as
an OPEN question (kill-criterion: re-probe after the deploy below; if it
recurs, keep a VM alive multi-turn and inspect the live codex run in-VM).

## Land (this slice — the systematic, reproducible blocker)

**Fix: provision-side `apt-get` must WAIT for the dpkg lock.** The Start-time
provision SSHes in the instant the VM reports ready and races cloud-init's
first-boot `apt-get install qemu-guest-agent` (holds
`/var/lib/dpkg/lock-frontend`). Added `-o DPkg::Lock::Timeout=300` to the two
VM-over-SSH apt paths:

- `internal/devbox/backend/harvester_vm.go` `buildProvisionScript` (git/curl).
- `internal/hud/spawn.go` `agentCLIInstallShell` `ensureNPM` (nodejs/npm).

The Dockerfile/k8s path (`agentCLIInstallLines`) is build-isolated → untouched.
300s comfortably covers cloud-init's first-boot apt; the `command -v` guards
make both no-ops on a future curated base image (Phase B1) or reused VM.

## Prove

- `go test ./internal/devbox/backend -run TestBuildProvisionScript` — ok
- `CGO_ENABLED=0 go test ./internal/hud -run
  'TestAgentCLIInstallShell|TestBuildAgentCommand|TestResolveCodexModel'` — ok
- New regressions: `TestBuildProvisionScript_AptWaitsForDpkgLock`,
  `TestAgentCLIInstallShell_AptWaitsForDpkgLock`.

## Handoff / next

1. Deploy this fix (operator + mobile-hud image via Flux IUA).
2. Re-run the Stage-1 isolated probe (codex, harvester-vm). Confirm
   `turn_count≥1` + **non-empty diff** (resolves probe-1's open question).
3. If green → Stage-2 full A2: gitops flip `stage_substrate:
   {implement,tests: harvester-vm}` → enqueue `loom mills pipelines canary
   --force` → watch plan_slice(k8s)→implement/tests(VM)→mr→merge → revert flip.
4. The provision-time apt races validate **Phase B1 (curated base image)** as
   the durable fix — pre-bake git/node/agent-CLIs/qemu-guest-agent so Start
   does zero apt and boots in ≤60s. Promote B1 alongside A3.

Refs: `.loom/126`, `.loom/148`. Probe evidence: operator/mobile-hud logs
2026-06-18 ~19:54–20:30 (`spawn-535f9751f6dc`, `spawn-b5b17bf8e67e`).
