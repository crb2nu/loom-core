package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

const (
	harvesterManagedByLabel = "mcp-devbox-harvester"
	spawnManagedByLabel     = "loom-spawn"
	defaultHarvesterNS      = "default"
	defaultHarvesterNAD     = "default/lan10g"
	defaultHarvesterVCPUs   = 2
	defaultHarvesterMemMi   = 4096
	defaultHarvesterDiskGi  = 20
	// defaultHarvesterSSHUser mirrors the K8s spawn pod's agent user so
	// mounted credential paths under /home/agent resolve to the SSH user's
	// home dir (Slice 2d.5c). Must equal vmAgentUser in harvester_vm_objects.go.
	defaultHarvesterSSHUser = vmAgentUser

	// vmReadyTimeout bounds cold-boot from manifest creation to SSH-ready.
	// Slice 1.5 measured 130s on the apt-get path and projects ≤60s on the
	// pre-baked image. 5min leaves headroom for Longhorn provisioning + a
	// scheduler stall under load.
	vmReadyTimeout = 5 * time.Minute

	// vmReadyPollInterval is how often we re-check VMI phase + conditions
	// while waiting for Running + AgentConnected.
	vmReadyPollInterval = 2 * time.Second

	// sshConnectTimeout caps the SSH dial.
	sshConnectTimeout = 10 * time.Second

	// execDefaultTimeout matches K8sBackend.Exec.
	execDefaultTimeout = 5 * time.Minute

	// provisionTimeout bounds the post-boot SSH provisioning step (workspace
	// clone + guarded agent-CLI install). On a bare Ubuntu base image this
	// includes a cold `apt-get` + `npm install -g`, so it is generous; on a
	// curated base image the guarded installs are no-ops and it returns in
	// seconds.
	provisionTimeout = 10 * time.Minute

	// The cloud-init Secret doubles as the durable SSH credential envelope.
	// KubeVirt reads only `userdata`; the private key remains cluster-side.
	cloudInitSSHPrivateKeyDataKey = "ssh-private-key"
	cloudInitSSHPublicKeyDataKey  = "ssh-public-key"
	cloudInitSecretUIDAnnotation  = "loom.dev/cloudinit-secret-uid"
	sshKeyFingerprintAnnotation   = "loom.dev/ssh-key-fingerprint"
)

type harvesterSSHCredential struct {
	signer    ssh.Signer
	vmUID     types.UID
	secretUID types.UID
}

type sshKeyMaterial struct {
	signer        ssh.Signer
	authorizedKey string
	privatePEM    []byte
	fingerprint   string
}

// HarvesterVMBackendConfig configures the per-run KubeVirt VM backend on
// a Harvester cluster.
//
// Assumes a pre-baked Ubuntu 24.04 image registered as a Harvester
// VirtualMachineImage in the same namespace (see
// `.loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md`).
// `BaseImageName` is informational; actual disk hydration goes through
// the auto-generated `longhorn-image-<id>` storage class referenced by
// `StorageClassName`.
type HarvesterVMBackendConfig struct {
	// KubeconfigPath points at the Harvester admin kubeconfig (e.g.,
	// ~/workspace/platform/gitops/.kube/harvester-admin.yaml). Empty
	// means try in-cluster then ~/.kube/config — matches K8sBackend.
	KubeconfigPath string

	// BaseImageName is the curated VirtualMachineImage (e.g.,
	// "mills-devbox-base-2026-05-25"). Build returns it verbatim; image
	// production is owned by a separate GitOps pipeline.
	BaseImageName string

	// Namespace is the Kubernetes namespace for VMs + PVCs. Default
	// "default" because Slice 1.5 confirmed cross-NS CDI clone is
	// RBAC-blocked on Harvester and the same-NS PVC pattern works.
	Namespace string

	// StorageClassName is the storage class for per-VM OS PVCs. For
	// Harvester this is the auto-generated `longhorn-image-<id>` for
	// the BaseImageName.
	StorageClassName string

	// NetworkAttachmentDef is the multus net-attach-def reference
	// (`<namespace>/<name>`). Default "default/lan10g" — the plain
	// bridge confirmed working in Slice 1.5. Not a Whereabouts NAD;
	// the LAN has no free IPAM range (spec refinements).
	NetworkAttachmentDef string

	// Per-VM shape. Override CPUs/MemoryMB via StartOpts; disk is fixed
	// at config time.
	DefaultVCPUs  int
	DefaultMemMi  int
	DefaultDiskGi int

	// SSHUser is the cloud-init-provisioned login user. Default "agent"
	// (defaultHarvesterSSHUser) — the user buildVMManifest creates with
	// HOME=/home/agent so mounted credential paths resolve correctly
	// (Slice 2d.5c). Override only if a custom base image uses another user.
	SSHUser string

	// SecretResolver, when non-nil, lets Start flatten StartOpts.SecretEnv
	// into plaintext values that are baked into the VM's cloud-init env
	// file. K8s pods get Secret resolution via the API server's SecretKeyRef
	// machinery; Harvester VMs have no equivalent, so the orchestrator
	// passes its K8sBackend (which implements SecretResolver natively) when
	// it wires this backend up.
	//
	// Nil-safe: when nil, Start logs a warning if StartOpts.SecretEnv is
	// non-empty and proceeds with StartOpts.Env only.
	SecretResolver SecretResolver

	// GitBaseURL is the base git URL for workspace repos (e.g.,
	// "http://192.168.50.218/services"). When set, Start clones
	// "<GitBaseURL>/<project>.git" into StartOpts.WorkDir over SSH after the
	// VM is ready, mirroring the K8s backend's git-clone init container — the
	// VM's disk IS the worktree (spec §"Why a new backend"). The project name
	// is the last path segment of WorkDir. Empty disables cloning (Start
	// leaves the workspace untouched), preserving pre-provisioning behavior
	// for non-Mills callers.
	GitBaseURL string

	// GitSecret is the K8s Secret (resolved via SecretResolver) whose `token`
	// key holds the git access token used for the clone. Matches the K8s
	// backend's gitSecret. Only consulted when GitBaseURL is set; an empty
	// token then fails Start with an actionable error rather than cloning
	// anonymously against a private repo.
	GitSecret string
}

// HarvesterVMBackend implements Backend using KubeVirt VirtualMachines
// on a Harvester cluster. Cold-boot path only — warm pool ships in
// Slice 3 of the spec; serial-console fallback is stubbed to
// ErrNotSupported.
type HarvesterVMBackend struct {
	cfg HarvesterVMBackendConfig

	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	restConfig      *rest.Config

	logger *slog.Logger

	// Per-VM ed25519 credentials cached for SSH. The durable private key is
	// generation-scoped in the VM's cloud-init Secret; these UID bindings
	// prevent a same-name replacement from reusing an older cached signer.
	keysMu sync.RWMutex
	keys   map[string]harvesterSSHCredential
}

// NewHarvesterVMBackend wires up the dynamic + discovery clients against
// the configured Harvester kubeconfig.
func NewHarvesterVMBackend(cfg HarvesterVMBackendConfig) (*HarvesterVMBackend, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = defaultHarvesterNS
	}
	if cfg.NetworkAttachmentDef == "" {
		cfg.NetworkAttachmentDef = defaultHarvesterNAD
	}
	if cfg.DefaultVCPUs <= 0 {
		cfg.DefaultVCPUs = defaultHarvesterVCPUs
	}
	if cfg.DefaultMemMi <= 0 {
		cfg.DefaultMemMi = defaultHarvesterMemMi
	}
	if cfg.DefaultDiskGi <= 0 {
		cfg.DefaultDiskGi = defaultHarvesterDiskGi
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = defaultHarvesterSSHUser
	}
	if cfg.StorageClassName == "" {
		return nil, fmt.Errorf("HarvesterVMBackendConfig.StorageClassName is required")
	}

	restConfig, err := buildRestConfig(cfg.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build harvester kubeconfig: %w", err)
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}

	return &HarvesterVMBackend{
		cfg:             cfg,
		dynamicClient:   dyn,
		discoveryClient: disc,
		restConfig:      restConfig,
		logger:          slog.Default().With("backend", "harvester-vm"),
		keys:            make(map[string]harvesterSSHCredential),
	}, nil
}

