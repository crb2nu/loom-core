package backend

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newFakeHarvesterVMBackend wires up a HarvesterVMBackend with a
// fake dynamic client preloaded with the supplied objects. The discovery
// client and rest config are intentionally left nil — tests that need
// Health/subresources install a stub explicitly via fields they touch.
func newFakeHarvesterVMBackend(t *testing.T, objs ...runtime.Object) (*HarvesterVMBackend, dynamic.Interface) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		vmGVR:  "VirtualMachineList",
		vmiGVR: "VirtualMachineInstanceList",
		pvcGVR: "PersistentVolumeClaimList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
	h := &HarvesterVMBackend{
		cfg: HarvesterVMBackendConfig{
			Namespace:            "default",
			StorageClassName:     "longhorn-image-abc",
			NetworkAttachmentDef: "default/lan10g",
			SSHUser:              "agent",
			DefaultVCPUs:         defaultHarvesterVCPUs,
			DefaultMemMi:         defaultHarvesterMemMi,
			DefaultDiskGi:        defaultHarvesterDiskGi,
		},
		dynamicClient: dyn,
		logger:        slog.Default().With("backend", "harvester-vm-test"),
		keys:          make(map[string]ssh.Signer),
	}
	return h, dyn
}

func makeVMI(name, namespace, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"status": map[string]any{
				"phase": phase,
			},
		},
	}
	return obj
}

func makeVM(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
}

// ---------- Status ----------

func TestStatus_RunningVMI(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t, makeVMI("vm-run", "default", "Running"))
	st, err := h.Status(context.Background(), "vm-run")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Errorf("Running = false, want true")
	}
	if st.Status != "running" {
		t.Errorf("Status.Status = %q, want running", st.Status)
	}
}

func TestStatus_PendingVMI(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t, makeVMI("vm-pending", "default", "Pending"))
	st, err := h.Status(context.Background(), "vm-pending")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Running {
		t.Errorf("Running = true, want false")
	}
	if st.Status != "pending" {
		t.Errorf("Status.Status = %q, want pending", st.Status)
	}
}

func TestStatus_ScheduledVMI(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t, makeVMI("vm-sched", "default", "Scheduling"))
	st, err := h.Status(context.Background(), "vm-sched")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Running {
		t.Errorf("Running = true on Scheduling phase, want false")
	}
	if st.Status != "scheduling" {
		t.Errorf("Status.Status = %q, want scheduling", st.Status)
	}
}

func TestStatus_FailedVMI(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t, makeVMI("vm-failed", "default", "Failed"))
	st, err := h.Status(context.Background(), "vm-failed")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Running {
		t.Errorf("Running = true on Failed phase, want false")
	}
	if st.Status != "failed" {
		t.Errorf("Status.Status = %q, want failed", st.Status)
	}
}

func TestStatus_NotFound(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t) // no objects
	st, err := h.Status(context.Background(), "missing-vm")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Running || st.Status != "not_found" {
		t.Errorf("Status = %+v, want {Running:false Status:not_found}", st)
	}
}

func TestStatus_VMExistsButVMINotYet(t *testing.T) {
	// VM declared but VMI hasn't materialized yet — Start case during
	// cold boot. Status should report Running=false, Status="starting".
	h, _ := newFakeHarvesterVMBackend(t, makeVM("vm-starting", "default"))
	st, err := h.Status(context.Background(), "vm-starting")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Running {
		t.Errorf("Running = true, want false (VMI not yet materialized)")
	}
	if st.Status != "starting" {
		t.Errorf("Status.Status = %q, want starting", st.Status)
	}
}

// ---------- Stop ----------

