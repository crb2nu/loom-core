package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/devbox/detect"
)

const testGoMod = "module example.com/loom-core\n\ngo 1.26.0\n\nrequire example.com/dep v1.2.3\n"

// fakeManifestSource serves manifests from memory and counts fetches.
type fakeManifestSource struct {
	files     map[string]string
	err       error
	calls     int
	repoPaths []string
}

func (f *fakeManifestSource) FetchManifests(_ context.Context, repoPath string, names []string, dst string) ([]string, error) {
	f.calls++
	f.repoPaths = append(f.repoPaths, repoPath)
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, name := range names {
		content, ok := f.files[name]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(dst, name), []byte(content), 0o600); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New("no manifests")
	}
	return out, nil
}

func newRemoteFingerprintManager(t *testing.T, src manifestSource) (*manager, string) {
	t.Helper()
	root := t.TempDir()
	m := &manager{
		cfg: managerConfig{
			syncMode:      "git-clone",
			workspaceRoot: filepath.Join(root, "app"),
			cacheDir:      filepath.Join(root, "cache"),
			imagePrefix:   "mcp/devbox",
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		manifests: src,
	}
	return m, filepath.Join(root, "app", "services", "loom-core")
}

// localTwinHash fingerprints a local directory holding the same manifests so
// the test can assert the remote fingerprint hashes identically — the whole
// point is that hub and workstation converge on one image tag.
func localTwinHash(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "loom-core")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fp, err := detect.Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fp.Hash
}

func TestFingerprintProject_HydratesFromRemoteManifests(t *testing.T) {
	files := map[string]string{"go.mod": testGoMod, "go.sum": "example.com/dep v1.2.3 h1:abc=\n"}
	src := &fakeManifestSource{files: files}
	m, projectDir := newRemoteFingerprintManager(t, src)

	fp, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatalf("fingerprintProject: %v", err)
	}
	if len(fp.Languages) != 1 || fp.Languages[0].Language != "go" || fp.Languages[0].Version != "1.26.0" {
		t.Fatalf("languages = %+v, want go 1.26.0", fp.Languages)
	}
	if fp.ProjectDir != projectDir || fp.ProjectName != "loom-core" {
		t.Fatalf("project identity = (%q, %q), want (%q, loom-core)", fp.ProjectDir, fp.ProjectName, projectDir)
	}
	if want := recipeHashFor(t, m, files); fp.Hash != want {
		t.Fatalf("hash = %q, want local twin folded with the recipe %q", fp.Hash, want)
	}
	if got := src.repoPaths; len(got) != 1 || got[0] != "services/loom-core" {
		t.Fatalf("repo paths = %v, want [services/loom-core]", got)
	}
	if m.detectBaseImageAfterClone(fp) {
		t.Fatal("hydrated fingerprint must not need in-pod base image detection")
	}
	df, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		t.Fatalf("generateSandboxDockerfile: %v", err)
	}
	if !strings.Contains(string(df), "FROM registry.harbor.lan/mcp/devbox-base/go:1.26") {
		t.Fatalf("Dockerfile does not use the registered Go base:\n%s", df)
	}

	// Within the TTL the cached copy serves every poll without a fetch.
	if _, err := m.fingerprintProject(context.Background(), projectDir); err != nil {
		t.Fatalf("second fingerprintProject: %v", err)
	}
	if src.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (cache hit)", src.calls)
	}
}

// recipeHashFor is the tag hash a workstation checkout of the same manifests
// would produce with this manager's Dockerfile template: dependency hash
// folded with the rendered recipe.
func recipeHashFor(t *testing.T, m *manager, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "loom-core")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fp, err := detect.Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	df, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		t.Fatal(err)
	}
	return detect.RecipeHash(fp.Hash, df)
}

