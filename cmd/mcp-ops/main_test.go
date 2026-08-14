package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// Mock helper
func fakeExecCommand(ctx context.Context, command string, args ...string) *exec.Cmd {
	cs := []string{"-test.run=TestHelperProcess", "--", command}
	cs = append(cs, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], cs...)
	cmd.Env = []string{"GO_WANT_HELPER_PROCESS=1"}
	return cmd
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Parse the command line to figure out what to return
	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}

	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command\n")
		os.Exit(2)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "kubectl":
		handleKubectl(cmdArgs)
	case "ssh":
		handleSSH(cmdArgs)
	case "bash":
		handleBash(cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		os.Exit(1)
	}
	os.Exit(0)
}

func handleKubectl(args []string) {
	// Simple argument parsing for mock responses
	cmdStr := strings.Join(args, " ")

	if strings.Contains(cmdStr, "get nodes -o wide") {
		fmt.Println("NAME     STATUS   ROLES                  AGE   VERSION   INTERNAL-IP   EXTERNAL-IP   OS-IMAGE             KERNEL-VERSION      CONTAINER-RUNTIME")
		fmt.Println("node1    Ready    control-plane,master   10d   v1.26.0   192.168.1.10  <none>        Ubuntu 22.04.1 LTS   5.15.0-58-generic   containerd://1.6.15")
		return
	}

	if strings.Contains(cmdStr, "scale deploy/test-deploy --replicas=3") {
		fmt.Println("deployment.apps/test-deploy scaled")
		return
	}

	if strings.Contains(cmdStr, "get pods -o json") {
		// Return sample pods
		pods := struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			} `json:"items"`
		}{
			Items: []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			}{
				{
					Metadata: struct {
						Name string `json:"name"`
					}{Name: "pod-running"},
					Status: struct {
						Phase string `json:"phase"`
					}{Phase: "Running"},
				},
				{
					Metadata: struct {
						Name string `json:"name"`
					}{Name: "pod-failed"},
					Status: struct {
						Phase string `json:"phase"`
					}{Phase: "Failed"},
				},
				{
					Metadata: struct {
						Name string `json:"name"`
					}{Name: "pod-evicted"},
					Status: struct {
						Phase string `json:"phase"`
					}{Phase: "Evicted"},
				},
			},
		}
		json.NewEncoder(os.Stdout).Encode(pods)
		return
	}

	if strings.Contains(cmdStr, "delete pod --wait=false") {
		// Check which pods are being deleted
		if strings.Contains(cmdStr, "pod-failed") || strings.Contains(cmdStr, "pod-evicted") {
			fmt.Println("pod \"pod-failed\" deleted")
			fmt.Println("pod \"pod-evicted\" deleted")
		} else {
			fmt.Println("No pods deleted")
		}
		return
	}

	if strings.Contains(cmdStr, "label node node1 kube-vip.io/eligible=true") {
		fmt.Println("node/node1 labeled")
		return
	}

	if strings.Contains(cmdStr, "rollout restart deployment/test-deploy") {
		fmt.Println("deployment.apps/test-deploy restarted")
		return
	}

	if strings.Contains(cmdStr, "rollout status deployment/test-deploy") {
		fmt.Println("deployment \"test-deploy\" successfully rolled out")
		return
	}

	if strings.Contains(cmdStr, "delete pod my-pod --wait=false") {
		fmt.Println("pod \"my-pod\" deleted")
		return
	}

	// Default fallback
	fmt.Printf("Mock kubectl executed: %s\n", cmdStr)
}

func handleSSH(args []string) {
	fmt.Printf("Mock ssh executed: %s\n", strings.Join(args, " "))
}

func handleBash(args []string) {
	fmt.Printf("Mock bash executed: %s\n", strings.Join(args, " "))
}

func TestHandleGetNodes(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	ctx := context.Background()
	args := map[string]any{
		"kubeconfig": "/tmp/kubeconfig",
	}

	result, err := handleGetNodes(ctx, args)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if result.IsError {
		t.Fatalf("Expected success result, got error")
	}

	content := result.Content[0].Text
	if !strings.Contains(content, "node1") {
		t.Errorf("Expected output to contain 'node1', got %s", content)
	}
}

func TestHandleScaleDeploy(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	ctx := context.Background()
	args := map[string]any{
		"namespace":  "default",
		"name":       "test-deploy",
		"replicas":   3.0,
		"kubeconfig": "/tmp/kubeconfig",
	}

	result, err := handleScaleDeploy(ctx, args)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	content := result.Content[0].Text
	if !strings.Contains(content, "scaled") {
		t.Errorf("Expected output to contain 'scaled', got %s", content)
	}
}

