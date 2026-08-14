package killtest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	operatorDeploymentKey        = "loom-mills/loom-mills-operator"
	hudDeploymentKey             = "loom-hub/mobile-hud"
	maxReviewedArchiveSize       = int64(512 << 20)
	maxReviewedArchiveEntries    = 50_000
	maxReviewedCommandStderrSize = int64(64 << 10)
	maxReviewedFluxVersionSize   = int64(64 << 10)
	maxReviewedFluxRenderSize    = int64(64 << 20)
	maxReviewedPolicySourceSize  = int64(1 << 20)
	maxReviewedFluxManifestSize  = int64(4 << 20)
)

var errBoundedCommandOutput = errors.New("bounded command output exceeds limit")

type reviewedDeployment struct {
	Deployment         *appsv1.Deployment
	RenderedSpecSHA256 string
	Review             DeploymentReviewIdentity
	// PolicyConfigMap is populated only for the operator entry, from the same
	// apps Flux render bytes as Deployment.
	PolicyConfigMap reviewedPolicyConfigMap
}

type reviewedDeploymentContract struct {
	key, namespace, name string
	fluxOwner            string
	platformManifestPath string
	source               string
}

var reviewedDeploymentContracts = []reviewedDeploymentContract{
	{
		key: operatorDeploymentKey, namespace: "loom-mills", name: "loom-mills-operator",
		fluxOwner:            "apps",
		platformManifestPath: requiredFluxRenderSpecs["apps"].ManifestPath,
		source:               "platform",
	},
	{
		key: hudDeploymentKey, namespace: "loom-hub", name: "mobile-hud",
		fluxOwner:            "loom-hub-servers",
		platformManifestPath: requiredFluxRenderSpecs["loom-hub-servers"].ManifestPath,
		source:               "loom-core",
	},
}

