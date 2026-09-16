package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
)

// Remote manifest fingerprinting for git-clone sandboxes.
//
// In git-clone sync mode the hub has no checkout of the project: the sandbox
// pod's init container clones it. detect.Fingerprint on the (absent) local
// directory therefore finds no languages and hashes nothing, which produced
// the "generic" sandbox image keyed by the empty-input hash (e3b0c44). Two
// properties of that image clogged the Mills tests stage:
//
//   - Its base image could only be chosen inside the build pod (after the
//     clone), so the registry could never be trusted as a cache: every cold
//     call — and mcp-devbox is a fresh process per hub websocket session —
//     rebuilt and re-pushed the same 450MB image (24–28 min under load,
//     2026-09-03/04: fourteen pushes of the identical tag in 22 hours).
//   - The quality gate waits at most 8 minutes for a build, so each rebuild
//     burned two or three "sandbox image still building" attempts before a
//     run could test anything (137 of 447 tests-stage attempts in the week to
//     2026-09-04, 22.7 hours of wall-clock).
//
// The fix asks the git host for the dependency manifests instead: the same
// files detect.Fingerprint reads locally, fetched from the project's default
// branch through the GitLab files API with the sandbox git token. The
// fingerprint is then real — a language, a version, a content hash — so the
// tag is immutable, the Dockerfile is the language template on the registered
// base image, and the existing registry-hit short-circuits apply. The image
// is rebuilt only when a manifest changes. Anything that fails (no token, an
// unreachable host, a repo with no manifests) degrades to the previous
// generic-image behavior, never to a hard error.

const (
	// remoteManifestCacheDir is the cacheDir subtree holding fetched
	// manifests, one directory per workspace-relative project path.
	remoteManifestCacheDir = "remote-manifests"
	// remoteManifestStamp records (RFC 3339) when a directory was fetched.
	remoteManifestStamp = ".fetched-at"
	// defaultRemoteManifestTTL bounds how long fetched manifests are reused
	// before the host is asked again. It must comfortably exceed the quality
	// gate's build wait (8m), whose poll loop re-fingerprints every 3–15s.
	defaultRemoteManifestTTL = 10 * time.Minute
	// maxRemoteManifestBytes caps one fetched file (lockfiles run to a few
	// MB; anything larger is not a manifest).
	maxRemoteManifestBytes = 8 << 20
	// gitTokenRetryInterval spaces retries of a failed git-token lookup so a
	// missing RBAC grant logs once a minute, not once per fingerprint.
	gitTokenRetryInterval = time.Minute
)

// manifestSource fetches a project's dependency manifests from the git host.
type manifestSource interface {
	// FetchManifests writes each of names that exists at the default branch
	// of the repository the sandbox would clone for the workspace-relative
	// repoPath ("services/loom-core") into dst, and returns the names written.
	// A repository with none of the files is an error, not an empty result.
	FetchManifests(ctx context.Context, repoPath string, names []string, dst string) ([]string, error)
}

// gitlabManifestSource reads manifests through the GitLab repository files
// API (GET /projects/:id/repository/files/:path/raw), authenticated with the
// same token the sandbox git-clone init container uses.
type gitlabManifestSource struct {
	gitBaseURL string
	ref        string
	client     *http.Client
	token      func(context.Context) string
}

func newGitLabManifestSource(gitBaseURL string, token func(context.Context) string) *gitlabManifestSource {
	return &gitlabManifestSource{
		gitBaseURL: gitBaseURL,
		ref:        "HEAD",
		client:     &http.Client{Timeout: 20 * time.Second},
		token:      token,
	}
}