func TestHandleDeletePodsByPhase(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	ctx := context.Background()
	args := map[string]any{
		"namespace":  "default",
		"phases":     []any{"Failed", "Evicted"},
		"kubeconfig": "/tmp/kubeconfig",
	}

	result, err := handleDeletePodsByPhase(ctx, args)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	content := result.Content[0].Text
	if !strings.Contains(content, "deleted") {
		t.Errorf("Expected output to contain 'deleted', got %s", content)
	}
}

func TestHandleVipLabelNode(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	ctx := context.Background()
	args := map[string]any{
		"node":       "node1",
		"eligible":   true,
		"kubeconfig": "/tmp/kubeconfig",
	}

	result, err := handleVipLabelNode(ctx, args)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	content := result.Content[0].Text
	if !strings.Contains(content, "labeled") {
		t.Errorf("Expected output to contain 'labeled', got %s", content)
	}
}

func TestHandleRolloutRestart(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	ctx := context.Background()
	result, err := handleRolloutRestart(ctx, map[string]any{
		"namespace":  "default",
		"name":       "test-deploy",
		"kubeconfig": "/tmp/kubeconfig",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "restarted") {
		t.Errorf("Expected output to contain 'restarted', got %s", result.Content[0].Text)
	}
}

func TestHandleRolloutRestart_RejectsBadKind(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	result, err := handleRolloutRestart(context.Background(), map[string]any{
		"namespace": "default",
		"name":      "test-deploy",
		"kind":      "configmap",
	})
	if err != nil {
		t.Fatalf("Expected no transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("Expected error result for unsupported kind, got success")
	}
}

func TestHandleRolloutStatus(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	result, err := handleRolloutStatus(context.Background(), map[string]any{
		"namespace": "default",
		"name":      "test-deploy",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "successfully rolled out") {
		t.Errorf("Expected rollout status output, got %s", result.Content[0].Text)
	}
}

func TestHandleDeletePod(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	result, err := handleDeletePod(context.Background(), map[string]any{
		"namespace": "default",
		"name":      "my-pod",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "deleted") {
		t.Errorf("Expected output to contain 'deleted', got %s", result.Content[0].Text)
	}
}

func TestResolveKubeconfig(t *testing.T) {
	dir := t.TempDir()
	existing := dir + "/exists.yaml"
	if err := os.WriteFile(existing, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("KUBECONFIG", "")

	// Explicit path that exists is used as-is.
	if got := resolveKubeconfig(clusterK3s, existing); got != existing {
		t.Errorf("explicit existing: got %q want %q", got, existing)
	}

	// Explicit path that does NOT exist falls through; with KUBECONFIG empty and
	// (typically) no ~/.kube files in CI, it returns the explicit value so
	// kubectl emits a clear error rather than silently using a wrong config.
	missing := dir + "/missing.yaml"
	if got := resolveKubeconfig(clusterK3s, missing); got != missing && got == "" {
		t.Errorf("explicit missing: expected a non-empty fallback, got %q", got)
	}

	// KUBECONFIG is honored when the explicit path is empty and it exists.
	t.Setenv("KUBECONFIG", existing)
	if got := resolveKubeconfig(clusterK3s, ""); got != existing {
		t.Errorf("from KUBECONFIG: got %q want %q", got, existing)
	}
}

// setKubeconfigVars points the package-level cluster kubeconfigs at test values
// and restores them afterwards. It also relocates $HOME so the ~/.kube/*
// candidates cannot pick up the developer's real files.
func setKubeconfigVars(t *testing.T, k3s, rke2 string) {
	t.Helper()
	origK3s, origRKE2 := k3sKubeconfig, rke2Kubeconfig
	t.Cleanup(func() { k3sKubeconfig, rke2Kubeconfig = origK3s, origRKE2 })
	k3sKubeconfig, rke2Kubeconfig = k3s, rke2
	t.Setenv("HOME", t.TempDir())
}

// TestResolveKubeconfigRKE2NeverFallsBackToK3s is the regression guard for the
// cross-cluster bug: when RKE2_KUBECONFIG points at a path that does not exist,
// resolution must NOT walk into the k3s candidates. Doing so pointed the
// mutating harvester_vm_restart at the k3s cluster.
func TestResolveKubeconfigRKE2NeverFallsBackToK3s(t *testing.T) {
	dir := t.TempDir()
	k3sPath := dir + "/k3s.yaml"
	if err := os.WriteFile(k3sPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	missingRKE2 := dir + "/harvester-missing.yaml"

	setKubeconfigVars(t, k3sPath, missingRKE2)
	// Every k3s-flavored source is live and points at an existing file: the
	// ambient $KUBECONFIG, K3S_KUBECONFIG, and (via $HOME) ~/.kube/config.
	t.Setenv("KUBECONFIG", k3sPath)
	kubeDir := os.ExpandEnv("$HOME/.kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"k3s.yaml", "config"} {
		if err := os.WriteFile(kubeDir+"/"+name, []byte("apiVersion: v1\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got := resolveKubeconfig(clusterRKE2, rke2Kubeconfig)
	if got != missingRKE2 {
		t.Errorf("RKE2 with a missing path: got %q, want the configured %q so kubectl fails with a clear error", got, missingRKE2)
	}
	for _, forbidden := range []string{k3sPath, kubeDir + "/k3s.yaml", kubeDir + "/config"} {
		if got == forbidden {
			t.Errorf("RKE2 resolution fell back to the k3s kubeconfig %q — that targets the WRONG CLUSTER", forbidden)
		}
	}

	// Sanity check on the same fixture: k3s resolution is unchanged and still
	// finds its own kubeconfig.
	if k3sGot := resolveKubeconfig(clusterK3s, ""); k3sGot != k3sPath {
		t.Errorf("k3s resolution: got %q want %q", k3sGot, k3sPath)
	}
}

// TestResolveKubeconfigK3sUnchanged pins the k3s candidate chain (explicit,
// $KUBECONFIG, K3S_KUBECONFIG, ~/.kube/k3s.yaml, ~/.kube/config) so the
// cross-cluster scoping does not narrow the behavior the k3s tools rely on.
func TestResolveKubeconfigK3sUnchanged(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte("apiVersion: v1\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	explicit := write("explicit.yaml")
	fromEnv := write("from-env.yaml")
	configured := write("configured.yaml")
	missing := dir + "/missing.yaml"

	setKubeconfigVars(t, configured, dir+"/harvester-missing.yaml")
	kubeDir := os.ExpandEnv("$HOME/.kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	homeK3s := kubeDir + "/k3s.yaml"
	if err := os.WriteFile(homeK3s, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name       string
		explicit   string
		kubeconfig string // $KUBECONFIG
		want       string
	}{
		{"explicit wins", explicit, fromEnv, explicit},
		{"KUBECONFIG when explicit missing", missing, fromEnv, fromEnv},
		{"K3S_KUBECONFIG when explicit and KUBECONFIG empty/missing", "", "", configured},
		{"home k3s.yaml when configured path missing", "", "", homeK3s},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tc.kubeconfig)
			if tc.want == homeK3s {
				k3sKubeconfig = missing
				defer func() { k3sKubeconfig = configured }()
			}
			if got := resolveKubeconfig(clusterK3s, tc.explicit); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestHarvesterHandlersUseRKE2Kubeconfig asserts end-to-end (through the kubectl
// mock) that the Harvester tools hand kubectl the RKE2 path even when it is
// absent and a perfectly good k3s kubeconfig is available — including the
// mutating restart, which patches spec.running false then true.
func TestHarvesterHandlersUseRKE2Kubeconfig(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.CommandContext }()

	dir := t.TempDir()
	k3sPath := dir + "/k3s.yaml"
	if err := os.WriteFile(k3sPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	missingRKE2 := "/etc/harvester/kubeconfig-does-not-exist"

	setKubeconfigVars(t, k3sPath, missingRKE2)
	t.Setenv("KUBECONFIG", k3sPath)

	ctx := context.Background()

	listResult, err := handleHarvesterVMsList(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("harvester_vms_list: %v", err)
	}
	restartResult, err := handleHarvesterVMRestart(ctx, map[string]any{
		"namespace": "default",
		"name":      "k3s-cp-1",
	})
	if err != nil {
		t.Fatalf("harvester_vm_restart: %v", err)
	}

	for name, result := range map[string]*mcp.CallToolResult{
		"harvester_vms_list":   listResult,
		"harvester_vm_restart": restartResult,
	} {
		out := result.Content[0].Text
		if !strings.Contains(out, "--kubeconfig "+missingRKE2) {
			t.Errorf("%s: kubectl was not given the RKE2 kubeconfig %q; got %s", name, missingRKE2, out)
		}
		if strings.Contains(out, k3sPath) {
			t.Errorf("%s: kubectl was given the k3s kubeconfig %q — WRONG CLUSTER; got %s", name, k3sPath, out)
		}
	}
}
