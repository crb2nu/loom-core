package backend

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// vmEnvFilePath is the in-VM path the cloud-init write_files entry lands
// and HarvesterVMBackend.Exec sources before applying its own ExecOpts.Env
// shell prefix. Out-of-band SSH sessions can also `source` this file for
// parity with the orchestrator's exec environment.
const vmEnvFilePath = "/etc/loom-spawn.env"

// KubeVirt API GVRs used by this backend.
//
// Pinned to v1 — the spec is built against KubeVirt v1.4.0 (Harvester
// v1.5.1), which serves both `kubevirt.io/v1` and the legacy
// `kubevirt.io/v1alpha3`. We use v1 to match the operator's preferred
// storage version.
var (
	vmGVR = schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachines",
	}
	vmiGVR = schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachineinstances",
	}
	pvcGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "persistentvolumeclaims",
	}
)

// harvesterVMObjects bundles the manifests produced by buildVMManifest:
// a per-VM PVC (block-mode, Longhorn-image-backed) and a VirtualMachine
// CR with `runStrategy: RerunOnFailure` and cloud-init that injects the
// per-VM SSH pubkey.
//
// Both manifests share `app=<name>` + `app.kubernetes.io/managed-by` labels
// so list/cleanup operations can scope by label selector. The PVC declares
// the VM as an owner reference so VM deletion cascades the PVC.
type harvesterVMObjects struct {
	PVC *unstructured.Unstructured
	VM  *unstructured.Unstructured
}

// buildVMManifest constructs the per-VM PVC + VirtualMachine manifests.
// Pure function — no cluster calls, easy to unit test.
//
// `sshPubKey` is the ssh.MarshalAuthorizedKey output for the per-VM
// ed25519 keypair (e.g., "ssh-ed25519 AAAA... mills-devbox").
//
// `env` is the merged StartOpts.Env + resolved SecretEnv map. When
// non-empty, cloud-init writes a sourceable `KEY='value'` file at
// vmEnvFilePath so the orchestrator's Exec shell prefix can pick it up.
// Newline-bearing values are rejected to keep the cloud-init YAML clean
// (legitimate env-var values don't contain `\n`, and accepting them would
// let a malformed secret bleed into the manifest).
//
// Cloud-init injects the pubkey into the `ubuntu` user's
// authorized_keys, disables password auth, and defensively re-enables
// qemu-guest-agent + ssh even though the pre-baked image already has
// them enabled (Slice 1.5 evidence:
// `.loom/local/handoffs/mills-harvester-vm-slice15-2026-05-25.md`).
func buildVMManifest(opts StartOpts, cfg HarvesterVMBackendConfig, sshPubKey string, env map[string]string) (*harvesterVMObjects, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("StartOpts.Name is required")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("HarvesterVMBackendConfig.Namespace is required")
	}
	if cfg.StorageClassName == "" {
		return nil, fmt.Errorf("HarvesterVMBackendConfig.StorageClassName is required")
	}
	if cfg.NetworkAttachmentDef == "" {
		return nil, fmt.Errorf("HarvesterVMBackendConfig.NetworkAttachmentDef is required")
	}
	if sshPubKey == "" {
		return nil, fmt.Errorf("sshPubKey is required")
	}
	envBlock, err := renderCloudInitEnvBlock(env)
	if err != nil {
		return nil, err
	}

	vcpus := cfg.DefaultVCPUs
	if vcpus <= 0 {
		vcpus = 2
	}
	memMi := cfg.DefaultMemMi
	if memMi <= 0 {
		memMi = 4096
	}
	diskGi := cfg.DefaultDiskGi
	if diskGi <= 0 {
		diskGi = 20
	}

	managedBy := harvesterManagedByLabel
	if opts.ManagedByOverride != "" {
		managedBy = opts.ManagedByOverride
	}

	labels := map[string]any{
		"app":                          opts.Name,
		"app.kubernetes.io/managed-by": managedBy,
	}
	if opts.AgentID != "" {
		labels["devbox/agent-id"] = opts.AgentID
	}
	for k, v := range opts.ExtraLabels {
		labels[k] = v
	}

	pvcName := opts.Name + "-os"

	pvc := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":      pvcName,
				"namespace": cfg.Namespace,
				"labels":    labels,
			},
			"spec": map[string]any{
				"accessModes":      []any{"ReadWriteMany"},
				"storageClassName": cfg.StorageClassName,
				"volumeMode":       "Block",
				"resources": map[string]any{
					"requests": map[string]any{
						"storage": fmt.Sprintf("%dGi", diskGi),
					},
				},
			},
		},
	}

	userData := fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