// TestFingerprintProject_TagCoversRecipeNotJustInputs pins the property that
// closed the 2026-09-05 regression: an image tag must change when the
// Dockerfile changes, even though go.mod did not — otherwise the registry
// hands back whatever an older manager built under the same dependency hash.
func TestFingerprintProject_TagCoversRecipeNotJustInputs(t *testing.T) {
	files := map[string]string{"go.mod": testGoMod}
	m, projectDir := newRemoteFingerprintManager(t, &fakeManifestSource{files: files})
	fp, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatal(err)
	}
	raw := localTwinHash(t, files)
	if fp.Hash == raw {
		t.Fatalf("hash %q is the bare dependency hash; the recipe was not folded in", fp.Hash)
	}
	// The fold uses the Dockerfile rendered for the RAW fingerprint (its
	// header line embeds the hash), which is what a workstation manager with
	// the same template renders for the same manifests.
	if want := recipeHashFor(t, m, files); fp.Hash != want {
		t.Fatalf("hash = %q, want RecipeHash(deps, dockerfile) = %q", fp.Hash, want)
	}
	if other := detect.RecipeHash(raw, []byte("FROM golang:1.26-alpine\nRUN go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest\n")); other == fp.Hash {
		t.Fatal("a different recipe produced the same hash")
	}
	// The generic fallback is stamped too, so it no longer collides with the
	// empty-input hash that predates recipe folding.
	generic, err := m.fingerprintProject(context.Background(), filepath.Join(m.cfg.workspaceRoot, "services", "unknown-repo"))
	if err != nil {
		t.Fatal(err)
	}
	if generic.Hash == "" || strings.HasPrefix(generic.Hash, "e3b0c44") {
		t.Fatalf("generic fallback hash = %q, want a recipe-stamped hash", generic.Hash)
	}
}

func TestFingerprintProject_RefetchesAfterTTL(t *testing.T) {
	src := &fakeManifestSource{files: map[string]string{"go.mod": testGoMod}}
	m, projectDir := newRemoteFingerprintManager(t, src)
	m.manifestTTL = time.Hour

	if _, err := m.fingerprintProject(context.Background(), projectDir); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(m.cfg.cacheDir, remoteManifestCacheDir, "services", "loom-core", remoteManifestStamp)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(stamp, []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.fingerprintProject(context.Background(), projectDir); err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Fatalf("fetch calls = %d, want 2 (expired stamp)", src.calls)
	}
}

func TestFingerprintProject_UsesStaleCacheWhenRefreshFails(t *testing.T) {
	src := &fakeManifestSource{files: map[string]string{"go.mod": testGoMod}}
	m, projectDir := newRemoteFingerprintManager(t, src)

	first, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(m.cfg.cacheDir, remoteManifestCacheDir, "services", "loom-core", remoteManifestStamp)
	if err := os.WriteFile(stamp, []byte("2000-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src.err = errors.New("gitlab down")

	second, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Languages) == 0 || second.Hash != first.Hash {
		t.Fatalf("stale cache not used: languages=%+v hash=%q want %q", second.Languages, second.Hash, first.Hash)
	}
}

func TestFingerprintProject_FallsBackToGenericWhenRemoteUnavailable(t *testing.T) {
	src := &fakeManifestSource{err: errors.New("401 Unauthorized")}
	m, projectDir := newRemoteFingerprintManager(t, src)

	fp, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatalf("fingerprintProject must degrade, got error: %v", err)
	}
	if len(fp.Languages) != 0 {
		t.Fatalf("languages = %+v, want none (generic fallback)", fp.Languages)
	}
	if !m.detectBaseImageAfterClone(fp) {
		t.Fatal("generic fallback must keep in-pod base image detection")
	}
	df, err := m.generateSandboxDockerfile(fp)
	if err != nil {
		t.Fatalf("generateSandboxDockerfile: %v", err)
	}
	if !strings.Contains(string(df), "FROM ${DEVBOX_BASE_IMAGE}") {
		t.Fatalf("expected the generic git-clone Dockerfile, got:\n%s", df)
	}
}

func TestFingerprintProject_LocalCheckoutWinsWithoutFetch(t *testing.T) {
	src := &fakeManifestSource{files: map[string]string{"go.mod": "module x\n\ngo 1.25\n"}}
	m, projectDir := newRemoteFingerprintManager(t, src)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte(testGoMod), 0o600); err != nil {
		t.Fatal(err)
	}

	fp, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 when the checkout is local", src.calls)
	}
	if len(fp.Languages) != 1 || fp.Languages[0].Version != "1.26.0" {
		t.Fatalf("languages = %+v, want the local go 1.26.0", fp.Languages)
	}
}