func TestStop_DeletesVMAndClearsKey(t *testing.T) {
	h, dyn := newFakeHarvesterVMBackend(t, makeVM("vm-stop", "default"))

	// Stash a key so we can confirm Stop drops it.
	signer, _, err := generateSSHKey()
	if err != nil {
		t.Fatalf("generateSSHKey: %v", err)
	}
	h.putKey("vm-stop", signer)

	if err := h.Stop(context.Background(), "vm-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify VM was deleted.
	_, err = dyn.Resource(vmGVR).Namespace("default").
		Get(context.Background(), "vm-stop", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected VM gone after Stop, got err=%v", err)
	}

	if h.hasKey("vm-stop") {
		t.Errorf("key still present after Stop")
	}
}

func TestStop_NotFoundIsNotError(t *testing.T) {
	// Stop must be idempotent — calling on a VM that's already gone is
	// allowed because reconcilers retry Stop without checking first.
	h, _ := newFakeHarvesterVMBackend(t)
	if err := h.Stop(context.Background(), "absent-vm"); err != nil {
		t.Errorf("Stop on missing VM returned error: %v", err)
	}
}

// ---------- generateSSHKey ----------

func TestGenerateSSHKey_RoundTripPubKey(t *testing.T) {
	signer, pubStr, err := generateSSHKey()
	if err != nil {
		t.Fatalf("generateSSHKey: %v", err)
	}
	if signer == nil {
		t.Fatal("signer is nil")
	}
	if pubStr == "" {
		t.Fatal("pubStr is empty")
	}

	// authorized_keys parser round-trips the public key.
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubStr))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v (input=%q)", err, pubStr)
	}
	if parsed.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %q, want %q", parsed.Type(), ssh.KeyAlgoED25519)
	}

	// Signer's public key matches the marshalled one (modulo comment).
	if !strings.HasPrefix(pubStr, "ssh-ed25519 ") {
		t.Errorf("pubStr missing ssh-ed25519 prefix: %q", pubStr)
	}

	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("signer pubkey type = %q, want ed25519", signer.PublicKey().Type())
	}
}

func TestGenerateSSHKey_DistinctEachCall(t *testing.T) {
	// Sanity check: ed25519 keypairs must be random per invocation,
	// otherwise per-VM key registration collapses to a shared key.
	_, a, err := generateSSHKey()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, b, err := generateSSHKey()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a == b {
		t.Errorf("two generateSSHKey calls returned identical pubkey: %q", a)
	}
}

// ---------- key store ----------

func TestKeyStore_RoundTrip(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)

	if h.hasKey("vm-1") {
		t.Errorf("hasKey on empty store returned true")
	}

	signer, _, err := generateSSHKey()
	if err != nil {
		t.Fatalf("generateSSHKey: %v", err)
	}

	h.putKey("vm-1", signer)
	if !h.hasKey("vm-1") {
		t.Errorf("hasKey after put returned false")
	}

	got, ok := h.getKey("vm-1")
	if !ok {
		t.Errorf("getKey after put returned ok=false")
	}
	if got == nil {
		t.Errorf("getKey returned nil signer")
	}

	h.deleteKey("vm-1")
	if h.hasKey("vm-1") {
		t.Errorf("hasKey after delete returned true")
	}
	if _, ok := h.getKey("vm-1"); ok {
		t.Errorf("getKey after delete returned ok=true")
	}
}

func TestKeyStore_ConcurrentAccess(t *testing.T) {
	// putKey/getKey/hasKey/deleteKey must be safe under racing goroutines;
	// the production code uses sync.RWMutex. The race detector + this
	// fan-out is the regression net.
	h, _ := newFakeHarvesterVMBackend(t)

	signers := make([]ssh.Signer, 16)
	for i := range signers {
		s, _, err := generateSSHKey()
		if err != nil {
			t.Fatalf("generateSSHKey: %v", err)
		}
		signers[i] = s
	}

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(len(signers) * 3)
	for i, s := range signers {
		i, s := i, s
		name := vmName(i)

		// Writer.
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				h.putKey(name, s)
			}
		}()
		// Reader.
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = h.getKey(name)
				_ = h.hasKey(name)
			}
		}()
		// Deleter.
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				h.deleteKey(name)
			}
		}()
	}
	wg.Wait()
}

func vmName(i int) string {
	return "vm-conc-" + string(rune('a'+(i%26)))
}

// ---------- Build ----------