ssh_pwauth: false
%sruncmd:
  - [ systemctl, enable, --now, qemu-guest-agent ]
  - [ systemctl, enable, --now, ssh ]
`, opts.Name, sshPubKey, envBlock)

	vmTemplateLabels := map[string]any{
		"app":            opts.Name,
		"kubevirt.io/vm": opts.Name,
	}
	for k, v := range labels {
		// Don't shadow the kubevirt.io/vm label; inherit shared ones.
		if _, ok := vmTemplateLabels[k]; !ok {
			vmTemplateLabels[k] = v
		}
	}

	gracePeriod := int64(60)

	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      opts.Name,
				"namespace": cfg.Namespace,
				"labels":    labels,
			},
			"spec": map[string]any{
				"runStrategy": "RerunOnFailure",
				"template": map[string]any{
					"metadata": map[string]any{
						"labels": vmTemplateLabels,
					},
					"spec": map[string]any{
						"terminationGracePeriodSeconds": gracePeriod,
						"domain": map[string]any{
							"cpu": map[string]any{
								"cores": int64(vcpus),
							},
							"machine": map[string]any{
								"type": "q35",
							},
							"resources": map[string]any{
								"requests": map[string]any{
									"memory": fmt.Sprintf("%dMi", memMi),
								},
							},
							"devices": map[string]any{
								"disks": []any{
									map[string]any{
										"name": "os",
										"disk": map[string]any{"bus": "virtio"},
									},
									map[string]any{
										"name": "cloudinit",
										"disk": map[string]any{"bus": "virtio"},
									},
								},
								"interfaces": []any{
									map[string]any{
										"name":   "lan",
										"model":  "virtio",
										"bridge": map[string]any{},
									},
								},
							},
						},
						"networks": []any{
							map[string]any{
								"name": "lan",
								"multus": map[string]any{
									"networkName": cfg.NetworkAttachmentDef,
								},
							},
						},
						"volumes": []any{
							map[string]any{
								"name": "os",
								"persistentVolumeClaim": map[string]any{
									"claimName": pvcName,
								},
							},
							map[string]any{
								"name": "cloudinit",
								"cloudInitNoCloud": map[string]any{
									"userData": userData,
								},
							},
						},
					},
				},
			},
		},
	}

	return &harvesterVMObjects{PVC: pvc, VM: vm}, nil
}

// renderCloudInitEnvBlock returns a `write_files:` block (with a trailing
// newline) that lands `KEY='value'` shell-sourceable lines at
// vmEnvFilePath. Empty input returns an empty string so the calling
// fmt.Sprintf can drop it cleanly between the cloud-init `ssh_pwauth`
// stanza and `runcmd`. Keys are sorted for deterministic output.
//
// Values are single-quoted with embedded single-quotes escaped using the
// POSIX-portable `'\”` sequence. Newline-bearing values are rejected so
// a malformed Secret can't bleed into the cloud-init YAML payload.
func renderCloudInitEnvBlock(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "", nil
	}
	sort.Strings(keys)

	var content strings.Builder
	content.WriteString("# Generated by HarvesterVMBackend; sourced by Exec shell prefix.\n")
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("env value for %q contains a newline; refuse to embed in cloud-init userData", k)
		}
		content.WriteString(k)
		content.WriteString("='")
		content.WriteString(strings.ReplaceAll(v, "'", `'\''`))
		content.WriteString("'\n")
	}

	// Indent every line by 6 spaces so it sits under `      content: |`
	// in the cloud-config YAML below.
	indented := strings.ReplaceAll(content.String(), "\n", "\n      ")
	indented = strings.TrimRight(indented, " ")

	return fmt.Sprintf(`write_files:
  - path: %s
    owner: root:root
    permissions: '0644'
    content: |
      %s`, vmEnvFilePath, indented), nil
}

// applyOwnerReference rewrites the PVC's metadata.ownerReferences so it
// points at the freshly-created VM. Caller invokes this AFTER creating the
// VM (which assigns a UID).
//
// VM deletion cascades the PVC via Kubernetes garbage collection. Without
// this back-reference, the PVC outlives the VM and becomes orphan storage
// — the failure mode that CleanupBuilds exists to mop up.
func applyOwnerReference(pvc *unstructured.Unstructured, vm *unstructured.Unstructured) {
	owner := map[string]any{
		"apiVersion":         "kubevirt.io/v1",
		"kind":               "VirtualMachine",
		"name":               vm.GetName(),
		"uid":                string(vm.GetUID()),
		"controller":         true,
		"blockOwnerDeletion": true,
	}
	md, ok := pvc.Object["metadata"].(map[string]any)
	if !ok {
		md = map[string]any{}
		pvc.Object["metadata"] = md
	}
	md["ownerReferences"] = []any{owner}
}