func canonicalDeploymentSpecSHA256(spec appsv1.DeploymentSpec) (string, error) {
	canonical, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func canonicalPodTemplateSHA256(template corev1.PodTemplateSpec) (string, error) {
	canonical, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func canonicalDeploymentPodTemplateSHA256(template corev1.PodTemplateSpec) (string, error) {
	if _, reserved := template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]; reserved {
		return "", fmt.Errorf("deployment pod template declares reserved controller label %q",
			appsv1.DefaultDeploymentUniqueLabelKey)
	}
	return canonicalPodTemplateSHA256(template)
}

func canonicalReplicaSetPodTemplateSHA256(template corev1.PodTemplateSpec) (string, error) {
	controllerHash, exists := template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if !exists || strings.TrimSpace(controllerHash) == "" {
		return "", fmt.Errorf("ReplicaSet pod template has no controller-owned %q label",
			appsv1.DefaultDeploymentUniqueLabelKey)
	}
	canonical := template.DeepCopy()
	delete(canonical.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return canonicalPodTemplateSHA256(*canonical)
}

func labelSelectorDeclaresKey(selector metav1.LabelSelector, key string) bool {
	if _, exists := selector.MatchLabels[key]; exists {
		return true
	}
	for _, expression := range selector.MatchExpressions {
		if expression.Key == key {
			return true
		}
	}
	return false
}

func canonicalLabelSelectorSHA256(selector metav1.LabelSelector) (string, error) {
	canonical, err := json.Marshal(selector)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func canonicalDeploymentSelectorSHA256(selector *metav1.LabelSelector) (string, error) {
	if selector == nil {
		return "", errors.New("deployment selector is missing")
	}
	if labelSelectorDeclaresKey(*selector, appsv1.DefaultDeploymentUniqueLabelKey) {
		return "", fmt.Errorf("deployment selector declares reserved controller label %q",
			appsv1.DefaultDeploymentUniqueLabelKey)
	}
	return canonicalLabelSelectorSHA256(*selector)
}

func canonicalReplicaSetSelectorSHA256(
	selector *metav1.LabelSelector,
	template corev1.PodTemplateSpec,
) (string, error) {
	if selector == nil {
		return "", errors.New("ReplicaSet selector is missing")
	}
	selectorHash, selectorHasHash := selector.MatchLabels[appsv1.DefaultDeploymentUniqueLabelKey]
	templateHash, templateHasHash := template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if !selectorHasHash || !templateHasHash || strings.TrimSpace(selectorHash) == "" ||
		selectorHash != templateHash {
		return "", fmt.Errorf("ReplicaSet selector/template %q labels do not match: %q/%q",
			appsv1.DefaultDeploymentUniqueLabelKey, selectorHash, templateHash)
	}
	for _, expression := range selector.MatchExpressions {
		if expression.Key == appsv1.DefaultDeploymentUniqueLabelKey {
			return "", fmt.Errorf("ReplicaSet selector declares reserved controller label %q as an expression",
				appsv1.DefaultDeploymentUniqueLabelKey)
		}
	}
	canonical := selector.DeepCopy()
	delete(canonical.MatchLabels, appsv1.DefaultDeploymentUniqueLabelKey)
	return canonicalLabelSelectorSHA256(*canonical)
}

// reviewedDeployments reconstructs the exact desired Deployments from the two
// reviewed source commits. It exports only committed Git objects into private
// temporary directories, so a dirty working tree cannot influence the proof.
func (h *Harness) reviewedDeployments(
	ctx context.Context,
	snapshot fluxSourceSnapshot,
) (map[string]reviewedDeployment, error) {
	if h.reviewedDeploymentsFn != nil {
		return h.reviewedDeploymentsFn(ctx, snapshot)
	}
	platformIdentity := snapshot.platform.identity
	loomCoreIdentity := snapshot.loomCore.identity
	if err := validateDeploymentSourceIdentity("platform", platformIdentity); err != nil {
		return nil, err
	}
	if err := validateDeploymentSourceIdentity("loom-core", loomCoreIdentity); err != nil {
		return nil, err
	}
	if strings.TrimSpace(h.cfg.GitOpsRepoPath) == "" || strings.TrimSpace(h.cfg.LoomCoreRepoPath) == "" {
		return nil, errors.New("both GitOps and loom-core repository paths are required to render reviewed Deployments")
	}
	rendererVersion, err := h.fluxRendererVersion(ctx)
	if err != nil {
		return nil, err
	}
	cacheKey := strings.Join([]string{
		platformIdentity.BaselineRevision, platformIdentity.BaselineDigest,
		loomCoreIdentity.BaselineRevision, loomCoreIdentity.BaselineDigest,
		snapshot.platform.renderSpec.SpecSHA256, snapshot.loomCore.renderSpec.SpecSHA256,
		h.cfg.FluxBin, rendererVersion,
	}, "\x00")
	// Rendering the platform apps tree is intentionally expensive and must not
	// be repeated by every immediate/final preflight. Hold the mutex across a
	// cache miss so concurrent probes cannot synthesize two renderer identities.
	h.reviewedDeploymentMu.Lock()
	defer h.reviewedDeploymentMu.Unlock()
	if cached, ok := h.reviewedDeploymentCache[cacheKey]; ok {
		return cloneReviewedDeployments(cached), nil
	}
	if err := ensureGitScopeCommits(ctx, h.cfg.GitOpsRepoPath, "Deployment platform render",
		platformIdentity.BaselineRevision); err != nil {
		return nil, err
	}
	if err := ensureGitScopeCommits(ctx, h.cfg.LoomCoreRepoPath, "Deployment loom-core render",
		loomCoreIdentity.BaselineRevision); err != nil {
		return nil, err
	}

	platformDir, err := exportReviewedGitTree(ctx, h.cfg.GitOpsRepoPath,
		platformIdentity.BaselineRevision,
		[]string{"k3s", reviewedDeploymentContracts[0].platformManifestPath,
			reviewedDeploymentContracts[1].platformManifestPath})
	if err != nil {
		return nil, fmt.Errorf("export reviewed platform tree: %w", err)
	}
	defer os.RemoveAll(platformDir)
	policySource, err := readBoundedRegularFile(
		filepath.Join(platformDir, filepath.FromSlash(policyConfigMapSourcePath)),
		"reviewed policy source", maxReviewedPolicySourceSize,
	)
	if err != nil {
		return nil, fmt.Errorf("read exact reviewed policy source %s: %w", policyConfigMapSourcePath, err)
	}
	loomCoreDir, err := exportReviewedGitTree(ctx, h.cfg.LoomCoreRepoPath,
		loomCoreIdentity.BaselineRevision, []string{"k8s/base"})
	if err != nil {
		return nil, fmt.Errorf("export reviewed loom-core tree: %w", err)
	}
	defer os.RemoveAll(loomCoreDir)

	result := make(map[string]reviewedDeployment, len(reviewedDeploymentContracts))
	for _, contract := range reviewedDeploymentContracts {
		manifestPath := filepath.Join(platformDir, filepath.FromSlash(contract.platformManifestPath))
		if err := rejectDynamicFluxSubstitution(manifestPath); err != nil {
			return nil, fmt.Errorf("reviewed %s transform: %w", contract.fluxOwner, err)
		}
		root := platformDir
		path := filepath.Join(platformDir, "k3s", "flux", "apps")
		if contract.source == "loom-core" {
			root = loomCoreDir
			path = filepath.Join(loomCoreDir, "k8s", "base")
		}
		raw, err := h.runFluxBuild(ctx, root, contract.fluxOwner, path, manifestPath)
		if err != nil {
			return nil, err
		}
		deployment, renderedDigest, err := selectRenderedDeployment(raw, contract.namespace, contract.name)
		if err != nil {
			return nil, fmt.Errorf("select reviewed %s Deployment: %w", contract.key, err)
		}
		sourceIdentity := platformIdentity
		if contract.source == "loom-core" {
			sourceIdentity = loomCoreIdentity
		}
		entry := reviewedDeployment{
			Deployment: deployment, RenderedSpecSHA256: renderedDigest,
			Review: DeploymentReviewIdentity{
				Contract: DeploymentProvenanceContract, ContractVersion: DeploymentProvenanceContractVersion,
				FluxOwner: contract.fluxOwner, FluxSpecSHA256: fluxStateByName(snapshot, contract.fluxOwner).renderSpec.SpecSHA256,
				Renderer: "flux build kustomization --dry-run", RendererVersion: rendererVersion,
				RenderedSpecSHA256:  renderedDigest,
				PlatformRevision:    platformIdentity.BaselineRevision,
				PlatformScopeDigest: platformIdentity.BaselineDigest,
				SourceRevision:      sourceIdentity.BaselineRevision,
				SourceScopeDigest:   sourceIdentity.BaselineDigest,
			},
		}
		if contract.key == operatorDeploymentKey {
			entry.PolicyConfigMap, err = reviewedPolicyConfigMapFromFluxRender(
				raw, policySource, deployment,
				PolicyConfigMapReviewIdentity{
					Contract: PolicyConfigMapProvenanceContract, ContractVersion: PolicyConfigMapProvenanceContractVersion,
					FluxOwner:      policyConfigMapFluxOwner,
					FluxSpecSHA256: fluxStateByName(snapshot, policyConfigMapFluxOwner).renderSpec.SpecSHA256,
					Renderer:       policyConfigMapRenderer, RendererVersion: rendererVersion,
					PlatformRevision:    platformIdentity.BaselineRevision,
					PlatformScopeDigest: platformIdentity.BaselineDigest,
				},
			)
			if err != nil {
				return nil, fmt.Errorf("select reviewed policy ConfigMap: %w", err)
			}
		}
		result[contract.key] = entry
	}
	if h.reviewedDeploymentCache == nil {
		h.reviewedDeploymentCache = make(map[string]map[string]reviewedDeployment)
	}
	h.reviewedDeploymentCache[cacheKey] = cloneReviewedDeployments(result)
	return cloneReviewedDeployments(result), nil
}

func cloneReviewedDeployments(source map[string]reviewedDeployment) map[string]reviewedDeployment {
	clone := make(map[string]reviewedDeployment, len(source))
	for key, value := range source {
		if value.Deployment != nil {
			value.Deployment = value.Deployment.DeepCopy()
		}
		value.PolicyConfigMap = cloneReviewedPolicyConfigMap(value.PolicyConfigMap)
		clone[key] = value
	}
	return clone
}

func validateDeploymentSourceIdentity(label string, identity GitOpsScopeIdentity) error {
	if identity.BaselineRevision == "" || identity.BaselineDigest == "" ||
		identity.ObservedRevision == "" || identity.ObservedDigest == "" {
		return fmt.Errorf("%s source identity is incomplete", label)
	}
	if normalized, err := normalizeGitOpsScopeRevision(identity.BaselineRevision); err != nil ||
		normalized != identity.BaselineRevision {
		return fmt.Errorf("%s review revision %q is invalid: %v", label, identity.BaselineRevision, err)
	}
	return nil
}

func fluxStateByName(snapshot fluxSourceSnapshot, name string) fluxSourceState {
	switch name {
	case "apps":
		return snapshot.platform
	case "bootstrap":
		return snapshot.bootstrap
	case "system":
		return snapshot.system
	case "loom-hub-servers":
		return snapshot.loomCore
	default:
		return fluxSourceState{}
	}
}

func exportReviewedGitTree(
	ctx context.Context,
	repoPath, revision string,
	paths []string,
) (string, error) {
	archive, err := os.CreateTemp("", "mills-s1c-reviewed-*.tar")
	if err != nil {
		return "", err
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()
	if err := runReviewedGitArchive(ctx, repoPath, revision, paths, archive); err != nil {
		return "", err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind reviewed Git archive: %w", err)
	}
	dir, err := os.MkdirTemp("", "mills-s1c-reviewed-")
	if err != nil {
		return "", err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(dir)
		}
	}()

	if err := extractReviewedGitArchive(archive, dir, maxReviewedArchiveEntries); err != nil {
		return "", err
	}
	remove = false
	return dir, nil
}

func extractReviewedGitArchive(reader io.Reader, dir string, entryLimit int) error {
	if entryLimit <= 0 {
		return errors.New("reviewed Git archive entry limit must be positive")
	}
	tarReader := tar.NewReader(reader)
	entries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > entryLimit {
			return fmt.Errorf("reviewed Git archive entry count exceeds %d", entryLimit)
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("reviewed Git archive contains unsafe path %q", header.Name)
		}
		destination := filepath.Join(dir, clean)
		if destination != dir && !strings.HasPrefix(destination, dir+string(filepath.Separator)) {
			return fmt.Errorf("reviewed Git archive path %q escapes destination", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// Git uses a PAX global header for archive metadata such as the
			// source commit. It materializes no filesystem entry.
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxReviewedArchiveSize {
				return fmt.Errorf("reviewed Git archive file %q has invalid size %d", header.Name, header.Size)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o700)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("reviewed Git archive contains unsupported entry %q type %d", header.Name, header.Typeflag)
		}
	}
	return nil
}

func readBoundedReviewedContent(label string, reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("%s reader is missing", label)
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%s byte limit must be positive", label)
	}
	content, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
	}
	return content, nil
}

func readBoundedRegularFile(path, label string, maxBytes int64) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %q is not a regular non-symlink file", label, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("%s %q changed before its read", label, path)
	}
	if openedInfo.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
	}
	content, err := readBoundedReviewedContent(label, file, maxBytes)
	if err != nil {
		return nil, err
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.Mode().IsRegular() ||
		!os.SameFile(openedInfo, currentInfo) || openedInfo.Size() != int64(len(content)) ||
		afterInfo.Size() != int64(len(content)) || !openedInfo.ModTime().Equal(afterInfo.ModTime()) {
		return nil, fmt.Errorf("%s %q changed during its read", label, path)
	}
	return content, nil
}

