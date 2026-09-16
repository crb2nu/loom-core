package backend

import (
	"strings"
	"testing"
)

func TestWorkspaceRelPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/workspace", ""},
		{"/workspace/", ""},
		{"/workspace/loom-core", "loom-core"},
		{"/workspace/services/loom-core", "services/loom-core"},
		{"/workspace/libs/mcp-go", "libs/mcp-go"},
		{"/workspace/libs/mcp-go/", "libs/mcp-go"},
		{"/elsewhere/services/foo", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := workspaceRelPath(tc.in); got != tc.want {
			t.Errorf("workspaceRelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJoinRepoURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// Group-neutral base + qualified paths (target state).
		{"http://192.168.50.218", "services/loom-core", "http://192.168.50.218/services/loom-core.git"},
		{"http://192.168.50.218", "libs/mcp-go", "http://192.168.50.218/libs/mcp-go.git"},
		// Legacy group-suffixed base + qualified path: boundary segment dedups
		// so config/image rollout skew never produces /services/services/.
		{"http://192.168.50.218/services", "services/loom-core", "http://192.168.50.218/services/loom-core.git"},
		// Legacy base + bare name keeps the legacy composition.
		{"http://192.168.50.218/services", "loom-core", "http://192.168.50.218/services/loom-core.git"},
		// Trailing slash on the base is tolerated.
		{"http://192.168.50.218/", "libs/mcp-go", "http://192.168.50.218/libs/mcp-go.git"},
	}
	for _, tc := range cases {
		if got := joinRepoURL(tc.base, tc.path); got != tc.want {
			t.Errorf("joinRepoURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// The buildah build pod passes no explicit projectPath; workspacePlan must
// derive it from the /workspace-relative build context so group-qualified
// repos (libs/…) clone correctly. Regression: the MCP-campaign smoke's tests
// stage died with ".../services/mcp-go.git not found" because the build pod
// fell back to the WorkDir basename against a group-suffixed base URL.
func TestBuildahPodSpecDerivesGroupQualifiedClonePath(t *testing.T) {
	k := testK8sBackendGitClone()
	k.gitBaseURL = "http://192.168.50.218"

	pod := k.buildBuildahPodSpec("build-pod", "registry.harbor.lan/devbox:tag", "dockerfile-cm", "/workspace/libs/mcp-go", false, false)
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected git-clone init container on the build pod")
	}
	script := pod.Spec.InitContainers[0].Command[2]
	if !strings.Contains(script, "192.168.50.218/libs/mcp-go.git") {
		t.Fatalf("expected libs clone target in build pod, got: %s", script)
	}

	pod = k.buildBuildahPodSpec("build-pod", "registry.harbor.lan/devbox:tag", "dockerfile-cm", "/workspace/services/loom-core", false, false)
	if script := pod.Spec.InitContainers[0].Command[2]; !strings.Contains(script, "192.168.50.218/services/loom-core.git") {
		t.Fatalf("expected services clone target in build pod, got: %s", script)
	}
}

// The devbox manager's sandbox start path also passes no GitProjectPath;
// buildPodSpec must derive it from the WorkDir the same way.
func TestStartPodDerivesProjectPathWhenUnset(t *testing.T) {
	k := testK8sBackendGitClone()
	k.gitBaseURL = "http://192.168.50.218"

	pod := k.buildPodSpec(StartOpts{
		Name:    "devbox-mcp-go",
		WorkDir: "/workspace/libs/mcp-go",
	}, "registry.harbor.lan/devbox:tag")
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected git-clone init container on the start pod")
	}
	if script := pod.Spec.InitContainers[0].Command[2]; !strings.Contains(script, "192.168.50.218/libs/mcp-go.git") {
		t.Fatalf("expected derived libs clone target, got: %s", script)
	}
}
