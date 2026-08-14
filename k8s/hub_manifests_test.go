// Package k8s holds the loom-hub Kustomize base. It contains no production Go
// code -- only guards over the hand-maintained manifests under base/servers.
//
// These tests exist because of a class of bug that is invisible in review: the
// local (stdio) targets in mcp/context/registry.yaml point *_KUBECONFIG at the
// operator's macOS kubeconfig, and when a server was promoted to a hub
// Deployment that macOS path was copied verbatim into the container env. The
// path does not exist in the Linux container, so every tool on that server
// failed -- mcp-flux with a hard `load kubeconfig: stat ...: no such file or
// directory`, and the kubectl-backed mcp-k8s-ops servers more quietly, by
// falling back to the pod ServiceAccount and returning `Forbidden`.
package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const serversDir = "base/servers"

// knownBroken lists Deployments that still carry an unusable kubeconfig path
// and why they cannot be fixed by wiring alone. Keep this empty if you can.
//
// ops-mcp used to be listed here: cmd/mcp-ops/main.go resolveKubeconfig() fell
// back to K3S_KUBECONFIG whenever the requested kubeconfig was absent, and that
// fallback applied to *every* kubectl call -- including the Harvester ones
// (harvester_vms_list and the mutating harvester_vm_restart). Mounting a working
// in-cluster kubeconfig at K3S_KUBECONFIG without first making the Harvester
// path fail closed would have silently retargeted a VM power-cycle at k3s.
// resolveKubeconfig now takes a targetCluster and scopes its candidates to that
// cluster, so ops-mcp is wired like flux/helm/longhorn-k3s.
var knownBroken = map[string]string{}

// reservedMount identifies one env on one Deployment. Exemptions are keyed this
// narrowly on purpose: /etc/harvester/kubeconfig is also mobile-hud's
// SPAWN_HARVESTER_KUBECONFIG, and mobile-hud really does mount a Secret there --
// a path-only exemption would stop checking that.
type reservedMount struct {
	manifest string
	envValue string
}

// reservedMountPoints are kubeconfig paths a Deployment declares on purpose
// without mounting anything there, so the mount point is documented and stable
// for the day the credential exists. This is only safe when the code behind the
// env fails closed on a missing file -- an empty exemption is better than a
// populated one, so justify every entry.
//
// ops-mcp points RKE2_KUBECONFIG at the path mobile-hud and the parked
// k8s-harvester-infra server use, while running its k3s tools against a real
// in-cluster kubeconfig. Its harvester_* tools return a plain kubectl "no such
// file" error until a read-scoped Harvester Secret is provisioned, because
// cmd/mcp-ops/main.go resolveKubeconfig(clusterRKE2, ...) refuses to fall back
// into the k3s candidates (TestResolveKubeconfigRKE2NeverFallsBackToK3s).
var reservedMountPoints = map[reservedMount]string{
	{manifest: "base/servers/ops-mcp/deployment.yaml", envValue: "/etc/harvester/kubeconfig"}: "Harvester credential not provisioned yet; mcp-ops RKE2 resolution fails closed",
}

// kubeconfigEnvSuffix matches the env var names the MCP servers read a
// kubeconfig path from: KUBECONFIG, MCP_K8S_KUBECONFIG, FLUX_KUBECONFIG,
// HELM_KUBECONFIG, SPAWN_HARVESTER_KUBECONFIG, ...
const kubeconfigEnvSuffix = "KUBECONFIG"

type deployment struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type container struct {
	Name string `yaml:"name"`
	Env  []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

// loadDeployments returns every Deployment manifest under base/servers keyed by
// its repo-relative path.
func loadDeployments(t *testing.T) map[string]deployment {
	t.Helper()

	out := map[string]deployment{}
	err := filepath.WalkDir(serversDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "deployment.yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var dep deployment
		if unmarshalErr := yaml.Unmarshal(raw, &dep); unmarshalErr != nil {
			return unmarshalErr
		}
		if dep.Kind != "Deployment" {
			return nil
		}
		out[path] = dep
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", serversDir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no Deployment manifests found under %s", serversDir)
	}
	return out
}

// pathIsMounted reports whether a volumeMount provides file. A mount satisfies
// the path either by mounting the file itself (subPath style, as the
// flux/helm/longhorn-k3s servers do) or by mounting an ancestor directory that
// contains it (as mobile-hud does with /etc/harvester + key `kubeconfig`).
func pathIsMounted(file string, c container) bool {
	for _, m := range c.VolumeMounts {
		if m.MountPath == file {
			return true
		}
		if strings.HasPrefix(file, strings.TrimSuffix(m.MountPath, "/")+"/") {
			return true
		}
	}
	return false
}

// TestHubDeploymentsHaveNoOperatorHostPaths is the narrow guard: a macOS
// /Users/... path can never resolve inside a hub container, so it is always a
// leak from a local stdio target regardless of which env var carries it.
func TestHubDeploymentsHaveNoOperatorHostPaths(t *testing.T) {
	for path, dep := range loadDeployments(t) {
		if reason, ok := knownBroken[path]; ok {
			t.Logf("skipping known-broken %s: %s", path, reason)
			continue
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			for _, env := range c.Env {
				if strings.HasPrefix(env.Value, "/Users/") {
					t.Errorf("%s: container %q env %s=%q is an operator host path; "+
						"hub containers cannot read it -- mount a Secret/ConfigMap instead",
						path, c.Name, env.Name, env.Value)
				}
			}
		}
	}
}

// TestHubDeploymentKubeconfigPathsAreMounted is the guard that actually
// reproduces the reported bug: a *_KUBECONFIG env pointing at a path that no
// volumeMount provides. Without a mount the file simply is not there, which is
// exactly how the flux/helm/longhorn-k3s servers failed.
//
// Deployments scaled to 0 are skipped: they are parked precisely because the
// credential they need does not exist yet, and their env documents the mount
// point a future Secret will occupy. reservedMountPoints does the same for a
// single env on an otherwise-running Deployment.
func TestHubDeploymentKubeconfigPathsAreMounted(t *testing.T) {
	for path, dep := range loadDeployments(t) {
		if reason, ok := knownBroken[path]; ok {
			t.Logf("skipping known-broken %s: %s", path, reason)
			continue
		}
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			continue
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			for _, env := range c.Env {
				if !strings.HasSuffix(env.Name, kubeconfigEnvSuffix) || env.Value == "" {
					continue
				}
				if pathIsMounted(env.Value, c) {
					continue
				}
				if reason, ok := reservedMountPoints[reservedMount{manifest: path, envValue: env.Value}]; ok {
					t.Logf("%s: container %q env %s=%q is a reserved mount point: %s",
						path, c.Name, env.Name, env.Value, reason)
					continue
				}
				t.Errorf("%s: container %q env %s=%q has no volumeMount covering that path; "+
					"the kubeconfig will be missing at runtime",
					path, c.Name, env.Name, env.Value)
			}
		}
	}
}