func (s *gitlabManifestSource) FetchManifests(ctx context.Context, repoPath string, names []string, dst string) ([]string, error) {
	apiBase, project, ok := backend.RepoProjectPath(s.gitBaseURL, repoPath)
	if !ok {
		return nil, fmt.Errorf("git base url %q is not an absolute URL", s.gitBaseURL)
	}
	token := ""
	if s.token != nil {
		token = s.token(ctx)
	}
	var fetched []string
	for _, name := range names {
		found, err := s.fetchOne(ctx, apiBase, project, name, token, dst)
		if err != nil {
			return fetched, err
		}
		if found {
			fetched = append(fetched, name)
		}
	}
	if len(fetched) == 0 {
		return nil, fmt.Errorf("no dependency manifests found in %s@%s", project, s.ref)
	}
	return fetched, nil
}

// fetchOne downloads a single file. A 404 is a normal "this project has no
// such manifest" (false, nil); an auth failure is reported with the likely
// cause because it is the one error an operator can fix.
func (s *gitlabManifestSource) fetchOne(ctx context.Context, apiBase, project, name, token, dst string) (bool, error) {
	endpoint := apiBase + "/api/v4/projects/" + url.PathEscape(project) +
		"/repository/files/" + url.PathEscape(name) + "/raw?ref=" + url.QueryEscape(s.ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("fetch %s: %w", name, err)
	}
	if token != "" {
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("fetch %s/%s: %w", project, name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Errorf("gitlab files api: %s for %s/%s (git token missing, or it lacks read_api on the project)", resp.Status, project, name)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return false, fmt.Errorf("gitlab files api: %s for %s/%s", resp.Status, project, name)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteManifestBytes+1))
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", project, name, err)
	}
	if len(data) > maxRemoteManifestBytes {
		return false, fmt.Errorf("%s/%s exceeds %d bytes; not a dependency manifest", project, name, maxRemoteManifestBytes)
	}
	if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	return true, nil
}

// gitToken returns the git host token for API calls: DEVBOX_GIT_TOKEN when
// set, otherwise the "token" key of the git-clone secret read through the
// backend's SecretResolver (the same credential the sandbox init container
// mounts). Lookups are cached; a failed lookup is retried at most once a
// minute so a missing RBAC grant does not log on every poll.
func (m *manager) gitToken(ctx context.Context) string {
	if v := strings.TrimSpace(m.cfg.gitToken); v != "" {
		return v
	}
	m.gitTokenMu.Lock()
	defer m.gitTokenMu.Unlock()
	if m.gitTokenVal != "" || time.Since(m.gitTokenAt) < gitTokenRetryInterval {
		return m.gitTokenVal
	}
	m.gitTokenAt = time.Now()
	if m.backend == nil || strings.TrimSpace(m.cfg.gitSecret) == "" {
		return ""
	}
	resolver, ok := m.backend.(backend.SecretResolver)
	if !ok {
		return ""
	}
	values, err := resolver.ResolveSecretEnv(ctx, []backend.SecretEnvVar{{
		Name: "GIT_TOKEN", SecretName: m.cfg.gitSecret, SecretKey: "token",
	}})
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("git token unreadable by the devbox manager; remote manifest fetches run anonymously (grant get on the secret, or set DEVBOX_GIT_TOKEN)",
				"secret", m.cfg.gitSecret, "error", err)
		}
		return ""
	}
	m.gitTokenVal = strings.TrimSpace(values["GIT_TOKEN"])
	return m.gitTokenVal
}

// fingerprintProject is detect.Fingerprint plus the git-clone remote
// hydration: when the local directory yields no languages and a manifest
// source is configured, the fingerprint is computed from manifests fetched
// from the git host instead. Hydration failures fall back to the local
// (empty) fingerprint, which downstream code degrades to the generic image
// exactly as before.
func (m *manager) fingerprintProject(ctx context.Context, projectDir string) (*detect.EnvFingerprint, error) {
	fp, err := detect.Fingerprint(projectDir)
	if err != nil {
		return nil, err
	}
	if len(fp.Languages) > 0 || m.cfg.syncMode != "git-clone" || m.manifests == nil {
		return m.stampRecipe(fp), nil
	}
	hydrated, err := m.remoteFingerprint(ctx, projectDir)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("remote manifest fingerprint unavailable; falling back to the generic git-clone image",
				"project", fp.ProjectName, "dir", projectDir, "error", err)
		}
		return m.stampRecipe(fp), nil
	}
	return m.stampRecipe(hydrated), nil
}