type strictBoundedCommandWriter struct {
	writer   io.Writer
	limit    int64
	written  int64
	overflow bool
}

func (writer *strictBoundedCommandWriter) Write(payload []byte) (int, error) {
	remaining := writer.limit - writer.written
	if remaining >= int64(len(payload)) {
		written, err := writer.writer.Write(payload)
		writer.written += int64(written)
		if err == nil && written != len(payload) {
			err = io.ErrShortWrite
		}
		return written, err
	}
	writer.overflow = true
	if remaining <= 0 {
		return 0, errBoundedCommandOutput
	}
	written, err := writer.writer.Write(payload[:int(remaining)])
	writer.written += int64(written)
	if err == nil && int64(written) != remaining {
		err = io.ErrShortWrite
	}
	return written, errors.Join(errBoundedCommandOutput, err)
}

type truncatingCommandBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (writer *truncatingCommandBuffer) Write(payload []byte) (int, error) {
	want := len(payload)
	remaining := writer.limit - int64(writer.buffer.Len())
	if remaining <= 0 {
		writer.truncated = writer.truncated || want > 0
		return want, nil
	}
	if remaining < int64(len(payload)) {
		writer.truncated = true
		payload = payload[:int(remaining)]
	}
	_, _ = writer.buffer.Write(payload)
	return want, nil
}

