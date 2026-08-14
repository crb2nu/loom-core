Stop `mcp-ops` from resolving a Harvester/RKE2 request to a k3s kubeconfig, and
wire the `ops-mcp` hub Deployment to an in-cluster kubeconfig
(`cmd/mcp-ops/{main,main_test}.go`, `k8s/base/servers/ops-mcp/{configmap,deployment}.yaml`,
`k8s/hub_manifests_test.go`, platform/gitops `k3s/loom-hub/rbac.yaml`).

`resolveKubeconfig()` tried the explicit value, `$KUBECONFIG`, `K3S_KUBECONFIG`,
then `~/.kube/{k3s.yaml,config}` — one chain, applied inside
`runKubectlWithStderr` to *every* kubectl invocation, including the Harvester
ones that pass `rke2Kubeconfig`: `harvester_vms_list` and the mutating
`harvester_vm_restart`, which patches `spec.running` false then true. So a
`RKE2_KUBECONFIG` pointing at a path that does not exist did not fail — it
silently power-cycled a VM against the **wrong cluster**. It stayed latent in
the hub only because `K3S_KUBECONFIG` there was an equally nonexistent macOS
path, which is exactly why !1460 fixed the other k8s servers and left this one
alone behind a documented `knownBroken` exemption.

Resolution now takes a `targetCluster`, and candidates never cross cluster
boundaries: the RKE2 chain is `explicit → RKE2_KUBECONFIG →
~/.kube/harvester-admin.yaml` and deliberately excludes `$KUBECONFIG` (ambient,
and in practice aimed at k3s), `K3S_KUBECONFIG` and `~/.kube/{k3s.yaml,config}`.
When nothing exists it returns the configured RKE2 value so kubectl emits a
clear "no such file" error. Every call site names its cluster, so a future
Harvester tool cannot silently inherit the k3s chain. k3s resolution is
unchanged and pinned by a table test.

With the Go path failing closed, `ops-mcp` gets the !1460 treatment: an
in-cluster kubeconfig (SA token + CA) from its own ConfigMap mounted at
`/home/mcp/.kube/config`, `K3S_KUBECONFIG` pointed at it, and
`serviceAccountName: loom-hub`. `RKE2_KUBECONFIG` moves to
`/etc/harvester/kubeconfig` — the same reserved mount point mobile-hud and the
parked `k8s-harvester-infra` server use — with nothing mounted there yet, so the
`harvester_*` tools error out plainly instead of answering with k3s. The
`knownBroken` entry is gone; the mount guard keeps checking `ops-mcp`'s k3s
kubeconfig and takes a single narrowly-keyed `reservedMountPoints` exemption for
that one env (keyed by manifest *and* value, so mobile-hud's real mount at the
same path is still verified).

The `loom-hub` ClusterRole was missing two grants `mcp-ops` needs — verified
against the live cluster with `kubectl auth can-i --as`, both `no` before:
`nodes` get/list/watch/patch (`k8s_get_nodes`; `vip_label_node` and
`stabilize_cluster` label a node, which is a PATCH on `/api/v1/nodes/<name>`),
and `deployments/scale` get/update/patch (`kubectl scale` goes through the scale
subresource, a distinct RBAC resource that the existing `deployments` patch
grant does not cover). Note this ClusterRole is shared by every loom-hub server.

Tests: `TestResolveKubeconfigRKE2NeverFallsBackToK3s`,
`TestResolveKubeconfigK3sUnchanged`, and `TestHarvesterHandlersUseRKE2Kubeconfig`
(end-to-end through the kubectl mock, asserting both Harvester tools hand kubectl
the RKE2 path and never the k3s one). All three were kill-tested: reintroducing
the cross-cluster fallback fails them.
