package backend

import (
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
	branch     string
	baseBranch string
}

// workspacePodPlan returns the workspace volume and any initContainers needed
// to materialize source content for runtime or build pods.
func (k *K8sBackend) workspacePlan(cloneTarget string, opts gitCloneOpts, emptyDirSizeLimit *resource.Quantity) workspacePodPlan {
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