type boundedCommandOutput struct {
	stdoutBytes     int64
	stderr          []byte
	stdoutOverflow  bool
	stderrTruncated bool
}

func runBoundedCommandOutput(
	command *exec.Cmd,
	stdoutDestination io.Writer,
	stdoutLimit, stderrLimit int64,
) (boundedCommandOutput, error) {
	if stdoutLimit < 0 || stderrLimit < 0 {
		return boundedCommandOutput{}, errors.New("bounded command output limits must be non-negative")
	}
	if stdoutDestination == nil {
		return boundedCommandOutput{}, errors.New("bounded command stdout destination is required")
	}
	stdout := strictBoundedCommandWriter{writer: stdoutDestination, limit: stdoutLimit}
	stderr := truncatingCommandBuffer{limit: stderrLimit}
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return boundedCommandOutput{
		stdoutBytes:     stdout.written,
		stderr:          append([]byte(nil), stderr.buffer.Bytes()...),
		stdoutOverflow:  stdout.overflow,
		stderrTruncated: stderr.truncated,
	}, err
}

func runBoundedCommandBytes(
	command *exec.Cmd,
	label string,
	stdoutLimit, stderrLimit int64,
) ([]byte, error) {
	var stdout bytes.Buffer
	output, commandErr := runBoundedCommandOutput(command, &stdout, stdoutLimit, stderrLimit)
	if output.stdoutOverflow {
		return nil, fmt.Errorf("%s stdout exceeds %d bytes", label, stdoutLimit)
	}
	stderr := strings.TrimSpace(string(output.stderr))
	if output.stderrTruncated {
		return nil, fmt.Errorf("%s stderr exceeds %d bytes (prefix %q)", label, stderrLimit, stderr)
	}
	if commandErr != nil {
		if stderr == "" {
			return nil, fmt.Errorf("%s: %w", label, commandErr)
		}
		return nil, fmt.Errorf("%s: %w: %s", label, commandErr, stderr)
	}
	return stdout.Bytes(), nil
}