func TestFingerprintProject_NonGitCloneModeNeverFetches(t *testing.T) {
	src := &fakeManifestSource{files: map[string]string{"go.mod": testGoMod}}
	m, projectDir := newRemoteFingerprintManager(t, src)
	m.cfg.syncMode = "tar-pipe"

	fp, err := m.fingerprintProject(context.Background(), projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 0 || len(fp.Languages) != 0 {
		t.Fatalf("tar-pipe mode fetched remotely: calls=%d languages=%+v", src.calls, fp.Languages)
	}
}

// TestEnsureRunning_GitCloneBuildsContentKeyedImage is the end-to-end
// property behind the fix: a git-clone sandbox for a repo the hub has never
// seen locally builds an immutable, content-hashed tag from the registered
// base image, with no in-pod base detection — so the registry can be trusted
// as a cache on every later call.
func TestEnsureRunning_GitCloneBuildsContentKeyedImage(t *testing.T) {
	files := map[string]string{"go.mod": testGoMod}
	src := &fakeManifestSource{files: files}
	m, projectDir := newRemoteFingerprintManager(t, src)
	m.cfg.backendType = "docker" // synchronous build path
	fb := &fakeBackend{}
	m.backend = fb
	store, err := newTestStore(m.cfg.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	m.store = store

	id, err := m.ensureRunning(context.Background(), projectDir, "loom-core", "")
	if err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if id != "devbox-loom-core" {
		t.Fatalf("container id = %q", id)
	}
	if len(fb.buildOpts) != 1 {
		t.Fatalf("builds = %d, want 1", len(fb.buildOpts))
	}
	opts := fb.buildOpts[0]
	if opts.DetectBaseImage {
		t.Fatal("build requested in-pod base detection; the registry-hit short-circuit would be skipped")
	}
	wantTag := "mcp/devbox/loom-core:" + recipeHashFor(t, m, files)[:7]
	if opts.Tag != wantTag {
		t.Fatalf("tag = %q, want content-and-recipe-keyed %q", opts.Tag, wantTag)
	}
	if strings.HasSuffix(opts.Tag, ":e3b0c44") || strings.HasSuffix(opts.Tag, ":"+localTwinHash(t, files)[:7]) {
		t.Fatalf("tag %q ignores the recipe", opts.Tag)
	}
	if !strings.Contains(string(opts.Dockerfile), "FROM registry.harbor.lan/mcp/devbox-base/go:1.26") {
		t.Fatalf("Dockerfile:\n%s", opts.Dockerfile)
	}
	if opts.ContextDir != projectDir {
		t.Fatalf("build context = %q, want the project dir %q", opts.ContextDir, projectDir)
	}
	entry := store.Get("loom-core")
	if entry == nil || entry.ImageTag != wantTag || entry.ProjectDir != projectDir {
		t.Fatalf("state entry = %+v", entry)
	}
}

func TestGitLabManifestSource_FetchManifests(t *testing.T) {
	var gotPaths []string
	var gotTokens []string
	var gotRefs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		gotTokens = append(gotTokens, r.Header.Get("PRIVATE-TOKEN"))
		gotRefs = append(gotRefs, r.URL.Query().Get("ref"))
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/services%2Floom-core/repository/files/go.mod/raw":
			_, _ = io.WriteString(w, testGoMod)
		case "/api/v4/projects/services%2Floom-core/repository/files/.devbox.yaml/raw":
			_, _ = io.WriteString(w, "system_deps: [make]\n")
		default:
			http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	src := newGitLabManifestSource(srv.URL, func(context.Context) string { return "glpat-test" })
	dst := t.TempDir()
	fetched, err := src.FetchManifests(context.Background(), "services/loom-core", detect.DependencyFiles(), dst)
	if err != nil {
		t.Fatalf("FetchManifests: %v", err)
	}
	if strings.Join(fetched, ",") != "go.mod,.devbox.yaml" {
		t.Fatalf("fetched = %v", fetched)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "go.mod")); err != nil || string(data) != testGoMod {
		t.Fatalf("go.mod = %q, %v", data, err)
	}
	if len(gotPaths) != len(detect.DependencyFiles()) {
		t.Fatalf("requests = %d, want one per manifest name", len(gotPaths))
	}
	for i := range gotPaths {
		if gotTokens[i] != "glpat-test" || gotRefs[i] != "HEAD" {
			t.Fatalf("request %d: token=%q ref=%q", i, gotTokens[i], gotRefs[i])
		}
		if !strings.HasPrefix(gotPaths[i], "/api/v4/projects/services%2Floom-core/repository/files/") {
			t.Fatalf("request %d path = %q", i, gotPaths[i])
		}
	}

	// The fingerprint of the fetched set is a real Go fingerprint with the
	// manifest override applied.
	fp, err := detect.Fingerprint(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Languages) != 1 || fp.Languages[0].Language != "go" {
		t.Fatalf("languages = %+v", fp.Languages)
	}
	if fp.Overrides == nil || len(fp.Overrides.SystemDeps) != 1 {
		t.Fatalf("overrides = %+v, want .devbox.yaml honored", fp.Overrides)
	}
}

func TestGitLabManifestSource_LegacyGroupBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if strings.HasSuffix(r.URL.EscapedPath(), "/go.mod/raw") {
			_, _ = io.WriteString(w, testGoMod)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := newGitLabManifestSource(srv.URL+"/services", nil)
	if _, err := src.FetchManifests(context.Background(), "services/loom-core", []string{"go.mod"}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/services%2Floom-core/repository/files/go.mod/raw" {
		t.Fatalf("path = %q (boundary segment must dedup like the clone URL)", gotPath)
	}
}

func TestGitLabManifestSource_AuthFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	src := newGitLabManifestSource(srv.URL, nil)
	_, err := src.FetchManifests(context.Background(), "services/loom-core", []string{"go.mod"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "git token") {
		t.Fatalf("err = %v, want an actionable 401", err)
	}
}

func TestGitLabManifestSource_NoManifestsIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := newGitLabManifestSource(srv.URL, nil)
	_, err := src.FetchManifests(context.Background(), "services/empty", []string{"go.mod", "package.json"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no dependency manifests") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitLabManifestSource_RejectsRelativeBaseURL(t *testing.T) {
	src := newGitLabManifestSource("gitlab.internal/services", nil)
	if _, err := src.FetchManifests(context.Background(), "services/loom-core", []string{"go.mod"}, t.TempDir()); err == nil {
		t.Fatal("expected an error for a base URL without a scheme")
	}
}

// fakeSecretBackend is a fakeBackend that also resolves secrets, the way the
// K8s backend does.
type fakeSecretBackend struct {
	fakeBackend
	values map[string]string
	err    error
	calls  int
}

func (f *fakeSecretBackend) ResolveSecretEnv(_ context.Context, secrets []backend.SecretEnvVar) (map[string]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, s := range secrets {
		if v, ok := f.values[s.SecretName+"/"+s.SecretKey]; ok {
			out[s.Name] = v
		}
	}
	return out, nil
}

func (f *fakeSecretBackend) ResolveSecretMounts(context.Context, []backend.SecretMount) ([]backend.ResolvedSecretFile, error) {
	return nil, nil
}

func TestGitToken_EnvOverridesSecret(t *testing.T) {
	fb := &fakeSecretBackend{values: map[string]string{"gitlab-creds/token": "from-secret"}}
	m := &manager{cfg: managerConfig{gitToken: " from-env ", gitSecret: "gitlab-creds"}, backend: fb}
	if got := m.gitToken(context.Background()); got != "from-env" {
		t.Fatalf("token = %q", got)
	}
	if fb.calls != 0 {
		t.Fatal("env token must not touch the secret")
	}
}

func TestGitToken_ReadsSecretOnceAndCaches(t *testing.T) {
	fb := &fakeSecretBackend{values: map[string]string{"gitlab-creds/token": "glpat-secret\n"}}
	m := &manager{cfg: managerConfig{gitSecret: "gitlab-creds"}, backend: fb}
	for i := 0; i < 3; i++ {
		if got := m.gitToken(context.Background()); got != "glpat-secret" {
			t.Fatalf("call %d token = %q", i, got)
		}
	}
	if fb.calls != 1 {
		t.Fatalf("secret reads = %d, want 1", fb.calls)
	}
}

func TestGitToken_LookupFailureIsRateLimitedAndEmpty(t *testing.T) {
	fb := &fakeSecretBackend{err: errors.New("secrets \"gitlab-creds\" is forbidden")}
	m := &manager{
		cfg:     managerConfig{gitSecret: "gitlab-creds"},
		backend: fb,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for i := 0; i < 3; i++ {
		if got := m.gitToken(context.Background()); got != "" {
			t.Fatalf("call %d token = %q, want empty", i, got)
		}
	}
	if fb.calls != 1 {
		t.Fatalf("secret reads = %d, want 1 within the retry interval", fb.calls)
	}
}

func TestGitToken_NoResolverBackend(t *testing.T) {
	m := &manager{cfg: managerConfig{gitSecret: "gitlab-creds"}, backend: &fakeBackend{}}
	if got := m.gitToken(context.Background()); got != "" {
		t.Fatalf("token = %q, want empty without a SecretResolver", got)
	}
}
