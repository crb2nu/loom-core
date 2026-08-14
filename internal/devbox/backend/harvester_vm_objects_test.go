package backend

import (
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// validHarvesterCfg returns a HarvesterVMBackendConfig that satisfies
// buildVMManifest's required fields. Tests mutate the returned struct to
// exercise individual behaviors without re-stating the shared boilerplate.
func validHarvesterCfg() HarvesterVMBackendConfig {
	return HarvesterVMBackendConfig{
		Namespace:            "default",
		StorageClassName:     "longhorn-image-abc123",
		NetworkAttachmentDef: "default/lan10g",
		BaseImageName:        "mills-devbox-base-2026-05-25",
		SSHUser:              "agent",
	}
}

// testPubKey is a syntactically valid SSH ed25519 authorized_keys string
// used by the manifest builder tests. The builder treats it as opaque, so
// the actual key bytes don't matter — only that the value is non-empty
// and appears verbatim inside the cloud-init blob.
const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKeyBytesForUnitTestsOnly mills-devbox"

func TestBuildVMManifest_HappyPath(t *testing.T) {
	cfg := validHarvesterCfg()
	opts := StartOpts{Name: "test-vm-1"}

	objs, err := buildVMManifest(opts, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	if objs == nil || objs.PVC == nil || objs.VM == nil {
		t.Fatalf("buildVMManifest returned incomplete objects: %#v", objs)
	}

	t.Run("PVC name and namespace", func(t *testing.T) {
		if got := objs.PVC.GetName(); got != "test-vm-1-os" {
			t.Errorf("PVC name = %q, want %q", got, "test-vm-1-os")
		}
		if got := objs.PVC.GetNamespace(); got != "default" {
			t.Errorf("PVC namespace = %q, want %q", got, "default")
		}
	})

	t.Run("PVC spec defaults", func(t *testing.T) {
		spec := mustGetMap(t, objs.PVC.Object, "spec")
		accessModes := mustGetSlice(t, spec, "accessModes")
		if len(accessModes) != 1 || accessModes[0] != "ReadWriteMany" {
			t.Errorf("accessModes = %v, want [ReadWriteMany]", accessModes)
		}
		if sc, _, _ := unstructured.NestedString(objs.PVC.Object, "spec", "storageClassName"); sc != "longhorn-image-abc123" {
			t.Errorf("storageClassName = %q, want %q", sc, "longhorn-image-abc123")
		}
		if vm, _, _ := unstructured.NestedString(objs.PVC.Object, "spec", "volumeMode"); vm != "Block" {
			t.Errorf("volumeMode = %q, want %q", vm, "Block")
		}
		if size, _, _ := unstructured.NestedString(objs.PVC.Object, "spec", "resources", "requests", "storage"); size != "20Gi" {
			t.Errorf("storage size = %q, want %q", size, "20Gi")
		}
	})

	t.Run("VM metadata", func(t *testing.T) {
		if got := objs.VM.GetName(); got != "test-vm-1" {
			t.Errorf("VM name = %q, want %q", got, "test-vm-1")
		}
		if got := objs.VM.GetNamespace(); got != "default" {
			t.Errorf("VM namespace = %q, want %q", got, "default")
		}
		if got := objs.VM.GetAPIVersion(); got != "kubevirt.io/v1" {
			t.Errorf("VM apiVersion = %q, want %q", got, "kubevirt.io/v1")
		}
		if got := objs.VM.GetKind(); got != "VirtualMachine" {
			t.Errorf("VM kind = %q, want %q", got, "VirtualMachine")
		}
	})

	t.Run("VM labels include managed-by + app", func(t *testing.T) {
		labels := objs.VM.GetLabels()
		if labels["app.kubernetes.io/managed-by"] != harvesterManagedByLabel {
			t.Errorf("managed-by label = %q, want %q", labels["app.kubernetes.io/managed-by"], harvesterManagedByLabel)
		}
		if labels["app"] != "test-vm-1" {
			t.Errorf("app label = %q, want %q", labels["app"], "test-vm-1")
		}
	})

	t.Run("VM runStrategy", func(t *testing.T) {
		rs, _, _ := unstructured.NestedString(objs.VM.Object, "spec", "runStrategy")
		if rs != "RerunOnFailure" {
			t.Errorf("runStrategy = %q, want %q", rs, "RerunOnFailure")
		}
	})

	t.Run("VM domain.cpu.cores default", func(t *testing.T) {
		cores, found, err := unstructured.NestedInt64(objs.VM.Object,
			"spec", "template", "spec", "domain", "cpu", "cores")
		if err != nil || !found {
			t.Fatalf("cpu.cores not found: found=%v err=%v", found, err)
		}
		if cores != defaultHarvesterVCPUs {
			t.Errorf("cores = %d, want %d", cores, defaultHarvesterVCPUs)
		}
	})

	t.Run("VM domain.resources.requests.memory default", func(t *testing.T) {
		mem, _, _ := unstructured.NestedString(objs.VM.Object,
			"spec", "template", "spec", "domain", "resources", "requests", "memory")
		if mem != "4096Mi" {
			t.Errorf("memory = %q, want %q", mem, "4096Mi")
		}
	})

	t.Run("VM machine.type", func(t *testing.T) {
		mt, _, _ := unstructured.NestedString(objs.VM.Object,
			"spec", "template", "spec", "domain", "machine", "type")
		if mt != "q35" {
			t.Errorf("machine.type = %q, want %q", mt, "q35")
		}
	})

	t.Run("VM terminationGracePeriodSeconds", func(t *testing.T) {
		grace, found, err := unstructured.NestedInt64(objs.VM.Object,
			"spec", "template", "spec", "terminationGracePeriodSeconds")
		if err != nil || !found {
			t.Fatalf("grace period not found: found=%v err=%v", found, err)
		}
		if grace != 60 {
			t.Errorf("grace period = %d, want 60", grace)
		}
	})

	t.Run("VM disks include os + cloudinit on virtio bus", func(t *testing.T) {
		disks := mustGetSliceAt(t, objs.VM.Object,
			"spec", "template", "spec", "domain", "devices", "disks")
		if len(disks) != 2 {
			t.Fatalf("disks length = %d, want 2", len(disks))
		}
		seen := map[string]string{}
		for _, raw := range disks {
			d, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("disk entry has wrong type: %T", raw)
			}
			name, _ := d["name"].(string)
			diskMap, _ := d["disk"].(map[string]any)
			bus, _ := diskMap["bus"].(string)
			seen[name] = bus
		}
		if seen["os"] != "virtio" {
			t.Errorf("os disk bus = %q, want virtio", seen["os"])
		}
		if seen["cloudinit"] != "virtio" {
			t.Errorf("cloudinit disk bus = %q, want virtio", seen["cloudinit"])
		}
	})

	t.Run("VM interface uses bridge + virtio on configured NAD", func(t *testing.T) {
		ifaces := mustGetSliceAt(t, objs.VM.Object,
			"spec", "template", "spec", "domain", "devices", "interfaces")
		if len(ifaces) != 1 {
			t.Fatalf("interfaces length = %d, want 1", len(ifaces))
		}
		i, ok := ifaces[0].(map[string]any)
		if !ok {
			t.Fatalf("interface has wrong type: %T", ifaces[0])
		}
		if i["name"] != "lan" {
			t.Errorf("interface name = %v, want lan", i["name"])
		}
		if i["model"] != "virtio" {
			t.Errorf("interface model = %v, want virtio", i["model"])
		}
		if _, ok := i["bridge"].(map[string]any); !ok {
			t.Errorf("interface missing bridge: %#v", i)
		}

		networks := mustGetSliceAt(t, objs.VM.Object,
			"spec", "template", "spec", "networks")
		if len(networks) != 1 {
			t.Fatalf("networks length = %d, want 1", len(networks))
		}
		net0, ok := networks[0].(map[string]any)
		if !ok {
			t.Fatalf("network has wrong type: %T", networks[0])
		}
		if net0["name"] != "lan" {
			t.Errorf("network name = %v, want lan", net0["name"])
		}
		multus, _ := net0["multus"].(map[string]any)
		if got, _ := multus["networkName"].(string); got != cfg.NetworkAttachmentDef {
			t.Errorf("multus networkName = %q, want %q", got, cfg.NetworkAttachmentDef)
		}
	})

	t.Run("VM volumes reference PVC and cloudInit", func(t *testing.T) {
		volumes := mustGetSliceAt(t, objs.VM.Object,
			"spec", "template", "spec", "volumes")
		if len(volumes) != 2 {
			t.Fatalf("volumes length = %d, want 2", len(volumes))
		}
		var osVol, ciVol map[string]any
		for _, raw := range volumes {
			v, _ := raw.(map[string]any)
			switch v["name"] {
			case "os":
				osVol = v
			case "cloudinit":
				ciVol = v
			}
		}
		if osVol == nil {
			t.Fatal("os volume missing")
		}
		pvcRef, _ := osVol["persistentVolumeClaim"].(map[string]any)
		if claim, _ := pvcRef["claimName"].(string); claim != "test-vm-1-os" {
			t.Errorf("os volume claimName = %q, want %q", claim, "test-vm-1-os")
		}

		if ciVol == nil {
			t.Fatal("cloudinit volume missing")
		}
		ciData, _ := ciVol["cloudInitNoCloud"].(map[string]any)
		ref, _ := ciData["secretRef"].(map[string]any)
		if name, _ := ref["name"].(string); name != "test-vm-1-cloudinit" {
			t.Errorf("cloudinit volume secretRef.name = %q, want %q", name, "test-vm-1-cloudinit")
		}
		userData := extractUserData(t, objs)
		if !strings.Contains(userData, testPubKey) {
			t.Errorf("userData does not contain pubkey: %s", userData)
		}
		if !strings.Contains(userData, "systemctl enable --now ssh") &&
			!strings.Contains(userData, "systemctl, enable, --now, ssh") {
			t.Errorf("userData missing ssh enable directive: %s", userData)
		}
		if !strings.Contains(userData, "qemu-guest-agent") {
			t.Errorf("userData missing qemu-guest-agent directive: %s", userData)
		}
		if !strings.Contains(userData, "hostname: test-vm-1") {
			t.Errorf("userData missing hostname: %s", userData)
		}
	})

	t.Run("VM template labels carry app + kubevirt.io/vm", func(t *testing.T) {
		tplLabels, _, _ := unstructured.NestedMap(objs.VM.Object,
			"spec", "template", "metadata", "labels")
		if tplLabels["app"] != "test-vm-1" {
			t.Errorf("template app label = %v, want test-vm-1", tplLabels["app"])
		}
		if tplLabels["kubevirt.io/vm"] != "test-vm-1" {
			t.Errorf("template kubevirt.io/vm label = %v, want test-vm-1", tplLabels["kubevirt.io/vm"])
		}
		if tplLabels["app.kubernetes.io/managed-by"] != harvesterManagedByLabel {
			t.Errorf("template managed-by label = %v, want %q",
				tplLabels["app.kubernetes.io/managed-by"], harvesterManagedByLabel)
		}
	})
}

func TestBuildVMManifest_ResourceOverridesFromStartOpts(t *testing.T) {
	// StartOpts has Memory + CPU fields exposed by the Backend interface
	// (StartOpts.MemoryMB, StartOpts.CPUs). The harvester-vm builder
	// currently sources vCPUs/Mem/Disk from the cfg defaults; per-VM
	// overrides happen at the config level. This test pins the contract:
	// changing cfg.DefaultVCPUs/MemMi/DiskGi flows through to the
	// manifest.
	cfg := validHarvesterCfg()
	cfg.DefaultVCPUs = 8
	cfg.DefaultMemMi = 16384
	cfg.DefaultDiskGi = 100

	objs, err := buildVMManifest(StartOpts{Name: "big-vm"}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}

	cores, _, _ := unstructured.NestedInt64(objs.VM.Object,
		"spec", "template", "spec", "domain", "cpu", "cores")
	if cores != 8 {
		t.Errorf("cores = %d, want 8", cores)
	}
	mem, _, _ := unstructured.NestedString(objs.VM.Object,
		"spec", "template", "spec", "domain", "resources", "requests", "memory")
	if mem != "16384Mi" {
		t.Errorf("memory = %q, want 16384Mi", mem)
	}
	disk, _, _ := unstructured.NestedString(objs.PVC.Object,
		"spec", "resources", "requests", "storage")
	if disk != "100Gi" {
		t.Errorf("disk = %q, want 100Gi", disk)
	}
}

func TestBuildVMManifest_CustomNetworkAttachmentDef(t *testing.T) {
	cfg := validHarvesterCfg()
	cfg.NetworkAttachmentDef = "custom-ns/mgmt-vlan"

	objs, err := buildVMManifest(StartOpts{Name: "net-vm"}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}

	networks := mustGetSliceAt(t, objs.VM.Object,
		"spec", "template", "spec", "networks")
	if len(networks) != 1 {
		t.Fatalf("networks length = %d, want 1", len(networks))
	}
	net0, _ := networks[0].(map[string]any)
	multus, _ := net0["multus"].(map[string]any)
	if got, _ := multus["networkName"].(string); got != "custom-ns/mgmt-vlan" {
		t.Errorf("multus networkName = %q, want %q", got, "custom-ns/mgmt-vlan")
	}
}

func TestBuildVMManifest_RequiredFieldErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*StartOpts, *HarvesterVMBackendConfig, *string)
		wantSub string
	}{
		{
			name: "empty StartOpts.Name",
			mutate: func(opts *StartOpts, _ *HarvesterVMBackendConfig, _ *string) {
				opts.Name = ""
			},
			wantSub: "StartOpts.Name is required",
		},
		{
			name: "empty Namespace",
			mutate: func(_ *StartOpts, cfg *HarvesterVMBackendConfig, _ *string) {
				cfg.Namespace = ""
			},
			wantSub: "Namespace is required",
		},
		{
			name: "empty StorageClassName",
			mutate: func(_ *StartOpts, cfg *HarvesterVMBackendConfig, _ *string) {
				cfg.StorageClassName = ""
			},
			wantSub: "StorageClassName is required",
		},
		{
			name: "empty NetworkAttachmentDef",
			mutate: func(_ *StartOpts, cfg *HarvesterVMBackendConfig, _ *string) {
				cfg.NetworkAttachmentDef = ""
			},
			wantSub: "NetworkAttachmentDef is required",
		},
		{
			name: "empty sshPubKey",
			mutate: func(_ *StartOpts, _ *HarvesterVMBackendConfig, pk *string) {
				*pk = ""
			},
			wantSub: "sshPubKey is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := StartOpts{Name: "vm"}
			cfg := validHarvesterCfg()
			pk := testPubKey
			tc.mutate(&opts, &cfg, &pk)

			_, err := buildVMManifest(opts, cfg, pk, nil, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestBuildVMManifest_EmptyBaseImageNameIsAccepted(t *testing.T) {
	// BaseImageName is informational — the storage class is what drives
	// actual disk hydration. An empty BaseImageName must not break the
	// manifest builder; it surfaces elsewhere (Build returns the cfg
	// fallback tag).
	cfg := validHarvesterCfg()
	cfg.BaseImageName = ""

	objs, err := buildVMManifest(StartOpts{Name: "no-base"}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	if objs == nil {
		t.Fatal("expected non-nil objs")
	}
}

func TestBuildVMManifest_AgentIDPropagatesAsLabel(t *testing.T) {
	cfg := validHarvesterCfg()
	objs, err := buildVMManifest(StartOpts{
		Name:    "labeled-vm",
		AgentID: "claude-code-42",
	}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}

	labels := objs.VM.GetLabels()
	if labels["devbox/agent-id"] != "claude-code-42" {
		t.Errorf("devbox/agent-id label = %q, want %q",
			labels["devbox/agent-id"], "claude-code-42")
	}
	pvcLabels := objs.PVC.GetLabels()
	if pvcLabels["devbox/agent-id"] != "claude-code-42" {
		t.Errorf("PVC devbox/agent-id label = %q, want %q",
			pvcLabels["devbox/agent-id"], "claude-code-42")
	}
}

func TestBuildVMManifest_ManagedByOverride(t *testing.T) {
	// Spawn pods set ManagedByOverride to surface a different
	// app.kubernetes.io/managed-by value so the reconciler can scope
	// queries. The override must flow through to both PVC + VM labels.
	cfg := validHarvesterCfg()
	objs, err := buildVMManifest(StartOpts{
		Name:              "spawn-vm",
		ManagedByOverride: "loom-spawn",
	}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	if got := objs.VM.GetLabels()["app.kubernetes.io/managed-by"]; got != "loom-spawn" {
		t.Errorf("VM managed-by = %q, want loom-spawn", got)
	}
	if got := objs.PVC.GetLabels()["app.kubernetes.io/managed-by"]; got != "loom-spawn" {
		t.Errorf("PVC managed-by = %q, want loom-spawn", got)
	}
}

func TestBuildVMManifest_ExtraLabelsMerge(t *testing.T) {
	cfg := validHarvesterCfg()
	objs, err := buildVMManifest(StartOpts{
		Name:        "labeled-vm",
		ExtraLabels: map[string]string{"team": "platform", "tier": "test"},
	}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	labels := objs.VM.GetLabels()
	if labels["team"] != "platform" || labels["tier"] != "test" {
		t.Errorf("extra labels missing: %#v", labels)
	}
}

func TestBuildVMManifest_StableNamesForRepeatedBuilds(t *testing.T) {
	// Calling buildVMManifest twice with identical StartOpts must produce
	// identical names. Slice 1.5 confirmed the reconciler's reuse logic
	// depends on deterministic naming: PVC = <name>-os, VM = <name>.
	cfg := validHarvesterCfg()
	opts := StartOpts{Name: "stable-vm"}

	a, err := buildVMManifest(opts, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	b, err := buildVMManifest(opts, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a.PVC.GetName() != b.PVC.GetName() {
		t.Errorf("PVC names differ: %q vs %q", a.PVC.GetName(), b.PVC.GetName())
	}
	if a.VM.GetName() != b.VM.GetName() {
		t.Errorf("VM names differ: %q vs %q", a.VM.GetName(), b.VM.GetName())
	}
	if a.PVC.GetName() != "stable-vm-os" {
		t.Errorf("PVC name = %q, want stable-vm-os", a.PVC.GetName())
	}
}

func TestApplyOwnerReference_PatchesPVCWithVMRef(t *testing.T) {
	// Owner reference is applied AFTER the VM Create returns (so the UID
	// is populated). We assert apiVersion + kind + name + controller flag
	// are correct; UID round-trips through GetUID/SetUID.
	cfg := validHarvesterCfg()
	objs, err := buildVMManifest(StartOpts{Name: "ownerref-vm"}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest: %v", err)
	}

	// Simulate API-server fill-in of UID after Create.
	objs.VM.SetUID(types.UID("vm-uid-42"))

	applyOwnerReference(objs.PVC, objs.VM)

	owners := objs.PVC.GetOwnerReferences()
	if len(owners) != 1 {
		t.Fatalf("ownerReferences length = %d, want 1", len(owners))
	}
	o := owners[0]
	if o.APIVersion != "kubevirt.io/v1" {
		t.Errorf("owner apiVersion = %q, want kubevirt.io/v1", o.APIVersion)
	}
	if o.Kind != "VirtualMachine" {
		t.Errorf("owner kind = %q, want VirtualMachine", o.Kind)
	}
	if o.Name != "ownerref-vm" {
		t.Errorf("owner name = %q, want ownerref-vm", o.Name)
	}
	if string(o.UID) != "vm-uid-42" {
		t.Errorf("owner uid = %q, want vm-uid-42", o.UID)
	}
	if o.Controller == nil || !*o.Controller {
		t.Errorf("owner controller = %v, want true", o.Controller)
	}
	if o.BlockOwnerDeletion == nil || !*o.BlockOwnerDeletion {
		t.Errorf("owner blockOwnerDeletion = %v, want true", o.BlockOwnerDeletion)
	}
}

func TestApplyOwnerReference_HandlesEmptyMetadata(t *testing.T) {
	// applyOwnerReference must not panic when the PVC's metadata map is
	// missing. The defensive nil-check in the implementation backfills
	// metadata before writing ownerReferences.
	pvc := &unstructured.Unstructured{Object: map[string]any{}}
	vm := &unstructured.Unstructured{Object: map[string]any{}}
	vm.SetName("vm-x")
	vm.SetUID(types.UID("uid-x"))

	applyOwnerReference(pvc, vm)

	owners := pvc.GetOwnerReferences()
	if len(owners) != 1 {
		t.Fatalf("ownerReferences length = %d, want 1", len(owners))
	}
}

// ---------- helpers ----------

func mustGetMap(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := obj[key].(map[string]any)
	if !ok {
		t.Fatalf("key %q is not map[string]any: %T", key, obj[key])
	}
	return v
}

func mustGetSlice(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	v, ok := obj[key].([]any)
	if !ok {
		t.Fatalf("key %q is not []any: %T", key, obj[key])
	}
	return v
}

func mustGetSliceAt(t *testing.T, root map[string]any, path ...string) []any {
	t.Helper()
	v, found, err := unstructured.NestedSlice(root, path...)
	if err != nil || !found {
		t.Fatalf("path %v not found or wrong type: found=%v err=%v", path, found, err)
	}
	return v
}

func TestBuildVMManifest_RendersEnvIntoCloudInit(t *testing.T) {
	cfg := validHarvesterCfg()
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-test-token",
		"DEVBOX_BACKEND":    "harvester-vm",
		"WITH_QUOTE":        "it's-quoted",
	}
	objs, err := buildVMManifest(StartOpts{Name: "env-vm"}, cfg, testPubKey, env, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}

	userData := extractUserData(t, objs)

	// Sorted KEY='value' entries, single-quoted, with POSIX escape for `'`.
	wantSubstrings := []string{
		"write_files:",
		"path: /etc/loom-spawn.env",
		"ANTHROPIC_API_KEY='sk-ant-test-token'",
		"DEVBOX_BACKEND='harvester-vm'",
		`WITH_QUOTE='it'\''s-quoted'`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(userData, want) {
			t.Errorf("userData missing %q\n--- userData ---\n%s", want, userData)
		}
	}
}

func TestBuildVMManifest_RejectsNewlineEnv(t *testing.T) {
	cfg := validHarvesterCfg()
	env := map[string]string{"BAD": "line1\nline2"}
	_, err := buildVMManifest(StartOpts{Name: "bad-env"}, cfg, testPubKey, env, nil)
	if err == nil {
		t.Fatalf("expected error for newline-bearing env value, got nil")
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("error message should mention newline; got %q", err.Error())
	}
}

func TestBuildVMManifest_EmptyEnvOmitsWriteFiles(t *testing.T) {
	cfg := validHarvesterCfg()
	objs, err := buildVMManifest(StartOpts{Name: "no-env-vm"}, cfg, testPubKey, nil, nil)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	ud := extractUserData(t, objs)
	if strings.Contains(ud, "write_files") {
		t.Errorf("expected no write_files block when env is empty; got userData:\n%s", ud)
	}
}

func TestRenderCloudInitWriteFiles_EmptyInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		files []ResolvedSecretFile
	}{
		{"nil-nil", nil, nil},
		{"empty-env-nil-files", map[string]string{}, nil},
		{"all-empty-env-keys", map[string]string{"": "value"}, nil},
		{"empty-files-slice", nil, []ResolvedSecretFile{}},
		{"files-with-empty-paths-only", nil, []ResolvedSecretFile{{Path: "", Content: []byte("x")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderCloudInitWriteFiles(tc.env, tc.files)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != "" {
				t.Errorf("expected empty block, got %q", out)
			}
		})
	}
}

func TestBuildVMManifest_RendersSecretFilesIntoCloudInit(t *testing.T) {
	cfg := validHarvesterCfg()
	files := []ResolvedSecretFile{
		{
			// Out-of-order on purpose — render must sort by Path.
			Path:    "/home/agent/.gcp/sa.json",
			Content: []byte(`{"type":"service_account","project_id":"x"}`),
		},
		{
			Path:    "/home/agent/.claude.auth/oauth.json",
			Content: []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oauth"}}`),
			Mode:    "0400",
		},
		{
			// Binary blob to prove encoding: b64 is binary-safe.
			Path:    "/etc/binary-blob",
			Content: []byte{0x00, 0xff, 0x10, 0x7f, 0x80, 0x0a, 0x00},
		},
	}
	objs, err := buildVMManifest(StartOpts{Name: "secret-vm"}, cfg, testPubKey, nil, files)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}

	userData := extractUserData(t, objs)

	for i, f := range files {
		encoded := base64.StdEncoding.EncodeToString(f.Content)
		wantSubstrings := []string{
			"path: " + f.Path,
			// Files are written root-owned; a runcmd chown hands /home/agent
			// to the agent after the users-groups module creates the user.
			"owner: " + vmWriteFilesOwner,
			"encoding: b64",
			"content: " + encoded,
		}
		for _, want := range wantSubstrings {
			if !strings.Contains(userData, want) {
				t.Errorf("file[%d] %s: userData missing %q\n--- userData ---\n%s",
					i, f.Path, want, userData)
			}
		}
	}

	if c := strings.Count(userData, "write_files:"); c != 1 {
		t.Errorf("expected exactly one write_files: block, got %d\n--- userData ---\n%s", c, userData)
	}

	// Default mode 0600 when ResolvedSecretFile.Mode is empty; honored mode
	// override (0400) survives the render.
	if !strings.Contains(userData, "permissions: '0600'") {
		t.Errorf("expected default permissions 0600 for unset Mode; got userData:\n%s", userData)
	}
	if !strings.Contains(userData, "permissions: '0400'") {
		t.Errorf("expected permissions 0400 from explicit Mode; got userData:\n%s", userData)
	}

	// Deterministic Path sort: /etc/binary-blob precedes the /home/agent paths
	// in the rendered output.
	binaryIdx := strings.Index(userData, "path: /etc/binary-blob")
	claudeIdx := strings.Index(userData, "path: /home/agent/.claude.auth/oauth.json")
	gcpIdx := strings.Index(userData, "path: /home/agent/.gcp/sa.json")
	if binaryIdx < 0 || binaryIdx >= claudeIdx || claudeIdx >= gcpIdx {
		t.Errorf("write_files entries not sorted by Path (binary=%d claude=%d gcp=%d)",
			binaryIdx, claudeIdx, gcpIdx)
	}
}

func TestBuildVMManifest_MergesEnvAndFilesIntoSingleBlock(t *testing.T) {
	cfg := validHarvesterCfg()
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-merged"}
	files := []ResolvedSecretFile{
		{Path: "/home/agent/.codex.auth/auth.json", Content: []byte(`{"token":"x"}`)},
	}
	objs, err := buildVMManifest(StartOpts{Name: "merged-vm"}, cfg, testPubKey, env, files)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	userData := extractUserData(t, objs)

	if c := strings.Count(userData, "write_files:"); c != 1 {
		t.Errorf("env + files must share one write_files block; got %d blocks\n%s", c, userData)
	}
	for _, want := range []string{
		"path: /etc/loom-spawn.env",
		"ANTHROPIC_API_KEY='sk-ant-merged'",
		"path: /home/agent/.codex.auth/auth.json",
		"encoding: b64",
	} {
		if !strings.Contains(userData, want) {
			t.Errorf("merged block missing %q\n--- userData ---\n%s", want, userData)
		}
	}
}

func TestBuildVMManifest_FilesOnlyOmitsEnvEntry(t *testing.T) {
	cfg := validHarvesterCfg()
	files := []ResolvedSecretFile{
		{Path: "/home/agent/.codex.auth/auth.json", Content: []byte(`{"token":"x"}`)},
	}
	objs, err := buildVMManifest(StartOpts{Name: "files-only-vm"}, cfg, testPubKey, nil, files)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	userData := extractUserData(t, objs)

	if !strings.Contains(userData, "write_files:") {
		t.Errorf("expected write_files block; got userData:\n%s", userData)
	}
	if strings.Contains(userData, "path: /etc/loom-spawn.env") {
		t.Errorf("files-only render must not include env file entry; got userData:\n%s", userData)
	}
}

// TestBuildVMManifest_CreatesAgentUserForHomeParity pins the Slice 2d.5c
// behavior: the VM must provision an `agent` user with HOME=/home/agent so
// SecretMount paths and injectAgentConfig writes (all hardcoded to
// /home/agent in internal/hud/spawn.go) resolve to the SSH user's home dir.
func TestBuildVMManifest_CreatesAgentUserForHomeParity(t *testing.T) {
	cfg := validHarvesterCfg()
	files := []ResolvedSecretFile{
		{Path: "/home/agent/.codex.auth/auth.json", Content: []byte(`{"token":"x"}`)},
	}
	objs, err := buildVMManifest(StartOpts{Name: "parity-vm"}, cfg, testPubKey, nil, files)
	if err != nil {
		t.Fatalf("buildVMManifest returned error: %v", err)
	}
	userData := extractUserData(t, objs)

	// The login user is `agent`, not the base image's `ubuntu`.
	if !strings.Contains(userData, "name: agent") {
		t.Errorf("users: stanza must define the agent user\n--- userData ---\n%s", userData)
	}
	if strings.Contains(userData, "name: ubuntu") {
		t.Errorf("users: stanza must not define the ubuntu user (Slice 2d.5c)\n--- userData ---\n%s", userData)
	}
	if !strings.Contains(userData, "home: /home/agent") {
		t.Errorf("agent user must have HOME=/home/agent\n--- userData ---\n%s", userData)
	}

	// Secret files are written root-owned because the agent user does not
	// exist yet during cloud-init's write-files module (which runs before
	// users-groups). A regression that writes them owner: agent:agent would
	// make cloud-init fail the chown and potentially abort the write.
	if !strings.Contains(userData, "owner: root:root") {
		t.Errorf("secret files must be written root-owned\n--- userData ---\n%s", userData)
	}
	if strings.Contains(userData, "owner: agent:agent") {
		t.Errorf("write_files must not use owner: agent:agent (agent not created until users-groups)\n--- userData ---\n%s", userData)
	}

	// A runcmd chown hands /home/agent (and the secret files under it) to the
	// agent after the users-groups module has created the user. This is what
	// makes the mounted credentials readable by the agent CLIs at exec time.
	if !strings.Contains(userData, "[ chown, -R, agent:agent, /home/agent ]") {
		t.Errorf("runcmd must chown /home/agent to the agent user\n--- userData ---\n%s", userData)
	}

	// Self-heal qemu-guest-agent on stock-Ubuntu base images that lack it.
	// Without the agent KubeVirt never reports the VM's DHCP IP and
	// Start.waitForVMReady times out (Slice 2d.5d kill-test evidence). The
	// `command -v qemu-ga` guard makes it a no-op on the curated image.
	if !strings.Contains(userData, "command -v qemu-ga") || !strings.Contains(userData, "apt-get install -y qemu-guest-agent") {
		t.Errorf("runcmd must install qemu-guest-agent when missing\n--- userData ---\n%s", userData)
	}
}

// extractUserData pulls the cloud-init userData string out of the rendered
// VirtualMachine manifest. Shared across the secret-file rendering tests.
// extractUserData returns the cloud-init userData. Since Slice 2d.5d it lives
// in the out-of-line CloudInitSecret (cloudInitNoCloud.secretRef), not inline
// on the VM volume. Also asserts the VM volume references that Secret by name.
func extractUserData(t *testing.T, objs *harvesterVMObjects) string {
	t.Helper()
	if objs.CloudInitSecret == nil {
		t.Fatalf("CloudInitSecret is nil; expected out-of-line cloud-init")
	}
	// The VM's cloudinit volume must reference the Secret by name.
	wantRef := objs.CloudInitSecret.GetName()
	volumes := mustGetSliceAt(t, objs.VM.Object, "spec", "template", "spec", "volumes")
	foundRef := false
	for _, v := range volumes {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		ci, ok := vm["cloudInitNoCloud"].(map[string]any)
		if !ok {
			continue
		}
		ref, _ := ci["secretRef"].(map[string]any)
		if name, _ := ref["name"].(string); name == wantRef {
			foundRef = true
		}
		if ud, _ := ci["userData"].(string); ud != "" {
			t.Fatalf("inline userData present (%d bytes); must use secretRef (KubeVirt 2048-byte cap)", len(ud))
		}
	}
	if !foundRef {
		t.Fatalf("VM cloudinit volume does not reference secret %q via secretRef", wantRef)
	}
	sd, _, err := unstructured.NestedStringMap(objs.CloudInitSecret.Object, "stringData")
	if err != nil {
		t.Fatalf("read CloudInitSecret stringData: %v", err)
	}
	ud, ok := sd["userdata"]
	if !ok || ud == "" {
		t.Fatalf("CloudInitSecret stringData missing non-empty 'userdata' key")
	}
	return ud
}
