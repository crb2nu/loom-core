package killtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

const maxProtectedGitCommandOutputSize = int64(32 << 20)

const (
	// GitOpsIdentityModeExactRevision requires the reviewed and observed Git
	// commits to be identical. It preserves the original S1c gate contract.
	GitOpsIdentityModeExactRevision = "exact-revision"
	// GitOpsIdentityModeProtectedScope permits descendant commits when every
	// Git object in the versioned S1c protected scope remains identical.
	GitOpsIdentityModeProtectedScope = "protected-scope"

	gitOpsProtectedScopeV1Prefix   = "mills-s1c-gitops-scope/v1\x00"
	loomCoreProtectedScopeV1Prefix = "mills-s1c-loom-core-scope/v1\x00"
	maxGitScopeAncestryCommits     = 512
)

// gitOpsProtectedScopeV1Pathspecs is the source-impact closure for the S1c
// operator, mobile-HUD, and Kubernetes spawn substrate. Keep this list
// versioned with gitOpsProtectedScopeV1Prefix: changing its meaning without a
// version bump would make evidence digests from different contracts appear
// comparable.
var gitOpsProtectedScopeV1Pathspecs = []string{
	// k3s/flux/system renders resources from each of these directories. Protect
	// the directory roots so newly added transitive inputs cannot bypass S1c.
	"clusters/k3s/flux-system",
	"k3s/system",
	"k3s/net",
	"k3s/coredns",
	"k3s/kube-vip",
	"k3s/flux/system",
	"k3s/flux/bootstrap/kustomization.yaml",
	"k3s/flux/apps/kustomization.yaml",
	"k3s/flux/apps/services/kustomization.yaml",
	// PASS-1's durable dedupe proof is read through Loki. Bind both the Flux
	// render root and the logging source tree so an accepted descendant cannot
	// silently change the evidence pipeline.
	"k3s/flux/apps/logging",
	"k3s/logging",
	"k3s/mills",
	"k3s/loom-hub",
	"k3s/devbox",
	"k3s/security-posture",
	"k3s/flux/image-automation/kustomization.yaml",
	"k3s/flux/image-automation/loom-core-*",
	"k3s/flux/image-automation/loom-mills-operator-*",
}

var loomCoreProtectedScopeV1Pathspecs = []string{
	"k8s/base",
}

type gitScopeContract struct {
	name      string
	version   int
	subject   string
	prefix    string
	pathspecs []string
}

var (
	platformGitOpsScopeV1 = gitScopeContract{
		name:      "platform-gitops",
		version:   1,
		subject:   "GitOps",
		prefix:    gitOpsProtectedScopeV1Prefix,
		pathspecs: gitOpsProtectedScopeV1Pathspecs,
	}
	loomCoreSourceScopeV1 = gitScopeContract{
		name:      "loom-core-source",
		version:   1,
		subject:   "loom-core source",
		prefix:    loomCoreProtectedScopeV1Prefix,
		pathspecs: loomCoreProtectedScopeV1Pathspecs,
	}
)

// GitOpsScopeIdentity records both the source revisions observed by the gate
// and the identity used to compare them. In exact-revision mode the digest is
// the normalized commit ID. In protected-scope mode it is the SHA-256 of the
// versioned, sorted Git tree records for the protected path set.
type GitOpsScopeIdentity struct {
	Mode               string `json:"mode"`
	Contract           string `json:"contract"`
	ContractVersion    int    `json:"contract_version"`
	BaselineRevision   string `json:"baseline_revision"`
	ObservedRevision   string `json:"observed_revision"`
	BaselineDigest     string `json:"baseline_digest"`
	ObservedDigest     string `json:"observed_digest"`
	CheckedCommitCount int    `json:"checked_commit_count"`
}

// ResolveGitOpsScopeIdentity proves that observed is either the exact reviewed
// baseline or a descendant whose protected Git objects remain unchanged at
// every commit on the ancestry path. Missing commits trigger one bounded
// `git fetch origin main`; every other operation is local and read-only.
func ResolveGitOpsScopeIdentity(
	ctx context.Context,
	repoPath, baseline, observed, mode string,
) (GitOpsScopeIdentity, error) {
	return resolveGitScopeIdentity(ctx, repoPath, baseline, observed, mode, platformGitOpsScopeV1)
}

// ResolveLoomCoreScopeIdentity applies the same exact-revision or protected-
// scope proof to the loom-core source consumed by Flux. Its v1 contract covers
// k8s/base, the full render root for loom-hub-servers.
func ResolveLoomCoreScopeIdentity(
	ctx context.Context,
	repoPath, baseline, observed, mode string,
) (GitOpsScopeIdentity, error) {
	return resolveGitScopeIdentity(ctx, repoPath, baseline, observed, mode, loomCoreSourceScopeV1)
}