func runReviewedGitArchive(
	ctx context.Context,
	repoPath, revision string,
	paths []string,
	destination io.Writer,
) error {
	args := append([]string{"archive", "--format=tar", revision, "--"}, paths...)
	gitArgs := append([]string{"-C", repoPath}, args...)
	command := exec.CommandContext(ctx, "git", gitArgs...)
	output, commandErr := runBoundedCommandOutput(
		command, destination, maxReviewedArchiveSize, maxReviewedCommandStderrSize,
	)
	if output.stdoutOverflow {
		return fmt.Errorf("reviewed Git archive exceeds %d bytes", maxReviewedArchiveSize)
	}
	stderr := strings.TrimSpace(string(output.stderr))
	if output.stderrTruncated {
		return fmt.Errorf("git %s stderr exceeds %d bytes (prefix %q)",
			strings.Join(args, " "), maxReviewedCommandStderrSize, stderr)
	}
	if commandErr != nil {
		if stderr == "" {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), commandErr)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), commandErr, stderr)
	}
	return nil
}

func (h *Harness) fluxRendererVersion(ctx context.Context) (string, error) {
	if h.fluxVersionFn != nil {
		return h.fluxVersionFn(ctx)
	}
	command := exec.CommandContext(ctx, h.cfg.FluxBin, "version", "--client")
	out, err := runBoundedCommandBytes(
		command, "read Flux renderer version", maxReviewedFluxVersionSize, maxReviewedCommandStderrSize,
	)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("flux renderer returned an empty version")
	}
	return version, nil
}

func (h *Harness) runFluxBuild(
	ctx context.Context,
	workDir, name, path, manifestPath string,
) ([]byte, error) {
	if h.fluxBuildFn != nil {
		return h.fluxBuildFn(ctx, workDir, name, path, manifestPath)
	}
	command := exec.CommandContext(ctx, h.cfg.FluxBin,
		"build", "kustomization", name,
		"--dry-run", "--path", path, "--kustomization-file", manifestPath)
	command.Dir = workDir
	out, err := runBoundedCommandBytes(command, "render reviewed Flux Kustomization "+name,
		maxReviewedFluxRenderSize, maxReviewedCommandStderrSize)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("render reviewed Flux Kustomization %s: empty output", name)
	}
	return out, nil
}

func rejectDynamicFluxSubstitution(path string) error {
	raw, err := readBoundedRegularFile(path, "reviewed Flux Kustomization manifest", maxReviewedFluxManifestSize)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	if yamlDocumentIsEmpty(&document) {
		return errors.New("flux Kustomization manifest is empty")
	}
	var manifest struct {
		Spec struct {
			PostBuild struct {
				SubstituteFrom []map[string]any `yaml:"substituteFrom"`
			} `yaml:"postBuild"`
		} `yaml:"spec"`
	}
	if err := document.Decode(&manifest); err != nil {
		return err
	}
	if len(manifest.Spec.PostBuild.SubstituteFrom) != 0 {
		return errors.New("dynamic postBuild.substituteFrom is unsupported by the exact offline renderer")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("flux Kustomization manifest contains multiple YAML documents")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing Flux Kustomization document: %w", err)
	}
	return nil
}