// Ensure HarvesterVMBackend implements Backend at compile time.
var _ Backend = (*HarvesterVMBackend)(nil)

// SetLogger overrides the default logger.
func (h *HarvesterVMBackend) SetLogger(l *slog.Logger) {
	if l != nil {
		h.logger = l.With("backend", "harvester-vm")
	}
}

// Build is a no-op for the harvester-vm backend. The curated base image
// is produced by a separate GitOps pipeline. Returns BaseImageName so
// the caller has something to surface in state/logs.
func (h *HarvesterVMBackend) Build(_ context.Context, opts BuildOpts) (*BuildResult, error) {
	tag := opts.Tag
	if h.cfg.BaseImageName != "" {
		tag = h.cfg.BaseImageName
	}
	if tag == "" {
		tag = "harvester-vm-base"
	}
	return &BuildResult{ImageTag: tag, Cached: true}, nil
}

// Start creates the per-VM PVC + VirtualMachine, waits for Running +
// AgentConnected, and returns the VM name as ContainerID. SSH details
// are kept internally; callers reach the VM via Exec/ReadFile/WriteFile.
func (h *HarvesterVMBackend) Start(ctx context.Context, opts StartOpts) (*StartResult, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("StartOpts.Name is required")
	}
	expectedIdentity := expectedStartIdentityLabels(opts)
	identityMode := len(expectedIdentity) > 0

	// Fast path: a fully-fenced Running VM can recover its persisted SSH key
	// after an operator restart. Identity-managed resources must already have
	// their exact labels; migration authority never authorizes key recovery.
	if existingVM, err := h.getVM(ctx, opts.Name); err == nil {
		if identityMode {
			if _, err := validateIdentityLabels(
				"VM "+existingVM.GetName(), existingVM.GetLabels(), expectedIdentity, false,
			); err != nil {
				return nil, err
			}
		}
		existingVM, err = h.ensureDynamicStartIdentity(ctx, vmGVR, opts.Name, opts)
		if err != nil {
			return nil, err
		}
		phase, phaseErr := vmiPhase(ctx, h.dynamicClient, h.cfg.Namespace, opts.Name)
		if phaseErr != nil && !apierrors.IsNotFound(phaseErr) {
			return nil, fmt.Errorf("get existing VMI phase: %w", phaseErr)
		}
		terminating := existingVM.GetDeletionTimestamp() != nil
		if phase == "Running" && !terminating {
			if identityMode {
				credential, recoverErr := h.recoverSSHCredential(ctx, opts, existingVM)
				if recoverErr != nil {
					return nil, fmt.Errorf("recover persisted SSH credential for VM %s: %w", opts.Name, recoverErr)
				}
				h.putCredential(opts.Name, credential)
				h.logger.Info("reusing existing VM", "name", opts.Name)
				return &StartResult{ContainerID: opts.Name}, nil
			}
			if h.hasKey(opts.Name) {
				h.logger.Info("reusing existing VM", "name", opts.Name)
				return &StartResult{ContainerID: opts.Name}, nil
			}
		}
		if identityMode {
			return nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("VM %s matches durable identity but is not safely reusable (phase=%q, ssh_key_available=%t, terminating=%t)",
					opts.Name, phase, h.hasKey(opts.Name), terminating),
			)
		}
		// Stale (no in-memory key → can't SSH) — recreate.
		h.logger.Info("stale VM found, recreating", "name", opts.Name, "phase", phase)
		if err := h.StopIfIdentity(ctx, opts.Name, expectedIdentity); err != nil {
			return nil, fmt.Errorf("replace existing VM: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get existing VM: %w", err)
	}

	keyMaterial, err := generateSSHKeyMaterial()
	if err != nil {
		return nil, fmt.Errorf("generate ssh key: %w", err)
	}

	env, err := h.resolveStartEnv(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("resolve secret env: %w", err)
	}

	files, err := h.resolveStartMounts(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("resolve secret mounts: %w", err)
	}

	objs, err := buildVMManifest(opts, h.cfg, keyMaterial.authorizedKey, env, files)
	if err != nil {
		return nil, fmt.Errorf("build vm manifest: %w", err)
	}
	if err := persistSSHKeyMaterial(objs.CloudInitSecret, keyMaterial); err != nil {
		return nil, fmt.Errorf("persist ssh key material: %w", err)
	}
	type createdResource struct {
		gvr  schema.GroupVersionResource
		name string
		uid  types.UID
	}
	createdChildren := make([]createdResource, 0, 2)
	cleanupCreatedChildrenWithContext := func(cleanupCtx context.Context) error {
		var cleanupErrs []error
		for i := len(createdChildren) - 1; i >= 0; i-- {
			resource := createdChildren[i]
			if err := h.deleteDynamicIfExactIdentity(
				cleanupCtx, resource.gvr, resource.name, resource.uid, expectedIdentity,
			); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		return errors.Join(cleanupErrs...)
	}
	cleanupCreatedChildren := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return cleanupCreatedChildrenWithContext(cleanupCtx)
	}

	// Create the cloud-init Secret first so VM-create failure can clean it up
	// and so the VM's userDataSecretRef resolves on first boot.
	createdSecret, err := h.dynamicClient.Resource(secretGVR).
		Namespace(h.cfg.Namespace).
		Create(ctx, objs.CloudInitSecret, metav1.CreateOptions{})
	if err == nil {
		createdChildren = append(createdChildren, createdResource{
			gvr: secretGVR, name: createdSecret.GetName(), uid: createdSecret.GetUID(),
		})
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create cloudinit secret: %w", err)
	} else {
		if identityMode {
			return nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("cloud-init Secret %s already exists for an identity-managed VM", objs.CloudInitSecret.GetName()),
			)
		}
		createdSecret, err = h.dynamicClient.Resource(secretGVR).
			Namespace(h.cfg.Namespace).
			Get(ctx, objs.CloudInitSecret.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get existing cloudinit secret: %w", err)
		}
		createdSecret, err = h.ensureDynamicStartIdentity(ctx, secretGVR, createdSecret.GetName(), opts)
		if err != nil {
			return nil, err
		}
	}
	if createdSecret.GetUID() == "" {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s has no UID", createdSecret.GetName()),
			cleanupCreatedChildren(),
		)
	}
	bindVMToSSHSecret(objs.VM, createdSecret.GetUID(), keyMaterial.fingerprint)

	createdPVC, err := h.dynamicClient.Resource(pvcGVR).
		Namespace(h.cfg.Namespace).
		Create(ctx, objs.PVC, metav1.CreateOptions{})
	if err == nil {
		createdChildren = append(createdChildren, createdResource{
			gvr: pvcGVR, name: createdPVC.GetName(), uid: createdPVC.GetUID(),
		})
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, errors.Join(fmt.Errorf("create pvc: %w", err), cleanupCreatedChildren())
	} else {
		if identityMode {
			return nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("PVC %s already exists for an identity-managed VM", objs.PVC.GetName()),
				cleanupCreatedChildren(),
			)
		}
		createdPVC, err = h.dynamicClient.Resource(pvcGVR).
			Namespace(h.cfg.Namespace).
			Get(ctx, objs.PVC.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("get existing pvc: %w", err), cleanupCreatedChildren())
		}
		createdPVC, err = h.ensureDynamicStartIdentity(ctx, pvcGVR, createdPVC.GetName(), opts)
		if err != nil {
			return nil, errors.Join(err, cleanupCreatedChildren())
		}
	}

	createdVM, err := h.dynamicClient.Resource(vmGVR).
		Namespace(h.cfg.Namespace).
		Create(ctx, objs.VM, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			if _, probeErr := h.ProbeStartIdentity(ctx, opts); probeErr != nil {
				return nil, errors.Join(probeErr, cleanupCreatedChildren())
			}
			return nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("create VM %s: a same-name VM appeared during startup", opts.Name),
				cleanupCreatedChildren(),
			)
		}
		return nil, errors.Join(fmt.Errorf("create vm: %w", err), cleanupCreatedChildren())
	}
	if createdVM.GetUID() == "" {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("created VM %s has no UID", createdVM.GetName()),
			cleanupCreatedChildren(),
		)
	}
	cleanupCreatedVMAndChildren := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vmErr := h.deleteDynamicIfExactIdentity(
			cleanupCtx, vmGVR, createdVM.GetName(), createdVM.GetUID(), expectedIdentity,
		)
		if vmErr == nil {
			h.deleteKeyIfVMUID(opts.Name, createdVM.GetUID())
		}
		return errors.Join(vmErr, cleanupCreatedChildrenWithContext(cleanupCtx))
	}

	// Patch owner references so VM delete cascades the PVC + cloud-init Secret.
	// Retry on conflict: the CDI/Longhorn + secret controllers mutate these
	// objects concurrently right after create, and a lost ownerRef update
	// orphans a 20Gi PVC every run (only swept later by CleanupBuilds).
	if err := h.setOwnerRefWithRetry(
		ctx, pvcGVR, createdPVC.GetName(), createdPVC.GetUID(), expectedIdentity, createdVM,
	); err != nil {
		if identityMode {
			return nil, errors.Join(fmt.Errorf("set pvc ownerRef: %w", err), cleanupCreatedVMAndChildren())
		}
		h.logger.Warn("set pvc ownerRef failed (orphan cleanup will sweep later)",
			"vm", opts.Name, "error", err)
	}
	if err := h.setOwnerRefWithRetry(
		ctx, secretGVR, createdSecret.GetName(), createdSecret.GetUID(), expectedIdentity, createdVM,
	); err != nil {
		if identityMode {
			return nil, errors.Join(fmt.Errorf("set cloudinit secret ownerRef: %w", err), cleanupCreatedVMAndChildren())
		}
		h.logger.Warn("set cloudinit secret ownerRef failed (orphan cleanup will sweep later)",
			"vm", opts.Name, "error", err)
	}

	// Register the SSH key BEFORE waiting so a concurrent Stop on a
	// failed VM can still find the entry to clean.
	h.putCredential(opts.Name, harvesterSSHCredential{
		signer: keyMaterial.signer, vmUID: createdVM.GetUID(), secretUID: createdSecret.GetUID(),
	})

	if err := h.waitForVMReady(ctx, opts.Name, vmReadyTimeout); err != nil {
		return nil, errors.Join(fmt.Errorf("vm not ready: %w", err), cleanupCreatedVMAndChildren())
	}

	h.logger.Info("VM ready", "name", opts.Name)

	// Provision the workspace + agent runtime on the freshly-booted VM. Unlike
	// the K8s backend — where Build bakes the agent CLI into the runtime image
	// and a git-clone init container hydrates the workspace — the harvester-vm
	// Build is a no-op against one shared base image, so neither the repo nor
	// the CLI exists until we put them there. Skipping this is exactly what
	// produced the Mills A2 empty-diff kill-test failure (`cd: ...: No such
	// file or directory` + `codex: command not found`, exit 127).
	if err := h.provisionVM(ctx, opts); err != nil {
		return nil, errors.Join(fmt.Errorf("provision vm: %w", err), cleanupCreatedVMAndChildren())
	}

	return &StartResult{ContainerID: opts.Name}, nil
}

