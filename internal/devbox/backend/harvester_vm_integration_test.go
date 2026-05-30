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

// TestHarvesterVMBackend_Integration_CodexHomeParity is the Slice 2d.5c
// kill-test: it proves a live harvester VM boots with the agent-user home-dir
// parity AND a codex SecretMount delivered to /home/agent/.codex.auth/auth.json
// owned by agent:agent. These are the load-bearing facts the 2d.5c change
// introduced — unit tests pin the manifest shape, but only a live boot proves
// cloud-init actually creates the agent user, SSH-as-agent works, and the
// root-written secret file is chowned to the agent after the user exists.
//
// Spec: .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md
// (Slice 2d.5c, "Kill-test still owed").
// Handoff/re-run procedure:
// .loom/local/handoffs/mills-harvester-home-parity-killtest-2026-05-30.md
//
// Gated by build tag `integration` AND env vars so it stays out of the
// default `go test ./...` run. Required env vars:
//
//   - HARVESTER_KUBECONFIG: harvester admin kubeconfig (drives VM boot)
//   - HARVESTER_VM_STORAGECLASS: longhorn-image-<id> for the base image
//   - HARVESTER_K3S_KUBECONFIG: k3s kubeconfig — the SecretResolver source
//     for the codex auth secret
//
// Optional env vars (defaults match the prod canary wiring):
//
//   - HARVESTER_VM_NAMESPACE: VM namespace (default "default")
//   - HARVESTER_VM_NAD: multus net-attach-def ref (default "default/lan10g")
//   - HARVESTER_CODEX_SECRET: k3s secret name (default "cluster-agent-auth")
//   - HARVESTER_CODEX_SECRET_KEY: key within the secret (default "codex-auth-json")
//   - HARVESTER_CODEX_SECRET_NS: namespace of the secret on k3s (default "default")
//
// The test resolves the secret itself (via the same K8sBackend resolver the
// backend uses) so it can assert the on-VM file content byte-for-byte without
// hardcoding any credential. Cleanup uses t.Cleanup so a panic still attempts
// the Stop path.
func TestHarvesterVMBackend_Integration_CodexHomeParity(t *testing.T) {
	kubeconfig := os.Getenv("HARVESTER_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("HARVESTER_KUBECONFIG not set; skipping integration test")
	}
	storageClass := os.Getenv("HARVESTER_VM_STORAGECLASS")
	if storageClass == "" {
		t.Skip("HARVESTER_VM_STORAGECLASS not set; skipping integration test")
	}
	k3sKubeconfig := os.Getenv("HARVESTER_K3S_KUBECONFIG")
	if k3sKubeconfig == "" {
		t.Skip("HARVESTER_K3S_KUBECONFIG not set; skipping integration test")
	}

	namespace := os.Getenv("HARVESTER_VM_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	nad := os.Getenv("HARVESTER_VM_NAD")
	if nad == "" {
		nad = "default/lan10g"
	}
	secretName := os.Getenv("HARVESTER_CODEX_SECRET")
	if secretName == "" {
		secretName = "cluster-agent-auth"
	}
	secretKey := os.Getenv("HARVESTER_CODEX_SECRET_KEY")
	if secretKey == "" {
		secretKey = "codex-auth-json"
	}
	secretNS := os.Getenv("HARVESTER_CODEX_SECRET_NS")
	if secretNS == "" {
		secretNS = "default"
	}

	// The codex SecretMount mirrors the orchestrator's injectAgentSecrets
	// wiring: cluster-agent-auth/codex-auth-json lands at
	// /home/agent/.codex.auth/auth.json (injectAgentConfig later symlinks
	// ~/.codex/auth.json to it).
	const codexAuthPath = vmAgentHome + "/.codex.auth/auth.json"
	codexMount := SecretMount{
		SecretName: secretName,
		MountPath:  vmAgentHome + "/.codex.auth",
		Items:      []SecretMountItem{{Key: secretKey, Path: "auth.json"}},
	}

	// K8sBackend on k3s acts as the SecretResolver. Namespace must match
	// where the codex secret lives, since ResolveSecretMounts reads from the
	// backend's configured namespace.
	resolver, err := NewK8sBackend(K8sBackendConfig{
		Kubeconfig: k3sKubeconfig,
		Namespace:  secretNS,
	})
	if err != nil {
		t.Fatalf("NewK8sBackend (resolver): %v", err)
	}

	// Resolve the expected on-VM content up front. This also fails fast with
	// a clear message if the secret/key is missing on k3s.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer resolveCancel()
	resolved, err := resolver.ResolveSecretMounts(resolveCtx, []SecretMount{codexMount})
	if err != nil {
		t.Fatalf("ResolveSecretMounts: %v", err)
	}
	var wantContent []byte
	for _, f := range resolved {
		if f.Path == codexAuthPath {
			wantContent = f.Content
			break
		}
	}
	if len(wantContent) == 0 {
		t.Fatalf("secret %s/%s key %q resolved empty or absent; cannot assert on-VM content", secretNS, secretName, secretKey)
	}

	backend, err := NewHarvesterVMBackend(HarvesterVMBackendConfig{
		KubeconfigPath:       kubeconfig,
		BaseImageName:        os.Getenv("HARVESTER_VM_BASEIMAGE"),
		Namespace:            namespace,
		StorageClassName:     storageClass,
		NetworkAttachmentDef: nad,
		SecretResolver:       resolver,
	})
	if err != nil {
		t.Fatalf("NewHarvesterVMBackend: %v", err)
	}

	if err := backend.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	vmName := fmt.Sprintf("mills-codex-it-%d", time.Now().Unix())
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.Stop(stopCtx, vmName); err != nil {
			t.Logf("cleanup Stop(%s): %v", vmName, err)
		}
	})

	startCtx, startCancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer startCancel()
	if _, err := backend.Start(startCtx, StartOpts{
		Name:         vmName,
		AgentID:      "harvester-vm-codex-parity-test",
		SecretMounts: []SecretMount{codexMount},
	}); err != nil {
		t.Fatalf("Start(%s): %v", vmName, err)
	}

	exec := func(cmd string) *ExecResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := backend.Exec(ctx, ExecOpts{
			ContainerID: vmName,
			Command:     cmd,
			TimeoutSec:  20,
		})
		if err != nil {
			t.Fatalf("Exec(%q): %v", cmd, err)
		}
		return res
	}

	// 1. SSH session runs as the agent user — proves the cloud-init `users:`
	//    stanza created `agent` and SSHUser defaulted to it (home parity).
	if got := strings.TrimSpace(exec("whoami").StdoutTail); got != vmAgentUser {
		t.Errorf("whoami = %q, want %q (agent-user home parity)", got, vmAgentUser)
	}
	if got := strings.TrimSpace(exec("echo $HOME").StdoutTail); got != vmAgentHome {
		t.Errorf("$HOME = %q, want %q", got, vmAgentHome)
	}

	// 2. The codex SecretMount file landed at the agent-home path, owned by
	//    agent:agent — proves the root-write + runcmd chown handoff worked.
	if got := strings.TrimSpace(exec("stat -c '%U:%G' " + codexAuthPath).StdoutTail); got != vmAgentUser+":"+vmAgentUser {
		t.Errorf("owner of %s = %q, want %q (runcmd chown)", codexAuthPath, got, vmAgentUser+":"+vmAgentUser)
	}

	// 3. File content matches the k3s secret byte-for-byte.
	if got := exec("cat " + codexAuthPath).StdoutTail; strings.TrimRight(got, "\n") != strings.TrimRight(string(wantContent), "\n") {
		t.Errorf("content of %s does not match secret %s/%s key %q", codexAuthPath, secretNS, secretName, secretKey)
	}

	// 4. Mirror injectAgentConfig's symlink and confirm ~/.codex/auth.json
	//    resolves to the mounted file — the end the agent CLIs actually read.
	linkCmd := "mkdir -p " + vmAgentHome + "/.codex && ln -sf " + codexAuthPath + " " + vmAgentHome + "/.codex/auth.json && readlink -f " + vmAgentHome + "/.codex/auth.json"
	if got := strings.TrimSpace(exec(linkCmd).StdoutTail); got != codexAuthPath {
		t.Errorf("readlink -f ~/.codex/auth.json = %q, want %q", got, codexAuthPath)
	}
}