func TestBuild_ReturnsBaseImageNameWhenSet(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	h.cfg.BaseImageName = "mills-devbox-base-2026-05-25"

	res, err := h.Build(context.Background(), BuildOpts{Tag: "ignored"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageTag != "mills-devbox-base-2026-05-25" {
		t.Errorf("ImageTag = %q, want mills-devbox-base-2026-05-25", res.ImageTag)
	}
	if !res.Cached {
		t.Errorf("Cached = false, want true (Build is a no-op)")
	}
}

func TestBuild_FallsBackToOptsTag(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	h.cfg.BaseImageName = ""

	res, err := h.Build(context.Background(), BuildOpts{Tag: "explicit:tag"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageTag != "explicit:tag" {
		t.Errorf("ImageTag = %q, want explicit:tag", res.ImageTag)
	}
}

func TestBuild_DefaultTagWhenAllEmpty(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	h.cfg.BaseImageName = ""

	res, err := h.Build(context.Background(), BuildOpts{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageTag != "harvester-vm-base" {
		t.Errorf("ImageTag = %q, want harvester-vm-base", res.ImageTag)
	}
}

// ---------- CleanupBuilds ----------

func TestCleanupBuilds_DeletesOrphanPVCs(t *testing.T) {
	// Orphan PVC: owner is a VirtualMachine that no longer exists.
	// CleanupBuilds should delete it.
	orphanLabels := map[string]string{
		"app.kubernetes.io/managed-by": harvesterManagedByLabel,
	}
	orphan := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":              "orphan-pvc",
				"namespace":         "default",
				"labels":            toAnyMap(orphanLabels),
				"creationTimestamp": "2020-01-01T00:00:00Z",
				"ownerReferences": []any{
					map[string]any{
						"apiVersion": "kubevirt.io/v1",
						"kind":       "VirtualMachine",
						"name":       "ghost-vm",
						"uid":        "uid-ghost",
					},
				},
			},
		},
	}

	h, dyn := newFakeHarvesterVMBackend(t, orphan)

	// Use a tiny maxAge so the old creationTimestamp qualifies.
	cleaned, err := h.CleanupBuilds(context.Background(), 0)
	if err != nil {
		t.Fatalf("CleanupBuilds: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1", cleaned)
	}

	_, err = dyn.Resource(pvcGVR).Namespace("default").
		Get(context.Background(), "orphan-pvc", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected orphan PVC gone, got err=%v", err)
	}
}

func TestCleanupBuilds_KeepsPVCsWithLiveOwner(t *testing.T) {
	// PVC whose owner VM still exists: keep it.
	vm := makeVM("live-vm", "default")
	pvcLabels := map[string]string{
		"app.kubernetes.io/managed-by": harvesterManagedByLabel,
	}
	pvc := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]any{
				"name":              "live-vm-os",
				"namespace":         "default",
				"labels":            toAnyMap(pvcLabels),
				"creationTimestamp": "2020-01-01T00:00:00Z",
				"ownerReferences": []any{
					map[string]any{
						"apiVersion": "kubevirt.io/v1",
						"kind":       "VirtualMachine",
						"name":       "live-vm",
						"uid":        "uid-live",
					},
				},
			},
		},
	}

	h, dyn := newFakeHarvesterVMBackend(t, vm, pvc)

	cleaned, err := h.CleanupBuilds(context.Background(), 0)
	if err != nil {
		t.Fatalf("CleanupBuilds: %v", err)
	}
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 (owner alive)", cleaned)
	}

	_, err = dyn.Resource(pvcGVR).Namespace("default").
		Get(context.Background(), "live-vm-os", metav1.GetOptions{})
	if err != nil {
		t.Errorf("PVC unexpectedly deleted: %v", err)
	}
}

// ---------- vmAddress ----------

func TestVMAddress_ReturnsFirstIPAddress(t *testing.T) {
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-net",
				"namespace": "default",
			},
			"status": map[string]any{
				"interfaces": []any{
					map[string]any{
						"ipAddress": "192.168.1.42",
						"name":      "lan",
					},
				},
			},
		},
	}
	h, _ := newFakeHarvesterVMBackend(t, vmi)
	addr, err := h.vmAddress(context.Background(), "vm-net")
	if err != nil {
		t.Fatalf("vmAddress: %v", err)
	}
	if addr != "192.168.1.42" {
		t.Errorf("addr = %q, want 192.168.1.42", addr)
	}
}

func TestVMAddress_FallsBackToIPAddressesArray(t *testing.T) {
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-multinet",
				"namespace": "default",
			},
			"status": map[string]any{
				"interfaces": []any{
					map[string]any{
						"ipAddresses": []any{"10.0.0.5"},
						"name":        "lan",
					},
				},
			},
		},
	}
	h, _ := newFakeHarvesterVMBackend(t, vmi)
	addr, err := h.vmAddress(context.Background(), "vm-multinet")
	if err != nil {
		t.Fatalf("vmAddress: %v", err)
	}
	if addr != "10.0.0.5" {
		t.Errorf("addr = %q, want 10.0.0.5", addr)
	}
}