// ProbeStartIdentity validates a deterministic VM name without changing it.
func (h *HarvesterVMBackend) ProbeStartIdentity(ctx context.Context, opts StartOpts) (bool, error) {
	vm, err := h.getVM(ctx, opts.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe VM %s identity: %w", opts.Name, err)
	}
	if _, err := validateIdentityLabels(
		"VM "+vm.GetName(), vm.GetLabels(), expectedStartIdentityLabels(opts), opts.AllowMissingIdentityLabels,
	); err != nil {
		return true, err
	}
	return true, nil
}

func (h *HarvesterVMBackend) ensureDynamicStartIdentity(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	name string,
	opts StartOpts,
) (*unstructured.Unstructured, error) {
	var result *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := h.dynamicClient.Resource(gvr).Namespace(h.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		missing, err := validateIdentityLabels(
			obj.GetKind()+" "+name, obj.GetLabels(), expectedStartIdentityLabels(opts), opts.AllowMissingIdentityLabels,
		)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			result = obj
			return nil
		}
		next := obj.DeepCopy()
		labels := next.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		for key, value := range missing {
			labels[key] = value
		}
		next.SetLabels(labels)
		result, err = h.dynamicClient.Resource(gvr).Namespace(h.cfg.Namespace).Update(ctx, next, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("ensure %s %s identity: %w", gvr.Resource, name, err)
	}
	return result, nil
}

func persistSSHKeyMaterial(secret *unstructured.Unstructured, material sshKeyMaterial) error {
	if secret == nil || material.signer == nil || len(material.privatePEM) == 0 {
		return fmt.Errorf("incomplete SSH key material")
	}
	stringData, found, err := unstructured.NestedStringMap(secret.Object, "stringData")
	if err != nil {
		return fmt.Errorf("read Secret stringData: %w", err)
	}
	if !found {
		stringData = make(map[string]string)
	}
	stringData[cloudInitSSHPrivateKeyDataKey] = string(material.privatePEM)
	stringData[cloudInitSSHPublicKeyDataKey] = material.authorizedKey

	// Write `data` directly so the dynamic client and tests observe the same
	// representation returned by a real API server. `stringData` is write-only.
	data, _, err := unstructured.NestedStringMap(secret.Object, "data")
	if err != nil {
		return fmt.Errorf("read Secret data: %w", err)
	}
	if data == nil {
		data = make(map[string]string, len(stringData))
	}
	for key, value := range stringData {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	if err := unstructured.SetNestedStringMap(secret.Object, data, "data"); err != nil {
		return fmt.Errorf("set Secret data: %w", err)
	}
	delete(secret.Object, "stringData")
	annotations := secret.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[sshKeyFingerprintAnnotation] = material.fingerprint
	secret.SetAnnotations(annotations)
	return nil
}

func bindVMToSSHSecret(vm *unstructured.Unstructured, secretUID types.UID, fingerprint string) {
	annotations := vm.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[cloudInitSecretUIDAnnotation] = string(secretUID)
	annotations[sshKeyFingerprintAnnotation] = fingerprint
	vm.SetAnnotations(annotations)
}

func (h *HarvesterVMBackend) recoverSSHCredential(
	ctx context.Context,
	opts StartOpts,
	vm *unstructured.Unstructured,
) (harvesterSSHCredential, error) {
	expectedLabels := expectedStartIdentityLabels(opts)
	secretName, secretUID, fingerprint, err := pinnedSSHBinding(vm, expectedLabels)
	if err != nil {
		return harvesterSSHCredential{}, err
	}
	if vm.GetDeletionTimestamp() != nil {
		return harvesterSSHCredential{}, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("VM %s is terminating", vm.GetName()),
		)
	}

	secret, err := h.dynamicClient.Resource(secretGVR).
		Namespace(h.cfg.Namespace).
		Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return harvesterSSHCredential{}, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("pinned cloud-init Secret %s is missing", secretName),
			)
		}
		return harvesterSSHCredential{}, fmt.Errorf("get pinned cloud-init Secret %s: %w", secretName, err)
	}
	if err := validateCloudInitSecretForVM(secret, vm, secretUID, fingerprint, expectedLabels); err != nil {
		return harvesterSSHCredential{}, err
	}
	signer, err := signerFromCloudInitSecret(secret, fingerprint)
	if err != nil {
		return harvesterSSHCredential{}, err
	}

	// Close the VM replacement window after reading the Secret. A same-name
	// VM created during recovery has a different UID and cannot inherit the
	// recovered signer.
	latestVM, err := h.getVM(ctx, vm.GetName())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return harvesterSSHCredential{}, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("VM %s disappeared during SSH credential recovery", vm.GetName()),
			)
		}
		return harvesterSSHCredential{}, fmt.Errorf("re-read VM %s after SSH credential recovery: %w", vm.GetName(), err)
	}
	latestSecretName, latestSecretUID, latestFingerprint, err := pinnedSSHBinding(latestVM, expectedLabels)
	if err != nil {
		return harvesterSSHCredential{}, err
	}
	if latestVM.GetUID() != vm.GetUID() || latestSecretName != secretName ||
		latestSecretUID != secretUID || latestFingerprint != fingerprint || latestVM.GetDeletionTimestamp() != nil {
		return harvesterSSHCredential{}, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("VM %s identity changed during SSH credential recovery", vm.GetName()),
		)
	}
	return harvesterSSHCredential{signer: signer, vmUID: vm.GetUID(), secretUID: secretUID}, nil
}

