# RALPH Iteration Plan — Mills Harvester-VM Home-Dir Parity (Slice 2d.5c)

## Review

- Roadmap milestone: Mills harvester-vm devbox substrate — `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`.
- Spec section: Slice 2d.5c (home-dir parity on the harvester VM so agent CLIs find mounted auth files at `$HOME/.…`).
- Prior decisions to preserve:
  - K8s spawn pod runs agents as uid-1000 `agent` user with `HOME=/home/agent` (`internal/hud/spawn.go`: `AgentHomeDir = "/home/agent"`, `GeminiSAMountPath = "/home/agent/.gcp"`).
  - SecretMount propagation (Slice 2d.5b) already lands auth files into the VM via cloud-init `write_files`.
  - The harvester-vm backend must mirror the K8s agent-user contract so `injectAgentConfig` + every `/home/agent` SecretMount path resolves identically across both substrates.

## Align

- Slice name: Harvester-VM agent-user home-dir parity.
- Chosen approach: **(a)** create an `agent` user (HOME=/home/agent) and SSH as `agent`, mirroring the K8s spawn pod.
- Scope in:
  - cloud-init `users:` stanza provisions `name: agent`, `home: /home/agent` (replacing `ubuntu`).
  - `HarvesterVMBackendConfig.SSHUser` defaults to `agent`.
  - Secret files written root-owned, then `chown -R agent:agent /home/agent` via `runcmd`.
- Scope out:
  - Live VM boot kill-test (owed separately; see below).
  - Any change to K8s spawn path or SecretMount discovery logic.
  - Multi-user / per-agent home isolation.
- Acceptance criteria:
  - `buildVMManifest` cloud-init renders `name: agent` + `home: /home/agent`; no `ubuntu` user.
  - Secret `write_files` entries are `root:root` at write time (write-files runs before users-groups).
  - `runcmd` contains `[ chown, -R, agent:agent, /home/agent ]`.
  - `SSHUser` defaults to `agent` via `defaultHarvesterSSHUser = vmAgentUser`.
  - All `internal/devbox/backend` unit tests, `go vet`, `gofmt`, `go build ./...` green.
- Dependencies/blockers: none for unit landing. Live kill-test depends on a bootable harvester-vm + valid codex SecretMount.

## Land

- Planned file areas:
  - `internal/devbox/backend/harvester_vm_objects.go` (cloud-init template, owner consts, render funcs).
  - `internal/devbox/backend/harvester_vm.go` (`SSHUser` default + `defaultHarvesterSSHUser`).
  - `internal/devbox/backend/harvester_vm_objects_test.go` (new agent-user test + owner assertions).
  - `internal/devbox/backend/harvester_vm_test.go` (SSHUser default assertions).
- Implementation steps:
  1. Add `vmAgentUser`/`vmAgentHome` consts; `vmWriteFilesOwner = root:root`, `vmSecretFileOwner = agent:agent`.
  2. Rewrite cloud-init `users:` stanza to provision the agent user with the SSH key; write secrets root-owned; append `runcmd` chown.
  3. Default `SSHUser` to `agent`.
  4. Add `TestBuildVMManifest_CreatesAgentUserForHomeParity` + update owner/SSHUser assertions.

## Prove

- Tests run (all green in worktree):
  - `go test ./internal/devbox/backend/` → `ok` (~2.5s).
  - `go vet ./internal/devbox/backend/` → clean.
  - `gofmt -l` on touched files → clean.
  - `go build ./...` → exit 0 (benign macOS linker version warnings only).
- Riskiest-assumption discipline (rule fired):
  - **Discovery 1**: cloud-init `write-files` runs *before* `users-groups` — the agent user does not exist when secret files are written, so writing them `agent:agent` would fail. Fix: write root-owned, chown later.
  - **Discovery 2**: `bootcmd`-precreated users get `ssh_authorized_keys` *skipped* by the `users:` module in some cloud-init versions → broken SSH login. Fix: let the `users:` stanza create the user canonically (reliable key application).

## Handoff/Harvest

- Docs updated:
  - `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md` — Slice 2d.5c marked SHIPPED with the two discoveries + kill-test owed.
- **Kill-test still owed** (BLOCKS declaring codex unblocked on harvester-vm): boot a VM, SSH as `agent`, confirm `~/.codex/auth.json` resolves to the mounted file and a codex invocation authenticates. Unit tests only pin manifest shape.
- Next-slice candidates:
  - Run the live home-parity kill-test and record evidence.
  - Wire harvester-vm into the Mills backend selector end-to-end.