func resolveGitScopeIdentity(
	ctx context.Context,
	repoPath, baseline, observed, mode string,
	contract gitScopeContract,
) (GitOpsScopeIdentity, error) {
	identity := GitOpsScopeIdentity{
		Mode:            mode,
		Contract:        contract.name,
		ContractVersion: contract.version,
	}
	if mode != GitOpsIdentityModeExactRevision && mode != GitOpsIdentityModeProtectedScope {
		return identity, fmt.Errorf("unsupported %s identity mode %q (want %q or %q)",
			contract.subject, mode, GitOpsIdentityModeExactRevision, GitOpsIdentityModeProtectedScope)
	}
	if strings.TrimSpace(repoPath) == "" {
		return identity, fmt.Errorf("%s repository path is required", contract.subject)
	}

	baselineRevision, err := normalizeGitOpsScopeRevision(baseline)
	if err != nil {
		return identity, fmt.Errorf("normalize baseline %s revision: %w", contract.subject, err)
	}
	observedRevision, err := normalizeGitOpsScopeRevision(observed)
	if err != nil {
		return identity, fmt.Errorf("normalize observed %s revision: %w", contract.subject, err)
	}
	identity.BaselineRevision = baselineRevision
	identity.ObservedRevision = observedRevision

	if _, err := runGitScope(ctx, repoPath, "rev-parse", "--git-dir"); err != nil {
		return identity, fmt.Errorf("validate %s repository: %w", contract.subject, err)
	}
	if err := ensureGitScopeCommits(ctx, repoPath, contract.subject, baselineRevision, observedRevision); err != nil {
		return identity, err
	}
	if err := requireGitScopeAncestor(ctx, repoPath, baselineRevision, observedRevision, contract.subject); err != nil {
		return identity, err
	}

	if mode == GitOpsIdentityModeExactRevision {
		identity.BaselineDigest = baselineRevision
		identity.ObservedDigest = observedRevision
		identity.CheckedCommitCount = 1
		if baselineRevision != observedRevision {
			return identity, fmt.Errorf("exact %s revision changed: %s -> %s", contract.subject, baselineRevision, observedRevision)
		}
		return identity, nil
	}

	if err := requireProtectedScopeBaseline(ctx, repoPath, baselineRevision, contract); err != nil {
		return identity, err
	}
	identity.BaselineDigest, err = protectedGitScopeDigest(ctx, repoPath, baselineRevision, contract)
	if err != nil {
		return identity, fmt.Errorf("compute baseline %s protected-scope digest: %w", contract.subject, err)
	}
	identity.ObservedDigest, err = protectedGitScopeDigest(ctx, repoPath, observedRevision, contract)
	if err != nil {
		return identity, fmt.Errorf("compute observed %s protected-scope digest: %w", contract.subject, err)
	}
	identity.CheckedCommitCount = 1

	ancestry, err := gitScopeAncestryRevisions(ctx, repoPath, baselineRevision, observedRevision)
	if err != nil {
		return identity, fmt.Errorf("enumerate %s protected-scope ancestry: %w", contract.subject, err)
	}
	if len(ancestry) > maxGitScopeAncestryCommits {
		return identity, fmt.Errorf("%s protected-scope ancestry has more than %d commits between %s and %s",
			contract.subject, maxGitScopeAncestryCommits, baselineRevision, observedRevision)
	}
	for _, revision := range ancestry {
		digest := identity.ObservedDigest
		if revision != observedRevision {
			digest, err = protectedGitScopeDigest(ctx, repoPath, revision, contract)
			if err != nil {
				return identity, fmt.Errorf("compute %s protected-scope digest at %s: %w", contract.subject, revision, err)
			}
		}
		identity.CheckedCommitCount++
		if digest != identity.BaselineDigest {
			return identity, fmt.Errorf("%s protected scope changed at ancestry revision %s between %s and %s: %s -> %s",
				contract.subject, revision, baselineRevision, observedRevision, identity.BaselineDigest, digest)
		}
	}
	return identity, nil
}

func normalizeGitOpsScopeRevision(raw string) (string, error) {
	revision := strings.TrimSpace(raw)
	if index := strings.LastIndex(revision, ":"); index >= 0 {
		revision = revision[index+1:]
	}
	if len(revision) != 40 && len(revision) != 64 {
		return "", fmt.Errorf("revision must be a 40- or 64-character commit SHA, got %q", revision)
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return "", fmt.Errorf("revision is not hexadecimal: %w", err)
	}
	return strings.ToLower(revision), nil
}