func TestVMAddress_NoInterfaceStatus(t *testing.T) {
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-noiface",
				"namespace": "default",
			},
			"status": map[string]any{},
		},
	}
	h, _ := newFakeHarvesterVMBackend(t, vmi)
	_, err := h.vmAddress(context.Background(), "vm-noiface")
	if err == nil {
		t.Errorf("expected error when interfaces missing, got nil")
	}
	if !strings.Contains(err.Error(), "guest-agent not connected") {
		t.Errorf("error = %q, want guest-agent hint", err.Error())
	}
}

// ---------- vmiAgentConnected (free function) ----------

func TestVMIAgentConnected_True(t *testing.T) {
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-c",
				"namespace": "default",
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "AgentConnected",
						"status": "True",
					},
				},
			},
		},
	}
	_, dyn := newFakeHarvesterVMBackend(t, vmi)
	connected, err := vmiAgentConnected(context.Background(), dyn, "default", "vm-c")
	if err != nil {
		t.Fatalf("vmiAgentConnected: %v", err)
	}
	if !connected {
		t.Errorf("connected = false, want true")
	}
}

func TestVMIAgentConnected_FalseWithoutCondition(t *testing.T) {
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-nc",
				"namespace": "default",
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "True"},
				},
			},
		},
	}
	_, dyn := newFakeHarvesterVMBackend(t, vmi)
	connected, err := vmiAgentConnected(context.Background(), dyn, "default", "vm-nc")
	if err != nil {
		t.Fatalf("vmiAgentConnected: %v", err)
	}
	if connected {
		t.Errorf("connected = true, want false (AgentConnected absent)")
	}
}

// ---------- vmiPhase (free function) ----------

func TestVMIPhase_ReturnsStatusPhase(t *testing.T) {
	_, dyn := newFakeHarvesterVMBackend(t, makeVMI("vm-p", "default", "Running"))
	phase, err := vmiPhase(context.Background(), dyn, "default", "vm-p")
	if err != nil {
		t.Fatalf("vmiPhase: %v", err)
	}
	if phase != "Running" {
		t.Errorf("phase = %q, want Running", phase)
	}
}

func TestVMIPhase_NotFound(t *testing.T) {
	_, dyn := newFakeHarvesterVMBackend(t)
	_, err := vmiPhase(context.Background(), dyn, "default", "absent")
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
}

// ---------- shellQuote ----------

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"hello", "hello"},
		{"a b", "'a b'"},
		{"foo$bar", "'foo$bar'"},
		{"with'apostrophe", `'with'\''apostrophe'`},
		{"path/with/spaces here", "'path/with/spaces here'"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- SetLogger ----------

func TestSetLogger_NilIsNoop(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	original := h.logger
	h.SetLogger(nil)
	if h.logger != original {
		t.Errorf("SetLogger(nil) replaced logger; want no-op")
	}
}

func TestSetLogger_NonNilReplaces(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	replacement := slog.Default()
	h.SetLogger(replacement)
	// Can't compare *slog.Logger by pointer reliably because With()
	// returns a new instance — verify it's at least non-nil and didn't
	// keep the test-default sentinel.
	if h.logger == nil {
		t.Errorf("logger is nil after SetLogger")
	}
}

// ---------- Start error path: missing key triggers explicit failure ----------

// TestStart_NameIsRequired makes sure the very first guard in Start fires
// before we touch the cluster — defensive cheap-failure.
func TestStart_NameIsRequired(t *testing.T) {
	h, _ := newFakeHarvesterVMBackend(t)
	_, err := h.Start(context.Background(), StartOpts{Name: ""})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "StartOpts.Name is required") {
		t.Errorf("err = %q, want StartOpts.Name is required", err.Error())
	}
}

// ---------- NewHarvesterVMBackend ----------