func pinnedSSHBinding(
	vm *unstructured.Unstructured,
	expectedLabels map[string]string,
) (string, types.UID, string, error) {
	if vm == nil || vm.GetUID() == "" {
		return "", "", "", errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("VM has no UID for SSH credential binding"),
		)
	}
	if _, err := validateIdentityLabels("VM "+vm.GetName(), vm.GetLabels(), expectedLabels, false); err != nil {
		return "", "", "", err
	}
	secretName, err := cloudInitSecretNameFromVM(vm)
	if err != nil {
		return "", "", "", errors.Join(ErrRuntimeIdentityConflict, err)
	}
	annotations := vm.GetAnnotations()
	secretUID := types.UID(annotations[cloudInitSecretUIDAnnotation])
	fingerprint := annotations[sshKeyFingerprintAnnotation]
	if secretUID == "" || fingerprint == "" {
		return "", "", "", errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("VM %s lacks a complete persisted SSH credential binding", vm.GetName()),
		)
	}
	return secretName, secretUID, fingerprint, nil
}

func cloudInitSecretNameFromVM(vm *unstructured.Unstructured) (string, error) {
	volumes, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return "", fmt.Errorf("read VM %s cloud-init volumes: %w", vm.GetName(), err)
	}
	if !found {
		return "", fmt.Errorf("VM %s has no cloud-init volumes", vm.GetName())
	}
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, found, err := unstructured.NestedString(volume, "cloudInitNoCloud", "secretRef", "name")
		if err != nil {
			return "", fmt.Errorf("read VM %s cloud-init Secret reference: %w", vm.GetName(), err)
		}
		if found && name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("VM %s has no cloud-init Secret reference", vm.GetName())
}

func validateCloudInitSecretForVM(
	secret, vm *unstructured.Unstructured,
	expectedSecretUID types.UID,
	expectedFingerprint string,
	expectedLabels map[string]string,
) error {
	if secret.GetUID() == "" || secret.GetUID() != expectedSecretUID {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s UID=%q, want pinned UID %q", secret.GetName(), secret.GetUID(), expectedSecretUID),
		)
	}
	if _, err := validateIdentityLabels("Secret "+secret.GetName(), secret.GetLabels(), expectedLabels, false); err != nil {
		return err
	}
	if got := secret.GetAnnotations()[sshKeyFingerprintAnnotation]; got != expectedFingerprint {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s SSH key fingerprint does not match its VM", secret.GetName()),
		)
	}
	for _, owner := range secret.GetOwnerReferences() {
		if owner.APIVersion == "kubevirt.io/v1" && owner.Kind == "VirtualMachine" &&
			owner.Name == vm.GetName() && owner.UID == vm.GetUID() &&
			owner.Controller != nil && *owner.Controller {
			return nil
		}
	}
	return errors.Join(
		ErrRuntimeIdentityConflict,
		fmt.Errorf("cloud-init Secret %s is not controlled by exact VM UID %q", secret.GetName(), vm.GetUID()),
	)
}

func signerFromCloudInitSecret(secret *unstructured.Unstructured, expectedFingerprint string) (ssh.Signer, error) {
	privatePEM, err := secretDataValue(secret, cloudInitSSHPrivateKeyDataKey)
	if err != nil {
		return nil, err
	}
	publicKey, err := secretDataValue(secret, cloudInitSSHPublicKeyDataKey)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s contains an invalid SSH private key", secret.GetName()),
		)
	}
	parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil || !bytes.Equal(parsedPublic.Marshal(), signer.PublicKey().Marshal()) {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s SSH public/private keys do not match", secret.GetName()),
		)
	}
	if ssh.FingerprintSHA256(signer.PublicKey()) != expectedFingerprint {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s SSH key does not match the VM fingerprint", secret.GetName()),
		)
	}
	return signer, nil
}

func secretDataValue(secret *unstructured.Unstructured, key string) ([]byte, error) {
	data, found, err := unstructured.NestedStringMap(secret.Object, "data")
	if err != nil {
		return nil, fmt.Errorf("read cloud-init Secret %s data: %w", secret.GetName(), err)
	}
	encoded := data[key]
	if !found || encoded == "" {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s is missing %s", secret.GetName(), key),
		)
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("cloud-init Secret %s has invalid %s encoding", secret.GetName(), key),
		)
	}
	return value, nil
}

// deleteDynamicIfExactIdentity deletes only the exact resource returned by a
// successful Create call. The UID pin prevents cleanup from deleting a
// same-name replacement, while the labels prevent an identity-managed caller
// from cleaning a resource whose logical generation changed in place.
func (h *HarvesterVMBackend) deleteDynamicIfExactIdentity(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	name string,
	expectedUID types.UID,
	expectedLabels map[string]string,
) error {
	if expectedUID == "" {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("refusing to delete %s %s without the UID returned by Create", gvr.Resource, name),
		)
	}
	obj, err := h.dynamicClient.Resource(gvr).Namespace(h.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %s %s before exact cleanup: %w", gvr.Resource, name, err)
	}
	if obj.GetUID() != expectedUID {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("%s %s UID=%q, want created UID %q", gvr.Resource, name, obj.GetUID(), expectedUID),
		)
	}
	if _, err := validateIdentityLabels(gvr.Resource+" "+name, obj.GetLabels(), expectedLabels, false); err != nil {
		return err
	}
	uid := expectedUID
	err = h.dynamicClient.Resource(gvr).Namespace(h.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete exact %s %s: %w", gvr.Resource, name, err)
	}
	return nil
}

