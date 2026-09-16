package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeDeps struct{ admin bool }

func (fakeDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (fakeDeps) WriteError(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		msg += ": " + err.Error()
	}
	http.Error(w, msg, status)
}
func (d fakeDeps) RequireAdminToken(w http.ResponseWriter, _ *http.Request) bool {
	if !d.admin {
		http.Error(w, "admin required", http.StatusUnauthorized)
	}
	return d.admin
}
func (fakeDeps) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeForge serves a tiny GitLab + GitHub API on one httptest server, keyed
// by path prefix, and records mutations.
type fakeForge struct {
	mu       sync.Mutex
	created  []map[string]any
	commits  []map[string]any
	mirrors  []map[string]any
	ghRepos  []map[string]any
	existing map[string]bool // gitlab project paths that exist
}

func (f *fakeForge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	// --- GitLab ---
	mux.HandleFunc("GET /gl/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "gl-tok" {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"username":"root"}`))
	})
	mux.HandleFunc("GET /gl/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("search") == "nothing" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":47,"path_with_namespace":"services/loom-core","name":"loom-core","web_url":"https://gl/services/loom-core","visibility":"private","default_branch":"main","last_activity_at":"2026-09-04T00:00:00Z"}]`))
	})
	mux.HandleFunc("GET /gl/projects/{path}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := f.existing[r.PathValue("path")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"404 Project Not Found"}`, 404)
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"path_with_namespace":"` + r.PathValue("path") + `","web_url":"https://gl/x","default_branch":"main","import_status":"finished"}`))
	})
	mux.HandleFunc("GET /gl/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":3,"full_path":"services"},{"id":30,"full_path":"services-archive"}]`))
	})
	mux.HandleFunc("GET /gl/groups", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":3,"full_path":"services","name":"services"},{"id":4,"full_path":"libs","name":"libs"}]`))
	})
	mux.HandleFunc("POST /gl/projects", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.created = append(f.created, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":99,"path_with_namespace":"services/` + body["path"].(string) + `","web_url":"https://gl/services/` + body["path"].(string) + `","default_branch":"main","import_status":"finished"}`))
	})
	mux.HandleFunc("POST /gl/projects/99/repository/commits", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.commits = append(f.commits, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"seed123"}`))
	})
	mux.HandleFunc("GET /gl/projects/99/remote_mirrors", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("POST /gl/projects/99/remote_mirrors", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.mirrors = append(f.mirrors, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":7,"enabled":true}`))
	})
	// --- GitHub ---
	mux.HandleFunc("GET /gh/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-tok" {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"crb2nu"}`))
	})
	mux.HandleFunc("GET /gh/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"full_name":"crb2nu/loom-core","name":"loom-core","html_url":"https://github.com/crb2nu/loom-core","private":false,"default_branch":"main","pushed_at":"2026-09-04T00:00:00Z"},{"id":2,"full_name":"crb2nu/other","name":"other","html_url":"https://github.com/crb2nu/other","private":true}]`))
	})
	mux.HandleFunc("GET /gh/repos/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "existing" {
			_, _ = w.Write([]byte(`{"full_name":"crb2nu/existing","html_url":"https://github.com/crb2nu/existing","private":true,"default_branch":"main","description":"old"}`))
			return
		}
		http.Error(w, `{"message":"Not Found"}`, 404)
	})
	mux.HandleFunc("POST /gh/user/repos", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.ghRepos = append(f.ghRepos, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"full_name":"crb2nu/` + body["name"].(string) + `","html_url":"https://github.com/crb2nu/` + body["name"].(string) + `"}`))
	})
	return mux
}

func newTestDomain(t *testing.T, admin bool, glTok, ghTok string) (*Domain, *fakeForge) {
	t.Helper()
	f := &fakeForge{existing: map[string]bool{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"GITLAB_API_URL": srv.URL + "/gl",
		"GITHUB_API_URL": srv.URL + "/gh",
		"GITLAB_TOKEN":   glTok,
		"GITHUB_TOKEN":   ghTok,
	}
	r := &Resolver{
		Getenv:   func(k string) string { return env[k] },
		LookPath: func(string) (string, error) { return "", errors.New("no cli") },
		RunCLI:   func(context.Context, string, ...string) (string, error) { return "", errors.New("no cli") },
		HTTP:     srv.Client(),
		cache:    map[Provider]cachedCred{},
	}
	return NewWithResolver(fakeDeps{admin: admin}, r), f
}

func do(t *testing.T, d *Domain, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	rec := httptest.NewRecorder()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, rdr))
	return rec
}

func TestAuth_ReportsBothProvidersWithoutLeakingTokens(t *testing.T) {
	d, _ := newTestDomain(t, false, "gl-tok", "")
	rec := do(t, d, http.MethodGet, "/api/forge/auth", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gl-tok") {
		t.Fatal("token leaked")
	}
	var out struct {
		Providers map[string]Status `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	gl, gh := out.Providers["gitlab"], out.Providers["github"]
	if !gl.Configured || gl.Source != "env" || gl.User != "root" || gl.ConnectHint != "" {
		t.Fatalf("gitlab = %+v", gl)
	}
	if gh.Configured || gh.ConnectHint == "" || !strings.Contains(gh.ConnectHint, "gh auth login") {
		t.Fatalf("github = %+v", gh)
	}
}