func TestNewHarvesterVMBackend_DefaultsAndOverrides(t *testing.T) {
	kubeconfig := writeTestKubeconfig(t)

	t.Run("defaults backfill", func(t *testing.T) {
		h, err := NewHarvesterVMBackend(HarvesterVMBackendConfig{
			KubeconfigPath:   kubeconfig,
			StorageClassName: "longhorn-image-default",
		})
		if err != nil {
			t.Fatalf("NewHarvesterVMBackend: %v", err)
		}
		if h.cfg.Namespace != defaultHarvesterNS {
			t.Errorf("namespace = %q, want %q", h.cfg.Namespace, defaultHarvesterNS)
		}
		if h.cfg.NetworkAttachmentDef != defaultHarvesterNAD {
			t.Errorf("NAD = %q, want %q", h.cfg.NetworkAttachmentDef, defaultHarvesterNAD)
		}
		if h.cfg.DefaultVCPUs != defaultHarvesterVCPUs {
			t.Errorf("vcpus = %d, want %d", h.cfg.DefaultVCPUs, defaultHarvesterVCPUs)
		}
		if h.cfg.DefaultMemMi != defaultHarvesterMemMi {
			t.Errorf("mem = %d, want %d", h.cfg.DefaultMemMi, defaultHarvesterMemMi)
		}
		if h.cfg.DefaultDiskGi != defaultHarvesterDiskGi {
			t.Errorf("disk = %d, want %d", h.cfg.DefaultDiskGi, defaultHarvesterDiskGi)
		}
		if h.cfg.SSHUser != defaultHarvesterSSHUser {
			t.Errorf("ssh user = %q, want %q", h.cfg.SSHUser, defaultHarvesterSSHUser)
		}
		if h.dynamicClient == nil || h.discoveryClient == nil || h.restConfig == nil {
			t.Errorf("clients/restConfig not wired: %+v", h)
		}
		if h.keys == nil {
			t.Errorf("keys map not initialized")
		}
	})

	t.Run("storage class required", func(t *testing.T) {
		_, err := NewHarvesterVMBackend(HarvesterVMBackendConfig{
			KubeconfigPath: kubeconfig,
		})
		if err == nil {
			t.Fatalf("expected error when StorageClassName is empty")
		}
		if !strings.Contains(err.Error(), "StorageClassName is required") {
			t.Errorf("err = %q, want StorageClassName is required", err.Error())
		}
	})
}

// ---------- waitForVMReady (poll loop) ----------

func TestWaitForVMReady_ReturnsImmediatelyWhenReady(t *testing.T) {
	// VMI is already Running, AgentConnected, and reports an IP — the
	// first poll iteration should succeed without sleeping a full tick.
	vmi := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]any{
				"name":      "vm-ready",
				"namespace": "default",
			},
			"status": map[string]any{
				"phase": "Running",
				"conditions": []any{
					map[string]any{"type": "AgentConnected", "status": "True"},
				},
				"interfaces": []any{
					map[string]any{"ipAddress": "10.0.0.7"},
				},
			},
		},
	}
	h, _ := newFakeHarvesterVMBackend(t, vmi)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.waitForVMReady(ctx, "vm-ready", 5*time.Second); err != nil {
		t.Errorf("waitForVMReady on ready VMI: %v", err)
	}
}

func TestWaitForVMReady_TimesOutOnUnreadyVMI(t *testing.T) {
	// VMI exists but stuck in Pending — wait should hit the timeout.
	vmi := makeVMI("vm-stuck", "default", "Pending")
	h, _ := newFakeHarvesterVMBackend(t, vmi)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := h.waitForVMReady(ctx, "vm-stuck", 500*time.Millisecond)
	if err == nil {
		t.Errorf("expected timeout error, got nil")
	}
}

func TestWaitForVMReady_NotFoundIsTransient(t *testing.T) {
	// VMI not yet materialized → wait should not surface the NotFound
	// as an error; it just keeps polling until ctx fires.
	h, _ := newFakeHarvesterVMBackend(t) // no objects
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := h.waitForVMReady(ctx, "absent", 500*time.Millisecond)
	// Expect a timeout-shaped error, not the underlying NotFound.
	if err == nil {
		t.Errorf("expected timeout error, got nil")
	}
	if apierrors.IsNotFound(err) {
		t.Errorf("err = NotFound, want timeout-shaped: %v", err)
	}
}

// ---------- helpers ----------

// toAnyMap converts map[string]string to map[string]any for unstructured
// labels (which are typed as map[string]any inside Object).
func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// errorsIs is a thin wrapper so the linter doesn't complain about
// importing "errors" only inside helpers. Kept for future Start error
// path checks.
var _ = errors.Is