func selectRenderedDeployment(raw []byte, namespace, name string) (*appsv1.Deployment, string, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var selected *appsv1.Deployment
	selectedDigest := ""
	for documentIndex := 1; ; documentIndex++ {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("decode rendered document %d: %w", documentIndex, err)
		}
		if yamlDocumentIsEmpty(&document) {
			continue
		}
		var genericDocument any
		if err := document.Decode(&genericDocument); err != nil {
			return nil, "", err
		}
		jsonDocument, err := json.Marshal(genericDocument)
		if err != nil {
			return nil, "", fmt.Errorf("convert rendered document %d to canonical JSON: %w", documentIndex, err)
		}
		var envelope struct {
			APIVersion string            `json:"apiVersion"`
			Kind       string            `json:"kind"`
			Metadata   metav1.ObjectMeta `json:"metadata"`
			Spec       json.RawMessage   `json:"spec"`
			Status     json.RawMessage   `json:"status"`
		}
		if err := json.Unmarshal(jsonDocument, &envelope); err != nil {
			return nil, "", fmt.Errorf("decode rendered document %d identity: %w", documentIndex, err)
		}
		if envelope.APIVersion != "apps/v1" || envelope.Kind != "Deployment" ||
			envelope.Metadata.Namespace != namespace || envelope.Metadata.Name != name {
			continue
		}
		if selected != nil {
			return nil, "", fmt.Errorf("render contains duplicate Deployment %s/%s", namespace, name)
		}
		if len(envelope.Status) != 0 && string(envelope.Status) != "null" && string(envelope.Status) != "{}" {
			return nil, "", fmt.Errorf("reviewed Deployment %s/%s unexpectedly contains status", namespace, name)
		}
		if envelope.Metadata.UID != "" || envelope.Metadata.ResourceVersion != "" ||
			envelope.Metadata.Generation != 0 || envelope.Metadata.DeletionTimestamp != nil {
			return nil, "", fmt.Errorf("reviewed Deployment %s/%s contains live object identity", namespace, name)
		}
		strict := json.NewDecoder(bytes.NewReader(jsonDocument))
		strict.DisallowUnknownFields()
		var deployment appsv1.Deployment
		if err := strict.Decode(&deployment); err != nil {
			return nil, "", fmt.Errorf("strictly decode reviewed Deployment %s/%s: %w", namespace, name, err)
		}
		if err := requireJSONEOF(strict); err != nil {
			return nil, "", err
		}
		var canonicalSpec any
		generic := json.NewDecoder(bytes.NewReader(envelope.Spec))
		generic.UseNumber()
		if err := generic.Decode(&canonicalSpec); err != nil {
			return nil, "", fmt.Errorf("decode rendered Deployment spec: %w", err)
		}
		digest, err := canonicalFluxSpecSHA256(canonicalSpec)
		if err != nil {
			return nil, "", err
		}
		selected, selectedDigest = &deployment, digest
	}
	if selected == nil {
		return nil, "", fmt.Errorf("render contains no exact Deployment %s/%s", namespace, name)
	}
	return selected, selectedDigest, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("rendered JSON contains trailing value")
		}
		return err
	}
	return nil
}

