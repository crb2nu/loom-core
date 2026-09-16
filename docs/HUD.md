# HUD operations

## Spawn pod storage and readiness

HUD spawn pods (and devbox sandbox pods, which share the same start path)
separate readiness into volume attach/mount, image pull, init-container work,
and container start phases. The overall wait is controlled by
`DEVBOX_K8S_START_TIMEOUT` (five minutes by default). Longhorn can report
`FailedAttachVolume` with an `Aborted` CSI operation while its RWX share manager
attaches; that event is transient, so the waiter continues while the pod is
otherwise progressing. Image pull and container start also have independent
two-minute phase budgets. A running init container — the git clone that
hydrates the workspace — is bounded only by the overall timeout, so a clone
that takes three minutes under load is waited out rather than failed (the
fixed two-minute wait it replaces failed eight consecutive plan-slice attempts
that way on 2026-09-05). Failures include a stable reason and the last
Kubernetes event reason: `attach_wait_exceeded`, `image_pull_backoff`,
`init_wait_exceeded`, or `container_crash`.

In `git-clone` mode, each pod's source workspace is an `emptyDir` populated by
the git-clone init container. It intentionally does not reuse a RWO workspace
claim. Optional cache PVCs remain shared and read-write (the spawn Go cache uses
a Longhorn RWX claim) because their contents
(Go build/module caches and similar dependency caches) are the performance
benefit being requested; replacing them with `emptyDir` would discard that
cache on every spawn. Operators should use a storage mode that supports the
desired concurrency for cache claims. The extended attach phase makes
sequential reuse reliable but does not serialize concurrent writers.