// StopIfIdentity conditionally deletes a VM and retains the SSH key when the
// current resource belongs to another generation.
func (h *HarvesterVMBackend) StopIfIdentity(ctx context.Context, id string, expectedLabels map[string]string) error {
	vm, err := h.getVM(ctx, id)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get VM before identity-scoped delete: %w", err)
	}
	if _, err := validateIdentityLabels("VM "+id, vm.GetLabels(), expectedLabels, false); err != nil {
		return fmt.Errorf("delete VM with identity precondition: %w", err)
	}
	if vm.GetUID() == "" {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("refusing to delete identity-managed VM %s without a UID", id),
		)
	}

	// New VMs pin the exact Secret that carries their durable SSH key. Validate
	// it before deleting the VM so a same-name replacement is never swept.
	var pinnedSecret *unstructured.Unstructured
	annotations := vm.GetAnnotations()
	hasSecretUID := annotations[cloudInitSecretUIDAnnotation] != ""
	hasFingerprint := annotations[sshKeyFingerprintAnnotation] != ""
	if hasSecretUID != hasFingerprint {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("VM %s has an incomplete persisted SSH credential binding", id),
		)
	}
	if hasSecretUID {
		secretName, secretUID, fingerprint, bindingErr := pinnedSSHBinding(vm, expectedLabels)
		if bindingErr != nil {
			return bindingErr
		}
		pinnedSecret, err = h.dynamicClient.Resource(secretGVR).
			Namespace(h.cfg.Namespace).
			Get(ctx, secretName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get pinned cloud-init Secret before VM delete: %w", err)
		}
		if err == nil {
			if err := validateCloudInitSecretForVM(
				pinnedSecret, vm, secretUID, fingerprint, expectedLabels,
			); err != nil {
				return err
			}
		}
	}

	if pinnedSecret != nil {
		if err := h.deleteDynamicIfExactIdentity(
			ctx, secretGVR, pinnedSecret.GetName(), pinnedSecret.GetUID(), expectedLabels,
		); err != nil {
			return fmt.Errorf("delete persisted SSH Secret with identity precondition: %w", err)
		}
	}
	if err := h.deleteDynamicIfExactIdentity(ctx, vmGVR, id, vm.GetUID(), expectedLabels); err != nil {
		return fmt.Errorf("delete VM with identity precondition: %w", err)
	}
	h.deleteKeyIfVMUID(id, vm.GetUID())
	return nil
}

// Stop deletes the VM. The PVC is cascade-deleted via ownerReferences
// set in Start. Drops the per-VM SSH key from the registry.
func (h *HarvesterVMBackend) Stop(ctx context.Context, id string) error {
	h.deleteKey(id)
	err := h.dynamicClient.Resource(vmGVR).
		Namespace(h.cfg.Namespace).
		Delete(ctx, id, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete vm: %w", err)
	}
	return nil
}

// Status maps VMI phase to StatusResult. Falls back to VM existence when
// the VMI hasn't materialized yet.
func (h *HarvesterVMBackend) Status(ctx context.Context, id string) (*StatusResult, error) {
	phase, err := vmiPhase(ctx, h.dynamicClient, h.cfg.Namespace, id)
	if err == nil {
		return &StatusResult{
			Running: phase == "Running",
			Status:  strings.ToLower(phase),
		}, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get vmi: %w", err)
	}
	if _, err := h.getVM(ctx, id); err != nil {
		if apierrors.IsNotFound(err) {
			return &StatusResult{Running: false, Status: "not_found"}, nil
		}
		return nil, fmt.Errorf("get vm: %w", err)
	}
	return &StatusResult{Running: false, Status: "starting"}, nil
}

// Health verifies the KubeVirt API is reachable.
func (h *HarvesterVMBackend) Health(_ context.Context) error {
	if _, err := h.discoveryClient.ServerVersion(); err != nil {
		return fmt.Errorf("harvester api unreachable: %w", err)
	}
	groups, err := h.discoveryClient.ServerGroups()
	if err != nil {
		return fmt.Errorf("list server groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == "kubevirt.io" {
			return nil
		}
	}
	return fmt.Errorf("kubevirt.io API group not present on cluster")
}

// Pause invokes KubeVirt's pause subresource (virtctl pause equivalent).
// Returns ErrNotSupported when subresources.kubevirt.io isn't served.
func (h *HarvesterVMBackend) Pause(ctx context.Context, id string) error {
	return h.callVMSubresource(ctx, id, "pause")
}

// Resume invokes KubeVirt's unpause subresource.
func (h *HarvesterVMBackend) Resume(ctx context.Context, id string) error {
	return h.callVMSubresource(ctx, id, "unpause")
}

// ReadFile reads a file from the VM. Plain `cat` over SSH is more
// portable than scp and avoids needing a separate scp binary on the
// image.
func (h *HarvesterVMBackend) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, _, err := h.execOverSSH(ctx, id, fmt.Sprintf("cat %s", shellQuote(path)))
	if err != nil {
		return nil, fmt.Errorf("read file %q: %w", path, err)
	}
	return out, nil
}