func (h *Harness) bindReviewedDeployment(
	ctx context.Context,
	id DeploymentIdentity,
	live *appsv1.Deployment,
	reviewed reviewedDeployment,
) (DeploymentIdentity, error) {
	if reviewed.Deployment == nil {
		return id, fmt.Errorf("reviewed Deployment %s/%s is missing", id.Namespace, id.Name)
	}
	if live == nil || live.Namespace != id.Namespace || live.Name != id.Name {
		return id, fmt.Errorf("live Deployment object does not match identity %s/%s", id.Namespace, id.Name)
	}
	if reviewed.Deployment.Namespace != id.Namespace || reviewed.Deployment.Name != id.Name {
		return id, fmt.Errorf("reviewed Deployment is %s/%s, want %s/%s",
			reviewed.Deployment.Namespace, reviewed.Deployment.Name, id.Namespace, id.Name)
	}
	normalized, err := h.normalizeReviewedDeployment(ctx, reviewed.Deployment, live)
	if err != nil {
		return id, fmt.Errorf("server-normalize reviewed Deployment %s/%s: %w", id.Namespace, id.Name, err)
	}
	if normalized == nil || normalized.Namespace != id.Namespace || normalized.Name != id.Name {
		return id, fmt.Errorf("server-normalized Deployment identity changed for %s/%s", id.Namespace, id.Name)
	}
	if len(normalized.Spec.Template.Spec.Containers) != 1 ||
		normalized.Spec.Template.Spec.Containers[0].Name != id.ContainerName {
		return id, fmt.Errorf("reviewed Deployment %s/%s main container name changed: live=%q reviewed=%v",
			id.Namespace, id.Name, id.ContainerName, normalized.Spec.Template.Spec.Containers)
	}
	reviewedDigest, err := canonicalDeploymentSpecSHA256(normalized.Spec)
	if err != nil {
		return id, err
	}
	if id.SpecSHA256 != reviewedDigest {
		return id, fmt.Errorf("deployment %s/%s live spec does not match reviewed rendered desired state: %s != %s",
			id.Namespace, id.Name, id.SpecSHA256, reviewedDigest)
	}
	reviewedPodTemplateDigest, err := canonicalDeploymentPodTemplateSHA256(normalized.Spec.Template)
	if err != nil {
		return id, fmt.Errorf("canonicalize reviewed Deployment %s/%s pod template: %w",
			id.Namespace, id.Name, err)
	}
	if id.PodTemplateSHA256 != reviewedPodTemplateDigest {
		return id, fmt.Errorf("deployment %s/%s live pod template does not match reviewed rendered desired state: %s != %s",
			id.Namespace, id.Name, id.PodTemplateSHA256, reviewedPodTemplateDigest)
	}
	reviewedSelectorDigest, err := canonicalDeploymentSelectorSHA256(normalized.Spec.Selector)
	if err != nil {
		return id, fmt.Errorf("canonicalize reviewed Deployment %s/%s selector: %w",
			id.Namespace, id.Name, err)
	}
	if id.SelectorSHA256 != reviewedSelectorDigest {
		return id, fmt.Errorf("deployment %s/%s live selector does not match reviewed rendered desired state: %s != %s",
			id.Namespace, id.Name, id.SelectorSHA256, reviewedSelectorDigest)
	}
	id.ReviewedSpecSHA256 = reviewedDigest
	id.ReviewedPodTemplateSHA256 = reviewedPodTemplateDigest
	id.ReviewedSelectorSHA256 = reviewedSelectorDigest
	id.Review = reviewed.Review
	if id.Review.RenderedSpecSHA256 == "" {
		id.Review.RenderedSpecSHA256 = reviewed.RenderedSpecSHA256
	}
	return id, nil
}

