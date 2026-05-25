//go:build integration

package backend

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHarvesterVMBackend_Integration_StartExecStop exercises the full
// Start → wait-ready → Exec → Stop loop against a real Harvester cluster.
//
// Gated by build tag `integration` AND env var HARVESTER_KUBECONFIG so it
// stays out of the default `go test ./...` run. Required env vars:
//
//   - HARVESTER_KUBECONFIG: path to a Harvester admin kubeconfig
//   - HARVESTER_VM_STORAGECLASS: longhorn-image-<id> for the base image
//   - HARVESTER_VM_NAMESPACE: namespace for the test VM (default "default")
//   - HARVESTER_VM_BASEIMAGE: VirtualMachineImage name (optional, doc only)
//   - HARVESTER_VM_NAD: multus net-attach-def ref (default "default/lan10g")
//
// The test creates one VM with a randomized name, runs `echo hello`, and
// tears everything down. Cleanup uses t.Cleanup so a panic still attempts
// the Stop path.
func TestHarvesterVMBackend_Integration_StartExecStop(t *testing.T) {
	kubeconfig := os.Getenv("HARVESTER_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("HARVESTER_KUBECONFIG not set; skipping integration test")
	}
	storageClass := os.Getenv("HARVESTER_VM_STORAGECLASS")
	if storageClass == "" {
		t.Skip("HARVESTER_VM_STORAGECLASS not set; skipping integration test")
	}

	namespace := os.Getenv("HARVESTER_VM_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	nad := os.Getenv("HARVESTER_VM_NAD")
	if nad == "" {
		nad = "default/lan10g"
	}

	cfg := HarvesterVMBackendConfig{
		KubeconfigPath:       kubeconfig,
		BaseImageName:        os.Getenv("HARVESTER_VM_BASEIMAGE"),
		Namespace:            namespace,
		StorageClassName:     storageClass,
		NetworkAttachmentDef: nad,
	}

	backend, err := NewHarvesterVMBackend(cfg)
	if err != nil {
		t.Fatalf("NewHarvesterVMBackend: %v", err)
	}

	// Health check up front — if KubeVirt isn't reachable, bail with a
	// clear message instead of failing 5min later in waitForVMReady.
	if err := backend.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	vmName := fmt.Sprintf("mills-it-%d", time.Now().Unix())
	startCtx, startCancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer startCancel()

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.Stop(stopCtx, vmName); err != nil {
			t.Logf("cleanup Stop(%s): %v", vmName, err)
		}
	})

	if _, err := backend.Start(startCtx, StartOpts{
		Name:    vmName,
		AgentID: "harvester-vm-integration-test",
	}); err != nil {
		t.Fatalf("Start(%s): %v", vmName, err)
	}

	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	res, err := backend.Exec(execCtx, ExecOpts{
		ContainerID: vmName,
		Command:     "echo hello",
		TimeoutSec:  20,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; stderr=%q", res.ExitCode, res.StderrTail)
	}
	if !strings.Contains(res.StdoutTail, "hello") {
		t.Errorf("StdoutTail = %q, want substring 'hello'", res.StdoutTail)
	}

	// Verify Stop succeeds and Status reports gone.
	if err := backend.Stop(context.Background(), vmName); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Give the API server a moment to converge before asking Status.
	time.Sleep(2 * time.Second)
	st, err := backend.Status(context.Background(), vmName)
	if err != nil {
		t.Fatalf("Status after Stop: %v", err)
	}
	if st.Status != "not_found" && st.Status != "stopping" {
		t.Errorf("Status after Stop = %q, want not_found or stopping", st.Status)
	}
}