// stampRecipe folds the Dockerfile this manager would build for fp into
// fp.Hash (see detect.RecipeHash), so the image tag names inputs AND recipe.
// The Dockerfile is a pure function of fp, so every caller — sandbox start,
// devbox_build, the quality gate — derives the same tag. A fingerprint whose
// Dockerfile cannot be generated keeps its raw hash; the build fails on that
// same error later, with the same message it always had.
func (m *manager) stampRecipe(fp *detect.EnvFingerprint) *detect.EnvFingerprint {
	df, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		return fp
	}
	fp.Hash = detect.RecipeHash(fp.Hash, df)
	return fp
}

// remoteFingerprint fingerprints the cached copy of the project's manifests,
// refreshing the copy from the git host when it is older than the TTL. A
// refresh failure keeps serving a stale copy when one exists: a manifest
// that is a few hours old still names the right toolchain, and the sandbox
// clones the real tree anyway.
func (m *manager) remoteFingerprint(ctx context.Context, projectDir string) (*detect.EnvFingerprint, error) {
	rel, err := filepath.Rel(m.cfg.workspaceRoot, projectDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("project %s is not under workspace root %s", projectDir, m.cfg.workspaceRoot)
	}
	repoPath := filepath.ToSlash(rel)
	dir := filepath.Join(m.cfg.cacheDir, remoteManifestCacheDir, rel)
	ttl := m.manifestTTL
	if ttl <= 0 {
		ttl = defaultRemoteManifestTTL
	}
	if !remoteManifestsFresh(dir, ttl) {
		if err := m.refreshRemoteManifests(ctx, repoPath, dir, ttl); err != nil {
			if _, statErr := os.Stat(filepath.Join(dir, remoteManifestStamp)); statErr != nil {
				return nil, err
			}
			if m.logger != nil {
				m.logger.Warn("remote manifest refresh failed; fingerprinting the cached copy", "project", repoPath, "error", err)
			}
		}
	}
	fp, err := detect.Fingerprint(dir)
	if err != nil {
		return nil, fmt.Errorf("fingerprint remote manifests: %w", err)
	}
	if len(fp.Languages) == 0 {
		return nil, fmt.Errorf("no languages detected in remote manifests for %s", repoPath)
	}
	// Downstream code keys everything (build context, work dir, state) on the
	// real project directory; only the detection inputs came from the cache.
	fp.ProjectDir = projectDir
	fp.ProjectName = filepath.Base(projectDir)
	return fp, nil
}

// refreshRemoteManifests fetches into a sibling temp directory and swaps it
// into place so a concurrent fingerprint never reads a half-written set.
func (m *manager) refreshRemoteManifests(ctx context.Context, repoPath, dir string, ttl time.Duration) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("prepare manifest cache: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(dir)+".fetch-*")
	if err != nil {
		return fmt.Errorf("prepare manifest cache: %w", err)
	}
	defer os.RemoveAll(tmp)

	fetched, err := m.manifests.FetchManifests(ctx, repoPath, detect.DependencyFiles(), tmp)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(filepath.Join(tmp, remoteManifestStamp), []byte(stamp), 0o600); err != nil {
		return fmt.Errorf("stamp manifest cache: %w", err)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		// A concurrent refresh for the same project may have landed first;
		// its copy is as good as ours.
		if remoteManifestsFresh(dir, ttl) {
			return nil
		}
		return fmt.Errorf("install fetched manifests: %w", err)
	}
	if m.logger != nil {
		m.logger.Info("fetched remote dependency manifests", "project", repoPath, "files", fetched)
	}
	return nil
}

// remoteManifestsFresh reports whether dir holds a manifest set fetched
// within ttl.
func remoteManifestsFresh(dir string, ttl time.Duration) bool {
	data, err := os.ReadFile(filepath.Join(dir, remoteManifestStamp))
	if err != nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	return time.Since(at) < ttl
}