func (h *Harness) normalizeReviewedDeployment(
	ctx context.Context,
	reviewed, live *appsv1.Deployment,
) (*appsv1.Deployment, error) {
	if h.normalizeDeploymentFn != nil {
		return h.normalizeDeploymentFn(ctx, reviewed.DeepCopy(), live.DeepCopy())
	}
	client, err := h.kubernetesClient()
	if err != nil {
		return nil, err
	}
	desired := reviewed.DeepCopy()
	desired.ResourceVersion = live.ResourceVersion
	desired.UID = live.UID
	desired.CreationTimestamp = metav1.Time{}
	desired.ManagedFields = nil
	desired.Generation = 0
	desired.Status = appsv1.DeploymentStatus{}
	normalized, err := client.AppsV1().Deployments(live.Namespace).Update(ctx, desired, metav1.UpdateOptions{
		DryRun:       []string{metav1.DryRunAll},
		FieldManager: "mills-s1c-deployment-provenance",
	})
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// ValidateDeploymentProvenance binds both serialized Deployment proofs to the
// exact reviewed Flux/source identities in the same preflight snapshot.
func ValidateDeploymentProvenance(report PreflightReport) error {
	sources := fluxProvenanceByName(report.FluxSourcesEnd)
	platform, ok := sources["apps"]
	if !ok {
		return errors.New("deployment provenance has no apps Flux owner")
	}
	checks := []struct {
		label, namespace, name, owner string
		deployment                    DeploymentIdentity
		source                        GitOpsScopeIdentity
	}{
		{"operator", "loom-mills", "loom-mills-operator", "apps", report.OperatorDeployment, platform.ProtectedIdentity},
		{"mobile-hud", "loom-hub", "mobile-hud", "loom-hub-servers", report.HudDeployment, sources["loom-hub-servers"].ProtectedIdentity},
	}
	for _, check := range checks {
		deployment := check.deployment
		if deployment.Name != check.name || deployment.Namespace != check.namespace ||
			strings.TrimSpace(deployment.UID) == "" || strings.TrimSpace(deployment.ResourceVersion) == "" ||
			strings.TrimSpace(deployment.ContainerName) == "" {
			return fmt.Errorf("%s Deployment object identity is incomplete or redirected: %+v", check.label, deployment)
		}
		if !isNormalizedSHA256(deployment.SpecSHA256) ||
			deployment.SpecSHA256 != deployment.ReviewedSpecSHA256 {
			return fmt.Errorf("%s Deployment live/reviewed full-spec SHA-256 mismatch: %q/%q",
				check.label, deployment.SpecSHA256, deployment.ReviewedSpecSHA256)
		}
		if !isNormalizedSHA256(deployment.PodTemplateSHA256) ||
			deployment.PodTemplateSHA256 != deployment.ReviewedPodTemplateSHA256 {
			return fmt.Errorf("%s Deployment live/reviewed pod-template SHA-256 mismatch: %q/%q",
				check.label, deployment.PodTemplateSHA256, deployment.ReviewedPodTemplateSHA256)
		}
		if !isNormalizedSHA256(deployment.SelectorSHA256) ||
			deployment.SelectorSHA256 != deployment.ReviewedSelectorSHA256 {
			return fmt.Errorf("%s Deployment live/reviewed selector SHA-256 mismatch: %q/%q",
				check.label, deployment.SelectorSHA256, deployment.ReviewedSelectorSHA256)
		}
		review := deployment.Review
		if review.Contract != DeploymentProvenanceContract ||
			review.ContractVersion != DeploymentProvenanceContractVersion ||
			review.FluxOwner != check.owner || !isNormalizedSHA256(review.FluxSpecSHA256) ||
			!isNormalizedSHA256(review.RenderedSpecSHA256) || strings.TrimSpace(review.Renderer) == "" ||
			strings.TrimSpace(review.RendererVersion) == "" {
			return fmt.Errorf("%s Deployment review identity is incomplete or unsupported: %+v", check.label, review)
		}
		owner, exists := sources[check.owner]
		if !exists || review.FluxSpecSHA256 != owner.RenderSpec.SpecSHA256 {
			return fmt.Errorf("%s Deployment review is not bound to Flux owner %s", check.label, check.owner)
		}
		if review.PlatformRevision != platform.ProtectedIdentity.BaselineRevision ||
			review.PlatformScopeDigest != platform.ProtectedIdentity.BaselineDigest {
			return fmt.Errorf("%s Deployment review is not bound to the platform baseline", check.label)
		}
		if review.SourceRevision != check.source.BaselineRevision ||
			review.SourceScopeDigest != check.source.BaselineDigest {
			return fmt.Errorf("%s Deployment review is not bound to its source baseline", check.label)
		}
	}
	if report.OperatorDeployment.Review.Renderer != report.HudDeployment.Review.Renderer ||
		report.OperatorDeployment.Review.RendererVersion != report.HudDeployment.Review.RendererVersion {
		return errors.New("operator and mobile-hud were not reconstructed by one renderer identity")
	}
	return nil
}

func sameDeploymentGateIdentity(before, after DeploymentIdentity) error {
	if before.Name != after.Name || before.Namespace != after.Namespace || before.UID != after.UID ||
		before.Generation != after.Generation || before.Image != after.Image ||
		before.ContainerName != after.ContainerName ||
		before.Strategy != after.Strategy || before.PolicyChecksum != after.PolicyChecksum ||
		before.SpecSHA256 != after.SpecSHA256 || before.ReviewedSpecSHA256 != after.ReviewedSpecSHA256 ||
		before.PodTemplateSHA256 != after.PodTemplateSHA256 ||
		before.ReviewedPodTemplateSHA256 != after.ReviewedPodTemplateSHA256 ||
		before.SelectorSHA256 != after.SelectorSHA256 ||
		before.ReviewedSelectorSHA256 != after.ReviewedSelectorSHA256 ||
		before.Review != after.Review {
		return fmt.Errorf("deployment stable reviewed identity changed: before=%+v after=%+v", before, after)
	}
	return nil
}

func sameLiveDeploymentIdentity(before, current DeploymentIdentity) error {
	if before.Name != current.Name || before.Namespace != current.Namespace || before.UID != current.UID ||
		before.Generation != current.Generation || before.Image != current.Image ||
		before.ContainerName != current.ContainerName ||
		before.Strategy != current.Strategy || before.PolicyChecksum != current.PolicyChecksum ||
		before.SpecSHA256 != current.SpecSHA256 ||
		before.PodTemplateSHA256 != current.PodTemplateSHA256 ||
		before.SelectorSHA256 != current.SelectorSHA256 {
		return fmt.Errorf("before=%+v current=%+v", before, current)
	}
	return nil
}
