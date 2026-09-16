package backend

import (
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type workspacePodPlan struct {
	volume         corev1.Volume
	volumeMounts   []corev1.VolumeMount
	initContainers []corev1.Container
}

// gitCloneOpts carries optional checkout info for the git-clone init
// container. Zero values keep the legacy behavior (clone default
// branch, no checkout step) so build pods that don't care about a
// specific branch can pass workspacePlanOpts{}.
type gitCloneOpts struct {
	branch      string
	baseBranch  string
	projectPath string
}

// workspaceRelPath derives the workspace-relative project path from a pod
// path like "/workspace/libs/mcp-go" ("libs/mcp-go"). Returns "" for paths
// that are not under /workspace (or are /workspace itself), letting callers
// fall back to legacy basename behavior.
func workspaceRelPath(podPath string) string {
	p := strings.TrimSuffix(strings.TrimSpace(podPath), "/")
	const root = "/workspace"
	if p == root {
		return ""
	}
	if !strings.HasPrefix(p, root+"/") {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(p, root+"/"), "/")
}

// joinRepoURL joins the git base URL and a repo path, deduplicating one
// shared boundary segment so a legacy group-suffixed base (".../services")
// composed with a group-qualified path ("services/loom-core") still yields
// ".../services/loom-core.git". This keeps clones correct in either rollout
// order while the base URL config migrates to the group-neutral root.
func joinRepoURL(base, repoPath string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	p := strings.Trim(strings.TrimSpace(repoPath), "/")
	if first, rest, ok := strings.Cut(p, "/"); ok && strings.HasSuffix(b, "/"+first) {
		p = rest
	}
	return b + "/" + p + ".git"
}

// RepoProjectPath resolves the git host's API origin ("scheme://host") and
// the host-relative project path ("group/repo") of the repository that the
// git-clone init container would clone for base+repoPath. It applies the
// same boundary-segment dedup as the clone URL so a caller talking to the
// host's HTTP API (rather than its git transport) names the same project the
// sandbox hydrates from. ok is false when base is not an absolute URL.
func RepoProjectPath(base, repoPath string) (apiBase, projectPath string, ok bool) {
	u, err := url.Parse(joinRepoURL(base, repoPath))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", false
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if p == "" {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host, p, true
}

// workspacePodPlan returns the workspace volume and any initContainers needed
// to materialize source content for runtime or build pods.
//
// When the caller did not supply an explicit git project path, it is derived
// from the clone target's /workspace-relative path — pods place a repo at
// /workspace/<bucket>/<name>, which is exactly the repo's path under the
// GitLab host. This fixes group-qualified clones (libs/…) for callers that
// never threaded projectPath (buildah build pods, the devbox manager's
// sandbox start path) without touching their call chains.
func (k *K8sBackend) workspacePlan(cloneTarget string, opts gitCloneOpts, emptyDirSizeLimit *resource.Quantity) workspacePodPlan {
	if strings.TrimSpace(opts.projectPath) == "" {
		opts.projectPath = workspaceRelPath(cloneTarget)
	}
	plan := workspacePodPlan{
		volumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
		},
	}

	switch {
	case k.syncMode == "tar-pipe":
		// Tar-pipe mode uses an emptyDir that is populated after pod start.
		plan.volume = emptyDirWorkspaceVolume(emptyDirSizeLimit)
	case k.gitEnabled():
		// Git-clone mode also uses emptyDir, but hydrates it before start.
		plan.volume = emptyDirWorkspaceVolume(emptyDirSizeLimit)
		plan.initContainers = []corev1.Container{k.gitCloneInitContainer(cloneTarget, opts)}
	default:
		plan.volume = pvcWorkspaceVolume(k.workspacePVC)
	}

	return plan
}

func emptyDirWorkspaceVolume(sizeLimit *resource.Quantity) corev1.Volume {
	return corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: sizeLimit,
			},
		},
	}
}

func pvcWorkspaceVolume(claimName string) corev1.Volume {
	return corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: claimName,
			},
		},
	}
}

func resourcePtr(q resource.Quantity) *resource.Quantity { return &q }