func ensureGitScopeCommits(ctx context.Context, repoPath, subject string, revisions ...string) error {
	missing := make([]string, 0, len(revisions))
	seen := make(map[string]struct{}, len(revisions))
	for _, revision := range revisions {
		if _, ok := seen[revision]; ok {
			continue
		}
		seen[revision] = struct{}{}
		exists, err := gitScopeCommitExists(ctx, repoPath, revision)
		if err != nil {
			return fmt.Errorf("verify %s commit %s: %w", subject, revision, err)
		}
		if !exists {
			missing = append(missing, revision)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := runGitScope(ctx, repoPath, "fetch", "--quiet", "--no-tags", "origin", "main"); err != nil {
		return fmt.Errorf("fetch origin main for missing %s commits %s: %w", subject, strings.Join(missing, ", "), err)
	}
	for _, revision := range missing {
		exists, err := gitScopeCommitExists(ctx, repoPath, revision)
		if err != nil {
			return fmt.Errorf("verify fetched %s commit %s: %w", subject, revision, err)
		}
		if !exists {
			return fmt.Errorf("%s commit %s is missing after fetching origin main", subject, revision)
		}
	}
	return nil
}

func gitScopeCommitExists(ctx context.Context, repoPath, revision string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "-e", revision+"^{commit}")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

func requireGitScopeAncestor(ctx context.Context, repoPath, baseline, observed, subject string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "merge-base", "--is-ancestor", baseline, observed)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return fmt.Errorf("baseline %s revision %s is not an ancestor of observed revision %s", subject, baseline, observed)
	}
	return fmt.Errorf("verify %s revision ancestry: %w", subject, err)
}

func requireProtectedScopeBaseline(
	ctx context.Context,
	repoPath, baseline string,
	contract gitScopeContract,
) error {
	for _, pathspec := range contract.pathspecs {
		records, err := protectedGitScopeRecords(ctx, repoPath, baseline, pathspec)
		if err != nil {
			return fmt.Errorf("inspect protected baseline pathspec %q: %w", pathspec, err)
		}
		if len(records) == 0 {
			return fmt.Errorf("protected baseline pathspec %q matched no Git objects at %s", pathspec, baseline)
		}
	}
	return nil
}

func protectedGitScopeDigest(
	ctx context.Context,
	repoPath, revision string,
	contract gitScopeContract,
) (string, error) {
	var records [][]byte
	for _, pathspec := range contract.pathspecs {
		matched, err := protectedGitScopeRecords(ctx, repoPath, revision, pathspec)
		if err != nil {
			return "", err
		}
		records = append(records, matched...)
	}
	sort.Slice(records, func(i, j int) bool {
		return bytes.Compare(records[i], records[j]) < 0
	})

	hash := sha256.New()
	_, _ = hash.Write([]byte(contract.prefix))
	var previous []byte
	for _, record := range records {
		if previous != nil && bytes.Equal(previous, record) {
			continue
		}
		_, _ = hash.Write(record)
		_, _ = hash.Write([]byte{0})
		previous = record
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func gitScopeAncestryRevisions(
	ctx context.Context,
	repoPath, baseline, observed string,
) ([]string, error) {
	if baseline == observed {
		return nil, nil
	}
	output, err := runGitScope(ctx, repoPath,
		"rev-list", "--ancestry-path", "--topo-order", "--reverse",
		fmt.Sprintf("--max-count=%d", maxGitScopeAncestryCommits+1),
		baseline+".."+observed)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(string(output))
	if len(lines) <= maxGitScopeAncestryCommits {
		foundObserved := false
		for _, revision := range lines {
			if revision == observed {
				foundObserved = true
				break
			}
		}
		if !foundObserved {
			return nil, fmt.Errorf("observed revision %s is absent from ancestry path", observed)
		}
	}
	return lines, nil
}

// protectedGitScopeRecords expands the two trailing-star selectors without
// relying on pathspec magic, which `git ls-tree` intentionally does not
// support. Exact files and directories are delegated directly to ls-tree.
func protectedGitScopeRecords(
	ctx context.Context,
	repoPath, revision, pathspec string,
) ([][]byte, error) {
	query := pathspec
	prefix := ""
	if strings.HasSuffix(pathspec, "*") {
		prefix = strings.TrimSuffix(pathspec, "*")
		separator := strings.LastIndex(prefix, "/")
		if separator < 0 {
			return nil, fmt.Errorf("protected prefix pathspec %q has no parent directory", pathspec)
		}
		query = prefix[:separator]
	}

	output, err := runGitScope(ctx, repoPath, "ls-tree", "-r", "-z", "--full-tree", revision, "--", query)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(output, []byte{0})
	records := make([][]byte, 0, len(parts))
	for _, record := range parts {
		if len(record) == 0 {
			continue
		}
		if prefix != "" {
			tab := bytes.IndexByte(record, '\t')
			if tab < 0 || !strings.HasPrefix(string(record[tab+1:]), prefix) {
				continue
			}
		}
		records = append(records, record)
	}
	return records, nil
}

func runGitScope(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	gitArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	return runBoundedCommandBytes(cmd, "git "+strings.Join(args, " "),
		maxProtectedGitCommandOutputSize, maxReviewedCommandStderrSize)
}