func TestAuth_CLITokenSourceIsUsedWhenEnvIsEmpty(t *testing.T) {
	d, _ := newTestDomain(t, false, "", "")
	d.resolver.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	d.resolver.RunCLI = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "gh" && strings.Join(args, " ") == "auth token" {
			return "gh-tok\n", nil
		}
		return "", errors.New("nope")
	}
	rec := do(t, d, http.MethodGet, "/api/forge/auth", "")
	var out struct {
		Providers map[string]Status `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if gh := out.Providers["github"]; !gh.Configured || gh.Source != "gh" || gh.User != "crb2nu" {
		t.Fatalf("github via gh = %+v", gh)
	}
	if gl := out.Providers["gitlab"]; gl.Configured || !gl.CLIPresent {
		t.Fatalf("gitlab = %+v", gl)
	}
}

func TestRepos_ListsBothProvidersAndFiltersGitHubClientSide(t *testing.T) {
	d, _ := newTestDomain(t, false, "gl-tok", "gh-tok")
	rec := do(t, d, http.MethodGet, "/api/forge/repos?provider=gitlab&q=loom", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"path":"services/loom-core"`) {
		t.Fatalf("gitlab repos: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, d, http.MethodGet, "/api/forge/repos?provider=github&q=other", "")
	var out struct {
		Repos []Repo `json:"repos"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Repos) != 1 || out.Repos[0].Path != "crb2nu/other" || out.Repos[0].Visibility != "private" {
		t.Fatalf("github repos = %+v", out.Repos)
	}
	if rec := do(t, d, http.MethodGet, "/api/forge/repos?provider=bitbucket", ""); rec.Code != 400 {
		t.Fatalf("bad provider → %d", rec.Code)
	}
	// Unconnected provider → 503 with the connect hint.
	d2, _ := newTestDomain(t, false, "gl-tok", "")
	if rec := do(t, d2, http.MethodGet, "/api/forge/repos?provider=github", ""); rec.Code != 503 || !strings.Contains(rec.Body.String(), "connect_hint") {
		t.Fatalf("unconnected → %d %s", rec.Code, rec.Body.String())
	}
}

func TestGroups(t *testing.T) {
	d, _ := newTestDomain(t, false, "gl-tok", "")
	rec := do(t, d, http.MethodGet, "/api/forge/groups", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"full_path":"libs"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateProject_DryRunThenRealWithMirror(t *testing.T) {
	d, f := newTestDomain(t, true, "gl-tok", "gh-tok")
	body := `{"name":"newthing","group":"services","description":"A thing","visibility":"private","template":"go","mirror_to_github":true,"dry_run":true}`
	rec := do(t, d, http.MethodPost, "/api/forge/projects", body)
	if rec.Code != 200 {
		t.Fatalf("dry-run: %d %s", rec.Code, rec.Body.String())
	}
	var out ProjectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.DryRun || len(f.created) != 0 || len(out.Steps) < 4 || out.Steps[len(out.Steps)-1].Step != "push_mirror" || out.Steps[len(out.Steps)-1].Status != "planned" {
		t.Fatalf("dry-run result = %+v (created=%d)", out, len(f.created))
	}

	rec = do(t, d, http.MethodPost, "/api/forge/projects", strings.Replace(body, `"dry_run":true`, `"dry_run":false`, 1))
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Project != "services/newthing" || out.SeedCommit != "seed123" || out.GitHub == nil || out.GitHub.Repo != "crb2nu/newthing" || out.GitHub.MirrorID != 7 {
		t.Fatalf("create result = %+v", out)
	}
	if len(f.created) != 1 || f.created[0]["namespace_id"].(float64) != 3 || f.created[0]["visibility"] != "private" {
		t.Fatalf("gitlab create payload = %+v", f.created)
	}
	acts := f.commits[0]["actions"].([]any)
	if len(acts) != 10 { // 7 generic + go.mod + main.go + Makefile
		t.Fatalf("seed actions = %d", len(acts))
	}
	if len(f.ghRepos) != 1 || f.ghRepos[0]["private"] != true {
		t.Fatalf("github create payload = %+v", f.ghRepos)
	}
	mirrorURL := f.mirrors[0]["url"].(string)
	if !strings.Contains(mirrorURL, "gh-tok@github.com/crb2nu/newthing.git") {
		t.Fatalf("mirror url = %s", mirrorURL)
	}
	if strings.Contains(rec.Body.String(), "gh-tok") {
		t.Fatal("mirror token leaked into the response")
	}
}

func TestCreateProject_ConflictAndGates(t *testing.T) {
	d, f := newTestDomain(t, true, "gl-tok", "")
	f.existing["services/taken"] = true
	rec := do(t, d, http.MethodPost, "/api/forge/projects", `{"name":"taken","group":"services"}`)
	if rec.Code != 409 || len(f.created) != 0 {
		t.Fatalf("conflict → %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, d, http.MethodPost, "/api/forge/projects", `{"name":"Bad Name","group":"services"}`); rec.Code != 400 {
		t.Fatalf("bad name → %d", rec.Code)
	}
	if rec := do(t, d, http.MethodPost, "/api/forge/projects", `{"name":"x","group":"services","mirror_to_github":true}`); rec.Code != 503 {
		t.Fatalf("mirror without github → %d %s", rec.Code, rec.Body.String())
	}
	noAdmin, _ := newTestDomain(t, false, "gl-tok", "")
	if rec := do(t, noAdmin, http.MethodPost, "/api/forge/projects", `{"name":"x","group":"services"}`); rec.Code != 401 {
		t.Fatalf("no admin → %d", rec.Code)
	}
}

func TestImportProject_ImportsAndMirrorsBack(t *testing.T) {
	d, f := newTestDomain(t, true, "gl-tok", "gh-tok")
	rec := do(t, d, http.MethodPost, "/api/forge/projects/import", `{"github_repo":"crb2nu/existing","group":"services","mirror_back":true}`)
	if rec.Code != 201 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var out ProjectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Project != "services/existing" || out.GitHub == nil || out.GitHub.MirrorID != 7 {
		t.Fatalf("import result = %+v", out)
	}
	if u, _ := f.created[0]["import_url"].(string); !strings.Contains(u, "gh-tok@github.com/crb2nu/existing.git") {
		t.Fatalf("import_url = %q", u)
	}
	if f.created[0]["visibility"] != "private" { // source is private → private
		t.Fatalf("visibility = %v", f.created[0]["visibility"])
	}
	if rec := do(t, d, http.MethodPost, "/api/forge/projects/import", `{"github_repo":"crb2nu/missing","group":"services"}`); rec.Code != 404 {
		t.Fatalf("missing source → %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeedActions_GenericAndGo(t *testing.T) {
	g := SeedActions("libs", "thing", "desc", "generic", "crb2nu/thing")
	if len(g) != 7 || g[0].FilePath != "README.md" || !strings.Contains(g[0].Content, "crb2nu/thing") {
		t.Fatalf("generic = %+v", g)
	}
	goSeed := SeedActions("libs", "thing", "", "go", "")
	if len(goSeed) != 10 || !strings.Contains(goSeed[7].Content, "module gitlab.flexinfer.ai/libs/thing") {
		t.Fatalf("go = %+v", goSeed)
	}
	if strings.Join(SeedPaths("go", "thing"), ",") != strings.Join(func() []string {
		var p []string
		for _, a := range goSeed {
			p = append(p, a.FilePath)
		}
		return p
	}(), ",") {
		t.Fatal("SeedPaths must match SeedActions order")
	}
}