// WriteFile writes content to a file inside the VM via a stdin-piped
// session. The chmod happens after the cat completes so a partial write
// doesn't leave the file world-readable.
func (h *HarvesterVMBackend) WriteFile(ctx context.Context, id, path string, content []byte, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if mode == "" {
		mode = "0644"
	}
	dir := filepath.Dir(path)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s",
		shellQuote(dir), shellQuote(path), mode, shellQuote(path))

	signer, ok := h.getKey(id)
	if !ok {
		return fmt.Errorf("no ssh key registered for vm %q", id)
	}
	addr, err := h.vmAddress(ctx, id)
	if err != nil {
		return err
	}
	client, err := dialSSH(ctx, addr, h.cfg.SSHUser, signer)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("ssh stdin: %w", err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr

	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("ssh start: %w", err)
	}
	if _, err := stdin.Write(content); err != nil {
		return fmt.Errorf("ssh write content: %w", err)
	}
	if err := stdin.Close(); err != nil {
		return fmt.Errorf("ssh close stdin: %w", err)
	}
	if err := session.Wait(); err != nil {
		return fmt.Errorf("write file %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Exec runs a command in the VM via SSH. Stdout/stderr are tail-truncated
// per opts.MaxLines, matching K8sBackend's contract.
func (h *HarvesterVMBackend) Exec(_ context.Context, opts ExecOpts) (*ExecResult, error) {
	// Detach from request context — long-running tests must survive
	// MCP proxy timeouts. Matches K8sBackend.Exec.
	timeout := execDefaultTimeout
	if opts.TimeoutSec > 0 {
		timeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	shellCmd := opts.Command
	if len(opts.Env) > 0 {
		var envPrefix strings.Builder
		for k, v := range opts.Env {
			envPrefix.WriteString(fmt.Sprintf("export %s=%s; ", k, shellQuote(v)))
		}
		shellCmd = envPrefix.String() + shellCmd
	}
	// Source the cloud-init env file first so any env baked in at Start
	// is honored even when callers don't pass ExecOpts.Env. `2>/dev/null`
	// keeps the prefix safe on VMs where the file is absent (older base
	// images, first-boot races). `set -a` auto-exports the assignments so
	// the values reach the spawned process tree.
	shellCmd = fmt.Sprintf("set -a; . %s 2>/dev/null; set +a; %s", vmEnvFilePath, shellCmd)
	if opts.WorkDir != "" {
		shellCmd = fmt.Sprintf("cd %s && %s", shellQuote(opts.WorkDir), shellCmd)
	}

	start := time.Now()
	stdout, stderr, runErr := h.execOverSSH(ctx, opts.ContainerID, shellCmd)
	durationMs := time.Since(start).Milliseconds()

	exitCode := 0
	if runErr != nil {
		var sshErr *ssh.ExitError
		switch {
		case errors.As(runErr, &sshErr):
			exitCode = sshErr.ExitStatus()
		case ctx.Err() != nil:
			return &ExecResult{
				ExitCode:   124,
				StdoutTail: "command timed out",
				DurationMs: durationMs,
			}, nil
		default:
			// Connection-level error (dial failed, signer missing, etc.)
			// Treat as exit 1 and surface diagnostics via stderr so the
			// caller has something actionable rather than a silent fail.
			exitCode = 1
			if len(stderr) == 0 {
				stderr = []byte("exec error: " + runErr.Error() + "\n")
			}
		}
	}

	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = 20
	}

	stdoutTail, stdoutTotal, stdoutTrunc := TruncateOutput(string(stdout), maxLines)
	stderrTail, stderrTotal, stderrTrunc := TruncateOutput(string(stderr), maxLines)

	return &ExecResult{
		ExitCode:    exitCode,
		StdoutLines: stdoutTotal,
		StderrLines: stderrTotal,
		StdoutTail:  stdoutTail,
		StderrTail:  stderrTail,
		DurationMs:  durationMs,
		Truncated:   stdoutTrunc || stderrTrunc,
		OOMKilled:   exitCode == 137,
	}, nil
}

// CleanupBuilds deletes orphan PVCs and cloud-init Secrets older than maxAge
// that carry the managed-by label but no longer have an owning VM. VM-owned
// objects cascade with the VM and aren't orphans; this sweeps the rare case
// where Start crashed after creating a PVC/Secret but before the VM's
// ownerReference was set.
func (h *HarvesterVMBackend) CleanupBuilds(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	pvcCleaned, err := h.cleanupOrphans(ctx, pvcGVR, "pvc", cutoff)
	if err != nil {
		return pvcCleaned, err
	}
	secretCleaned, err := h.cleanupOrphans(ctx, secretGVR, "cloudinit secret", cutoff)
	if err != nil {
		return pvcCleaned + secretCleaned, err
	}
	return pvcCleaned + secretCleaned, nil
}

// cleanupOrphans deletes managed-by-labeled objects of the given resource that
// are older than cutoff and have no living VirtualMachine owner.
func (h *HarvesterVMBackend) cleanupOrphans(ctx context.Context, gvr schema.GroupVersionResource, kind string, cutoff time.Time) (int, error) {
	listOpts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf(
			"app.kubernetes.io/managed-by in (%s,%s)",
			harvesterManagedByLabel,
			spawnManagedByLabel,
		),
	}
	items, err := h.dynamicClient.Resource(gvr).
		Namespace(h.cfg.Namespace).
		List(ctx, listOpts)
	if err != nil {
		return 0, fmt.Errorf("list %ss: %w", kind, err)
	}

	cleaned := 0
	for i := range items.Items {
		obj := &items.Items[i]
		if obj.GetCreationTimestamp().After(cutoff) {
			continue
		}
		ownerAlive := false
		ownerUnknown := false
		for _, owner := range obj.GetOwnerReferences() {
			if owner.Kind != "VirtualMachine" {
				continue
			}
			vm, err := h.getVM(ctx, owner.Name)
			switch {
			case err == nil && owner.UID != "" && vm.GetUID() == owner.UID:
				ownerAlive = true
			case err != nil && !apierrors.IsNotFound(err):
				ownerUnknown = true
				h.logger.Warn("cleanup: owner lookup failed; retaining child",
					"kind", kind, "name", obj.GetName(), "owner", owner.Name, "error", err)
			}
			if ownerAlive || ownerUnknown {
				break
			}
		}
		if ownerAlive || ownerUnknown {
			continue
		}
		uid := obj.GetUID()
		if uid == "" {
			h.logger.Warn("cleanup: retaining child without UID", "kind", kind, "name", obj.GetName())
			continue
		}
		if err := h.dynamicClient.Resource(gvr).
			Namespace(h.cfg.Namespace).
			Delete(ctx, obj.GetName(), metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			h.logger.Warn("cleanup: delete "+kind+" failed", "name", obj.GetName(), "error", err)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

// resolveStartEnv merges StartOpts.Env with any SecretEnv values resolved
// through the configured SecretResolver. Caller-supplied opts.Env keys win
// on conflict so explicit overrides survive Secret resolution. Returns a
// map with the same identity semantics as the inputs (empty map, not nil,
// to keep buildVMManifest's downstream handling simple).
func (h *HarvesterVMBackend) resolveStartEnv(ctx context.Context, opts StartOpts) (map[string]string, error) {
	env := make(map[string]string, len(opts.Env)+len(opts.SecretEnv))
	if len(opts.SecretEnv) > 0 {
		if h.cfg.SecretResolver == nil {
			h.logger.Warn("StartOpts.SecretEnv requested but no SecretResolver configured; secrets will be absent on the VM",
				"vm", opts.Name, "secret_count", len(opts.SecretEnv))
		} else {
			resolved, err := h.cfg.SecretResolver.ResolveSecretEnv(ctx, opts.SecretEnv)
			if err != nil {
				return nil, err
			}
			for k, v := range resolved {
				env[k] = v
			}
		}
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	return env, nil
}

// resolveStartMounts flattens StartOpts.SecretMounts into a slice of
// ResolvedSecretFile entries via the configured SecretResolver. K8s pods
// get file delivery via SecretVolumeSource; Harvester VMs need the bytes
// rendered into the cloud-init userData at boot time.
//
// Nil-safe parity with resolveStartEnv: nil cfg.SecretResolver +
// non-empty opts.SecretMounts logs a warning and returns nil (no files).
// Returning nil — rather than an empty slice — lets buildVMManifest skip
// the write_files render path cleanly when there's nothing to mount.
func (h *HarvesterVMBackend) resolveStartMounts(ctx context.Context, opts StartOpts) ([]ResolvedSecretFile, error) {
	if len(opts.SecretMounts) == 0 {
		return nil, nil
	}
	if h.cfg.SecretResolver == nil {
		h.logger.Warn("StartOpts.SecretMounts requested but no SecretResolver configured; mount files will be absent on the VM",
			"vm", opts.Name, "mount_count", len(opts.SecretMounts))
		return nil, nil
	}
	files, err := h.cfg.SecretResolver.ResolveSecretMounts(ctx, opts.SecretMounts)
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ---------- internals: VM provisioning ----------

// provisionVM hydrates a freshly-booted VM with the project workspace and the
// agent runtime. It is the harvester-vm analogue of two K8s-backend mechanisms
// the no-op VM Build skips: the per-agent runtime image (agent CLI) and the
// git-clone init container (workspace). The VM's disk IS the worktree, so both
// must be materialized in-place over SSH after boot. The provisioning script is
// guarded/idempotent (`command -v <cli> || install`, clone only when absent) so
// it is a fast no-op on a curated base image that pre-bakes the CLI and on a
// reused VM whose workspace already exists.
func (h *HarvesterVMBackend) provisionVM(ctx context.Context, opts StartOpts) error {
	// Resolve the git token only when a clone is actually requested, so
	// CLI-only provisioning (or no provisioning at all) needs no Secret.
	var gitToken string
	if h.cloneRequested(opts) {
		if h.cfg.SecretResolver == nil {
			return fmt.Errorf("git clone requested (GitBaseURL=%q) but no SecretResolver configured", h.cfg.GitBaseURL)
		}
		vals, err := h.cfg.SecretResolver.ResolveSecretEnv(ctx, []SecretEnvVar{{
			Name:       "GIT_TOKEN",
			SecretName: h.cfg.GitSecret,
			SecretKey:  "token",
		}})
		if err != nil {
			return fmt.Errorf("resolve git token from secret %q: %w", h.cfg.GitSecret, err)
		}
		gitToken = vals["GIT_TOKEN"]
		if gitToken == "" {
			return fmt.Errorf("git token empty (secret %q key \"token\"); cannot clone private repo %s", h.cfg.GitSecret, opts.WorkDir)
		}
	}

	script, doClone := h.buildProvisionScript(opts, gitToken)
	if script == "" {
		return nil // nothing to install and nothing to clone
	}

	provCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	h.logger.Info("provisioning VM",
		"name", opts.Name,
		"clone", doClone,
		"workdir", opts.WorkDir,
		"cli_install", opts.AgentCLIInstallCmd != "")

	start := time.Now()
	_, stderr, err := h.execOverSSH(provCtx, opts.Name, script)
	dur := time.Since(start).Round(time.Second)
	if err != nil {
		// apt/npm/git failures land on stderr — surface the tail so the
		// operator sees the real cause instead of a bare exit code.
		tail, _, _ := TruncateOutput(string(stderr), 20)
		tail = strings.TrimSpace(tail)
		var sshErr *ssh.ExitError
		if errors.As(err, &sshErr) {
			return fmt.Errorf("provision script exited %d after %s: %s", sshErr.ExitStatus(), dur, tail)
		}
		return fmt.Errorf("provision exec failed after %s: %w (stderr: %s)", dur, err, tail)
	}
	h.logger.Info("VM provisioned", "name", opts.Name, "duration", dur.String(), "clone", doClone)
	return nil
}

// cloneRequested reports whether Start should clone a repo into the VM
// workspace: a base URL is configured and WorkDir names a concrete project
// directory (not the bare /workspace root).
func (h *HarvesterVMBackend) cloneRequested(opts StartOpts) bool {
	return h.cfg.GitBaseURL != "" &&
		opts.WorkDir != "" &&
		strings.TrimSuffix(opts.WorkDir, "/") != "/workspace"
}

// buildProvisionScript assembles the idempotent bash run on the VM after boot.
// Returns the script and whether it includes a git clone. An empty string means
// there is nothing to do (no CLI install requested and no clone configured).
//
// The script mirrors K8sBackend.gitCloneInitContainer's branch logic so the VM
// ends up on the requested branch (created from BaseBranch when origin lacks
// it), and runs the orchestrator-supplied AgentCLIInstallCmd to install the CLI
// the no-op VM Build does not bake in.
func (h *HarvesterVMBackend) buildProvisionScript(opts StartOpts, gitToken string) (string, bool) {
	doClone := h.cloneRequested(opts) && gitToken != ""
	cliInstall := strings.TrimSpace(opts.AgentCLIInstallCmd)
	if !doClone && cliInstall == "" {
		return "", false
	}

	user := h.cfg.SSHUser
	if user == "" {
		user = defaultHarvesterSSHUser
	}

	var b strings.Builder
	b.WriteString("set -e\numask 002\n")

	// Base tooling. git is required for the clone and is absent from a stock
	// Ubuntu cloud image; ca-certificates/curl make https work for both the
	// clone and CLI installers. Guarded so it's a no-op on the curated image.
	//
	// DPkg::Lock::Timeout makes apt WAIT for the dpkg lock instead of failing
	// immediately. On first boot cloud-init's own runcmd runs `apt-get install
	// qemu-guest-agent`, which holds /var/lib/dpkg/lock-frontend; this Start-time
	// provision SSHes in as soon as the VM reports ready and races that apt. Without
	// the wait the provision aborts with `exited 100 ... Could not get lock
	// /var/lib/dpkg/lock-frontend ... held by process N (apt-get)` and the spawn
	// never reaches the agent exec (Mills A2 probe 2026-06-18). 300s comfortably
	// covers cloud-init's first-boot apt; on the curated base image the `command -v`
	// guard skips this entirely.
	b.WriteString("if ! command -v git >/dev/null 2>&1; then sudo apt-get -o DPkg::Lock::Timeout=300 update && sudo DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y git ca-certificates curl; fi\n")

	if cliInstall != "" {
		b.WriteString(cliInstall)
		b.WriteString("\n")
	}

	if doClone {
		dest := strings.TrimSuffix(opts.WorkDir, "/")
		parts := strings.Split(dest, "/")
		project := parts[len(parts)-1]
		repoURL := strings.TrimSuffix(h.cfg.GitBaseURL, "/") + "/" + project + ".git"
		scheme := "https"
		if strings.HasPrefix(repoURL, "http://") {
			scheme = "http"
		}
		hostAndPath := strings.TrimPrefix(strings.TrimPrefix(repoURL, "https://"), "http://")
		base := opts.BaseBranch
		if base == "" {
			base = "main"
		}

		// /workspace is root-owned on first boot; hand the tree to the agent
		// user before cloning so the agent (not root) owns the worktree and
		// can write/commit. The token is exported inline (not persisted to the
		// cloud-init env file) so it lives only for this SSH session.
		fmt.Fprintf(&b, "export GIT_TOKEN=%s\n", shellQuote(gitToken))
		fmt.Fprintf(&b, "sudo mkdir -p %s\n", shellQuote(dest))
		fmt.Fprintf(&b, "sudo chown -R %s:%s /workspace\n", user, user)
		fmt.Fprintf(&b, "if [ ! -e %s/.git ]; then git clone \"%s://token:${GIT_TOKEN}@%s\" %s; fi\n",
			shellQuote(dest), scheme, hostAndPath, shellQuote(dest))
		fmt.Fprintf(&b, "cd %s\n", shellQuote(dest))

		if opts.Branch != "" {
			fmt.Fprintf(&b, "BASE=%s\n", shellQuote(base))
			fmt.Fprintf(&b, "if git ls-remote --exit-code --heads origin %s >/dev/null 2>&1; then git checkout %s; ",
				shellQuote(opts.Branch), shellQuote(opts.Branch))
			fmt.Fprintf(&b, "elif git ls-remote --exit-code --heads origin \"${BASE}\" >/dev/null 2>&1; then git checkout -b %s \"origin/${BASE}\"; ",
				shellQuote(opts.Branch))
			fmt.Fprintf(&b, "else git checkout -b %s; fi\n", shellQuote(opts.Branch))
		}
		// Re-assert ownership over anything the clone wrote, then prove the
		// branch in the log tail for operator debugging.
		fmt.Fprintf(&b, "sudo chown -R %s:%s %s\n", user, user, shellQuote(dest))
		b.WriteString("echo \"provision: workspace ready on $(git rev-parse --abbrev-ref HEAD)\"\n")
	}

	return b.String(), doClone
}

// ---------- internals: SSH key management ----------

func (h *HarvesterVMBackend) putCredential(name string, credential harvesterSSHCredential) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	h.keys[name] = credential
}

func (h *HarvesterVMBackend) putKey(name string, signer ssh.Signer) {
	h.putCredential(name, harvesterSSHCredential{signer: signer})
}

func (h *HarvesterVMBackend) getKey(name string) (ssh.Signer, bool) {
	credential, ok := h.getCredential(name)
	return credential.signer, ok && credential.signer != nil
}

func (h *HarvesterVMBackend) getCredential(name string) (harvesterSSHCredential, bool) {
	h.keysMu.RLock()
	defer h.keysMu.RUnlock()
	credential, ok := h.keys[name]
	return credential, ok
}

func (h *HarvesterVMBackend) hasKey(name string) bool {
	h.keysMu.RLock()
	defer h.keysMu.RUnlock()
	_, ok := h.keys[name]
	return ok
}

func (h *HarvesterVMBackend) deleteKey(name string) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	delete(h.keys, name)
}

func (h *HarvesterVMBackend) deleteKeyIfVMUID(name string, vmUID types.UID) {
	if vmUID == "" {
		return
	}
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	credential, ok := h.keys[name]
	if ok && credential.vmUID == vmUID {
		delete(h.keys, name)
	}
}

// generateSSHKey returns an ssh.Signer + the OpenSSH authorized_keys
// representation of the public key. ed25519 because small, fast, and
// universally supported.
func generateSSHKey() (ssh.Signer, string, error) {
	material, err := generateSSHKeyMaterial()
	if err != nil {
		return nil, "", err
	}
	return material.signer, material.authorizedKey, nil
}

func generateSSHKeyMaterial() (sshKeyMaterial, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return sshKeyMaterial{}, fmt.Errorf("ed25519 generate: %w", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "mills-devbox")
	if err != nil {
		return sshKeyMaterial{}, fmt.Errorf("marshal private key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(pemBlock)
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return sshKeyMaterial{}, fmt.Errorf("parse signer: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return sshKeyMaterial{}, fmt.Errorf("ssh public key: %w", err)
	}
	authorized := strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	return sshKeyMaterial{
		signer:        signer,
		authorizedKey: authorized,
		privatePEM:    privatePEM,
		fingerprint:   ssh.FingerprintSHA256(signer.PublicKey()),
	}, nil
}

// ---------- internals: SSH I/O ----------

// execOverSSH opens an SSH session, runs cmd, and returns
// (stdout, stderr, err). When err is an *ssh.ExitError, the command ran
// to completion with a non-zero exit and the buffers are populated.
func (h *HarvesterVMBackend) execOverSSH(ctx context.Context, id, cmd string) ([]byte, []byte, error) {
	credential, ok := h.getCredential(id)
	if !ok || credential.signer == nil {
		return nil, nil, fmt.Errorf("no ssh key registered for vm %q", id)
	}
	if credential.vmUID != "" {
		vm, err := h.getVM(ctx, id)
		if err != nil {
			return nil, nil, fmt.Errorf("validate SSH credential VM identity: %w", err)
		}
		if vm.GetUID() != credential.vmUID ||
			types.UID(vm.GetAnnotations()[cloudInitSecretUIDAnnotation]) != credential.secretUID {
			return nil, nil, errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("refusing to reuse SSH credential for replaced VM %s", id),
			)
		}
	}
	signer := credential.signer
	addr, err := h.vmAddress(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	client, err := dialSSH(ctx, addr, h.cfg.SSHUser, signer)
	if err != nil {
		return nil, nil, fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case err := <-done:
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), ctx.Err()
	}
}

// dialSSH establishes an SSH connection. Skips host-key verification:
// VMs are ephemeral, freshly-provisioned, and we authenticate with a
// keypair we just generated. The threat model is intra-cluster traffic
// to an IP we just learned from KubeVirt — MITM would have to compromise
// the cluster network first.
func dialSSH(ctx context.Context, addr, user string, signer ssh.Signer) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see comment above; VM lifetime + keypair-per-VM threat model
		Timeout:         sshConnectTimeout,
	}
	dialer := &net.Dialer{Timeout: sshConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr, "22"))
	if err != nil {
		return nil, err
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// vmAddress returns the first DHCP-assigned IPv4 address reported by the
// VMI's guest-agent. Start's wait loop guarantees reportedness before
// Exec/ReadFile/WriteFile are reachable, so the "no IP" error here
// signals a regression rather than a transient.
func (h *HarvesterVMBackend) vmAddress(ctx context.Context, name string) (string, error) {
	vmi, err := h.dynamicClient.Resource(vmiGVR).
		Namespace(h.cfg.Namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get vmi %q: %w", name, err)
	}
	ifaces, found, err := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	if err != nil || !found || len(ifaces) == 0 {
		return "", fmt.Errorf("vmi %q has no interface status (guest-agent not connected?)", name)
	}
	for _, raw := range ifaces {
		i, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if ip, ok := i["ipAddress"].(string); ok && ip != "" {
			return ip, nil
		}
		if ips, ok := i["ipAddresses"].([]any); ok {
			for _, raw := range ips {
				if ip, ok := raw.(string); ok && ip != "" {
					return ip, nil
				}
			}
		}
	}
	return "", fmt.Errorf("vmi %q has no reported IPv4 address", name)
}

// ---------- internals: cluster reads ----------

// setOwnerRefWithRetry re-Gets the named object and stamps the VM as its
// owner, retrying on optimistic-concurrency conflicts from CDI/Longhorn and
// Secret controllers. Every retry rechecks the child UID and logical identity,
// so a same-name replacement can never be adopted by the new VM.
func (h *HarvesterVMBackend) setOwnerRefWithRetry(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	name string,
	expectedUID types.UID,
	expectedLabels map[string]string,
	vm *unstructured.Unstructured,
) error {
	if expectedUID == "" {
		return errors.Join(
			ErrRuntimeIdentityConflict,
			fmt.Errorf("refusing to adopt %s %s without the UID returned by Create", gvr.Resource, name),
		)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := h.dynamicClient.Resource(gvr).
			Namespace(h.cfg.Namespace).
			Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if latest.GetUID() != expectedUID {
			return errors.Join(
				ErrRuntimeIdentityConflict,
				fmt.Errorf("%s %s UID=%q, want created UID %q", gvr.Resource, name, latest.GetUID(), expectedUID),
			)
		}
		if _, err := validateIdentityLabels(gvr.Resource+" "+name, latest.GetLabels(), expectedLabels, false); err != nil {
			return err
		}
		applyOwnerReference(latest, vm)
		_, err = h.dynamicClient.Resource(gvr).
			Namespace(h.cfg.Namespace).
			Update(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func (h *HarvesterVMBackend) getVM(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return h.dynamicClient.Resource(vmGVR).
		Namespace(h.cfg.Namespace).
		Get(ctx, name, metav1.GetOptions{})
}

// vmiPhase reads `status.phase` from the VMI. Free function so unit
// tests can drive it directly with a fake dynamic client.
func vmiPhase(ctx context.Context, dyn dynamic.Interface, namespace, name string) (string, error) {
	vmi, err := dyn.Resource(vmiGVR).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	phase, _, err := unstructured.NestedString(vmi.Object, "status", "phase")
	if err != nil {
		return "", err
	}
	return phase, nil
}

// vmiAgentConnected returns true when the VMI carries
// `status.conditions[type=AgentConnected].status=True` — KubeVirt's
// signal that qemu-guest-agent has handshaken back. Slice 1.5 confirmed
// IPv4 reporting depends on this condition flipping.
func vmiAgentConnected(ctx context.Context, dyn dynamic.Interface, namespace, name string) (bool, error) {
	vmi, err := dyn.Resource(vmiGVR).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	conds, found, err := unstructured.NestedSlice(vmi.Object, "status", "conditions")
	if err != nil || !found {
		return false, nil
	}
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "AgentConnected" && c["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

// waitForVMReady polls the VMI until phase=Running AND
// AgentConnected=True AND it has a reported IPv4 address. Polling rather
// than watch because the dynamic-client watch interface adds enough
// boilerplate to be net-negative for a 2s tick on a 5min ceiling.
func (h *HarvesterVMBackend) waitForVMReady(ctx context.Context, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, vmReadyPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		phase, err := vmiPhase(ctx, h.dynamicClient, h.cfg.Namespace, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// VMI not yet materialized; keep waiting until ctx fires.
				return false, nil
			}
			return false, err
		}
		if phase != "Running" {
			return false, nil
		}
		connected, err := vmiAgentConnected(ctx, h.dynamicClient, h.cfg.Namespace, name)
		if err != nil {
			return false, err
		}
		if !connected {
			return false, nil
		}
		if _, err := h.vmAddress(ctx, name); err != nil {
			return false, nil
		}
		return true, nil
	})
}

// callVMSubresource invokes a VM subresource (pause/unpause) via the
// REST client. KubeVirt exposes these under
// /apis/subresources.kubevirt.io/v1/namespaces/<ns>/virtualmachines/<name>/<action>.
//
// Returns ErrNotSupported when subresources.kubevirt.io is absent —
// minimal KubeVirt installs (no virt-api shim) won't have it.
func (h *HarvesterVMBackend) callVMSubresource(ctx context.Context, name, action string) error {
	if !h.subresourcesAvailable() {
		return ErrNotSupported
	}
	subURL := strings.TrimRight(h.restConfig.Host, "/") +
		fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachines/%s/%s",
			h.cfg.Namespace, name, action)
	cli, err := rest.HTTPClientFor(h.restConfig)
	if err != nil {
		return fmt.Errorf("subresource client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, subURL, nil)
	if err != nil {
		return fmt.Errorf("subresource request: %w", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("vm %s: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vm %s: status %d: %s", action, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// subresourcesAvailable returns true when subresources.kubevirt.io is
// served by the cluster. Discovery is cheap; re-fetched per call.
func (h *HarvesterVMBackend) subresourcesAvailable() bool {
	groups, err := h.discoveryClient.ServerGroups()
	if err != nil {
		return false
	}
	for _, g := range groups.Groups {
		if g.Name == "subresources.kubevirt.io" {
			return true
		}
	}
	return false
}

// ---------- helpers ----------

// shellQuote single-quotes a string for safe shell interpolation.
// Sufficient for our caller set (paths, env values) which don't
// legitimately contain raw control characters.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, "'\"`$ \t\n;|&<>()*?[]{}!#~=\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
