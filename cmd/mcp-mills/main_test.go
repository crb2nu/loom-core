package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

type fakeProber struct{ branches []string }

func (f *fakeProber) ImplementBranches(context.Context, string, string) ([]string, error) {
	return f.branches, nil
}

// fakeOperator simulates the PascalCase backlog wire contract with
// revision-conflict behavior: a POST whose Revision != stored 409s.
type fakeOperator struct {
	item       map[string]any
	gets       int
	posts      []map[string]any
	starts     int
	startFail  bool
	concurrent func(map[string]any)
	conflict   int // number of leading POSTs to reject with 409
}

func (f *fakeOperator) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mills/backlog/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.gets++
		_ = json.NewEncoder(w).Encode(f.item)
	})
	mux.HandleFunc("GET /api/mills/pipeline/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("POST /api/mills/backlog", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body)
		if len(f.posts) <= f.conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"stale revision"}`))
			// A conflict means someone else wrote: bump the stored revision.
			f.item["Revision"] = f.item["Revision"].(float64) + 1
			if f.concurrent != nil {
				f.concurrent(f.item)
			}
			return
		}
		if body["Revision"] != nil && body["Revision"] != f.item["Revision"] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		rev := f.item["Revision"]
		f.item = make(map[string]any, len(body))
		for key, value := range body {
			f.item[key] = value
		}
		f.item["Revision"] = rev
		f.item["Revision"] = f.item["Revision"].(float64) + 1
		_ = json.NewEncoder(w).Encode(f.item)
	})
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		f.starts++
		if r.URL.Query().Get("requeue") != "1" || len(f.posts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.startFail {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"decision": "started", "run_id": "PIPE-test"})
	})
	return mux
}

func newTestServer(t *testing.T, op *fakeOperator, prober branchProber) (*millsServer, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(op.handler())
	t.Cleanup(ts.Close)
	return &millsServer{
		api:      &operatorClient{base: ts.URL, token: "test-token", client: httpclient.New(httpclient.DefaultConfig())},
		brancher: prober,
	}, ts
}

func item(state string, rev float64) map[string]any {
	return map[string]any{
		"ID": "bl-test-202608", "Title": "t", "State": state, "Revision": rev,
		"TargetProject": "", "SpecDoc": "spec", "Slices": []any{},
	}
}

func TestUpdateStateEchoesRevisionAndPreservesObject(t *testing.T) {
	op := &fakeOperator{item: item("escalated", 3)}
	s, _ := newTestServer(t, op, &fakeProber{})
	res, err := s.handleUpdateState(context.Background(), map[string]any{"id": "bl-test-202608", "state": "retired"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected error: %v %v", err, res)
	}
	if len(op.posts) != 1 {
		t.Fatalf("posts: %d", len(op.posts))
	}
	body := op.posts[0]
	if body["Revision"].(float64) != 3 || body["State"] != "retired" || body["SpecDoc"] != "spec" {
		t.Fatalf("stored object not preserved with echoed revision: %v", body)
	}
}

func TestUpdateStateRetriesOnceFromFreshReadOn409(t *testing.T) {
	op := &fakeOperator{item: item("escalated", 3), conflict: 1}
	s, _ := newTestServer(t, op, &fakeProber{})
	res, err := s.handleUpdateState(context.Background(), map[string]any{"id": "bl-test-202608", "state": "merged"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected error: %v %v", err, res)
	}
	if op.gets != 2 || len(op.posts) != 2 {
		t.Fatalf("expected re-read then retry: gets=%d posts=%d", op.gets, len(op.posts))
	}
	if op.posts[1]["Revision"].(float64) != 4 {
		t.Fatalf("retry did not echo the fresh revision: %v", op.posts[1]["Revision"])
	}
}

func TestRequeueRefusedWhileImplementBranchExists(t *testing.T) {
	op := &fakeOperator{item: item("escalated", 3)}
	s, _ := newTestServer(t, op, &fakeProber{branches: []string{"feat/bl-test-202608/slice"}})
	res, _ := s.handleUpdateState(context.Background(), map[string]any{"id": "bl-test-202608", "state": "queued"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "requeue refused") {
		t.Fatalf("expected requeue guard, got: %+v", res.Content[0].Text)
	}
	if len(op.posts) != 0 {
		t.Fatal("guard must refuse before any write")
	}

	res, _ = s.handleUpdateState(context.Background(), map[string]any{"id": "bl-test-202608", "state": "queued", "force": true})
	if res.IsError {
		t.Fatalf("force must override the guard: %v", res.Content[0].Text)
	}
}

func TestBacklogPostRequiresGroundingFiles(t *testing.T) {
	op := &fakeOperator{item: item("queued", 1)}
	s, _ := newTestServer(t, op, &fakeProber{})
	res, _ := s.handleBacklogPost(context.Background(), map[string]any{
		"id": "bl-x-202608", "title": "t", "spec_doc": "s", "files": []any{},
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "at least one file") {
		t.Fatalf("expected grounding-files validation, got %+v", res)
	}

	res, _ = s.handleBacklogPost(context.Background(), map[string]any{
		"id": "bl-x-202608", "title": "t", "spec_doc": "s",
		"files": []any{"pkg/a.go"}, "tests": []any{"go test ./..."},
	})
	if res.IsError {
		t.Fatalf("valid post rejected: %v", res.Content[0].Text)
	}
	body := op.posts[len(op.posts)-1]
	slices := body["Slices"].([]any)[0].(map[string]any)
	if slices["files"].([]any)[0] != "pkg/a.go" || body["CreatedBy"] != "mcp-mills" {
		t.Fatalf("wire shape wrong: %v", body)
	}
}

func TestMutationsNeedToken(t *testing.T) {
	op := &fakeOperator{item: item("escalated", 3)}
	s, _ := newTestServer(t, op, &fakeProber{})
	s.api.token = ""
	res, _ := s.handleUpdateState(context.Background(), map[string]any{"id": "bl-test-202608", "state": "retired"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "LOOM_MILLS_TOKEN") {
		t.Fatalf("expected NotConfigured guidance, got %+v", res)
	}
}

func TestDiagnoseVerdicts(t *testing.T) {
	op := &fakeOperator{item: item("escalated", 3)}
	s, _ := newTestServer(t, op, &fakeProber{branches: []string{"feat/bl-test-202608/slice"}})
	res, _ := s.handleDiagnose(context.Background(), map[string]any{"id": "bl-test-202608"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "RESCUE") {
		t.Fatalf("expected RESCUE verdict: %+v", res.Content[0])
	}

	s.brancher = &fakeProber{}
	res, _ = s.handleDiagnose(context.Background(), map[string]any{"id": "bl-test-202608"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "REQUEUE-SAFE") {
		t.Fatalf("expected REQUEUE-SAFE verdict: %+v", res.Content[0])
	}
}

func scopeItem() map[string]any {
	it := item("escalated", 7)
	it["Slices"] = []any{map[string]any{"name": "first", "files": []any{"pkg/a.go"}, "tests": []any{"keep"}}, map[string]any{"name": "second", "files": []any{"pkg/b.go"}}}
	it["Success"] = map[string]any{"tests": []any{"go test ./old"}, "custom": "keep"}
	it["Unknown"] = map[string]any{"keep": true}
	return it
}

func TestAmendScopePreservesAndRetries(t *testing.T) {
	for _, conflicts := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(conflicts), func(t *testing.T) {
			op := &fakeOperator{item: scopeItem(), conflict: conflicts, concurrent: func(it map[string]any) {
				it["Title"] = "concurrent"
				it["Slices"].([]any)[0].(map[string]any)["files"] = []any{"pkg/a.go", "pkg/concurrent.go"}
				it["Success"].(map[string]any)["tests"] = []any{"go test ./old", "go test ./concurrent"}
			}}
			s, _ := newTestServer(t, op, &fakeProber{})
			res, err := s.handleAmendScope(context.Background(), map[string]any{"id": "bl-test-202608", "add_files": []any{"pkg/a.go", "pkg/new.go", "pkg/new.go"}, "add_tests": []any{"go test ./old", "go test ./new", "go test ./new"}, "requeue": true})
			if err != nil || res.IsError != (conflicts == 2) {
				t.Fatalf("result: %v %+v", err, res)
			}
			wantPosts := 1
			if conflicts > 0 {
				wantPosts = 2
			}
			if len(op.posts) != wantPosts || op.gets != wantPosts {
				t.Fatalf("gets/posts: %d/%d", op.gets, len(op.posts))
			}
			for i, body := range op.posts {
				if body["Revision"] != float64(7+i) {
					t.Fatalf("revision: %v", body)
				}
			}
			if conflicts == 2 {
				if op.starts != 0 {
					t.Fatal("started after conflict")
				}
				return
			}
			wantFiles := []any{"pkg/a.go", "pkg/new.go"}
			wantTests := []any{"go test ./old", "go test ./new"}
			if conflicts == 1 {
				wantFiles = []any{"pkg/a.go", "pkg/concurrent.go", "pkg/new.go"}
				wantTests = []any{"go test ./old", "go test ./concurrent", "go test ./new"}
				if op.item["Title"] != "concurrent" {
					t.Fatal("lost concurrent title")
				}
			}
			slices := op.item["Slices"].([]any)
			if !reflect.DeepEqual(slices[0].(map[string]any)["files"], wantFiles) || !reflect.DeepEqual(op.item["Success"].(map[string]any)["tests"], wantTests) {
				t.Fatalf("amendment: %v", op.item)
			}
			if !reflect.DeepEqual(slices[1], scopeItem()["Slices"].([]any)[1]) || op.item["SpecDoc"] != "spec" || op.item["State"] != "escalated" || !reflect.DeepEqual(op.item["Unknown"], scopeItem()["Unknown"]) || op.item["Success"].(map[string]any)["custom"] != "keep" {
				t.Fatal("lost untouched fields")
			}
			if op.starts != 1 || !strings.Contains(res.Content[0].Text, "PIPE-test") || !strings.Contains(res.Content[0].Text, "files_added") {
				t.Fatalf("summary/start: %+v", res)
			}
		})
	}
}

func TestAmendScopeValidationAndSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		bad  bool
	}{
		{"empty", map[string]any{}, true},
		{"directory", map[string]any{"add_files": []any{"pkg/"}}, true},
		{"bare", map[string]any{"add_files": []any{"pkg"}}, true},
		{"invalid array", map[string]any{"add_tests": []any{4}}, true},
		{"blank", map[string]any{"add_tests": []any{" "}}, true},
		{"unknown slice", map[string]any{"slice_name": "missing", "add_tests": []any{"test"}}, true},
		{"named", map[string]any{"slice_name": "second", "add_files": []any{"pkg/new.go"}}, false},
		{"tests only", map[string]any{"add_tests": []any{"test"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := &fakeOperator{item: scopeItem()}
			s, _ := newTestServer(t, op, &fakeProber{})
			tc.args["id"] = "bl-test-202608"
			res, _ := s.handleAmendScope(context.Background(), tc.args)
			if res.IsError != tc.bad {
				t.Fatalf("result %+v", res)
			}
			if tc.bad && len(op.posts) > 0 {
				t.Fatal("invalid input wrote")
			}
			if op.starts != 0 {
				t.Fatal("default requeued")
			}
			if tc.name == "named" && !reflect.DeepEqual(op.item["Slices"].([]any)[0], scopeItem()["Slices"].([]any)[0]) {
				t.Fatal("changed first slice")
			}
		})
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(file, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{file, "pkg/*.go", "Dockerfile*", "pkg/new.go"} {
		if err := validateScopeFile(value); err != nil {
			t.Error(err)
		}
	}
	for _, value := range []string{dir, dir + "/", "pkg/**", "pkg/["} {
		if validateScopeFile(value) == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

type rescueProber struct {
	fakeProber
	rescue bool
	err    error
}

func (p *rescueProber) ScopeRescueBranch(context.Context, string, string) (bool, error) {
	return p.rescue, p.err
}
func TestAmendScopeRequeueGuard(t *testing.T) {
	for _, tc := range []struct {
		name                string
		rescue, force, fail bool
		probeErr            error
		bad                 bool
	}{
		{name: "ordinary", bad: true}, {name: "rescue", rescue: true}, {name: "force", force: true},
		{name: "probe failure", probeErr: fmt.Errorf("unavailable"), bad: true}, {name: "start failure", rescue: true, fail: true, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := &fakeOperator{item: scopeItem(), startFail: tc.fail}
			s, _ := newTestServer(t, op, &rescueProber{fakeProber: fakeProber{branches: []string{"feat/bl-test-202608/first"}}, rescue: tc.rescue, err: tc.probeErr})
			res, _ := s.handleAmendScope(context.Background(), map[string]any{"id": "bl-test-202608", "add_tests": []any{"test"}, "requeue": true, "force": tc.force})
			if res.IsError != tc.bad {
				t.Fatalf("result %+v", res)
			}
			if tc.bad && !tc.fail && (len(op.posts) > 0 || op.starts > 0) {
				t.Fatal("guard wrote")
			}
			if tc.fail && (len(op.posts) != 1 || !strings.Contains(res.Content[0].Text, "requeue_error") || res.StructuredContent.(map[string]any)["amended"] != true) {
				t.Fatalf("missing partial success: %+v", res)
			}
		})
	}
}

func TestScopeRescueMRMatching(t *testing.T) {
	for _, tc := range []struct {
		title, branch, state string
		draft, want          bool
	}{
		{"Draft: [scope-escalated] Fix", "feat/id/slice", "opened", true, true},
		{"Draft: ordinary", "feat/id/slice", "opened", true, false},
		{"Draft: [scope-escalated] Fix", "feat/other/slice", "opened", true, false},
		{"Draft: [scope-escalated] Fix", "feat/id/slice", "closed", true, false},
		{"Draft: [scope-escalated] Fix", "feat/id/slice", "opened", false, false},
	} {
		t.Run(tc.title+tc.branch+tc.state+fmt.Sprint(tc.draft), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("source_branch") != "feat/id/slice" || r.URL.Query().Get("state") != "opened" {
					t.Error("missing MR filters")
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{{"title": tc.title, "source_branch": tc.branch, "state": tc.state, "draft": tc.draft}})
			}))
			defer ts.Close()
			got, err := (&gitLsRemoteProber{gitlabBase: ts.URL}).ScopeRescueBranch(context.Background(), "services/loom-core", "feat/id/slice")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

type failingBranchProber struct{}

func (failingBranchProber) ImplementBranches(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("remote unavailable")
}

func TestAmendScopeFailsClosed(t *testing.T) {
	for _, mode := range []string{"no slices", "branch probe", "missing token", "missing id"} {
		t.Run(mode, func(t *testing.T) {
			op := &fakeOperator{item: scopeItem()}
			s, _ := newTestServer(t, op, &fakeProber{})
			args := map[string]any{"id": "bl-test-202608", "add_files": []any{"pkg/new.go"}, "requeue": true}
			switch mode {
			case "no slices":
				op.item["Slices"] = []any{}
			case "branch probe":
				s.brancher = failingBranchProber{}
			case "missing token":
				s.api.token = ""
			case "missing id":
				delete(args, "id")
			}
			res, _ := s.handleAmendScope(context.Background(), args)
			if !res.IsError || len(op.posts) != 0 || op.starts != 0 {
				t.Fatalf("expected refusal before write: %+v", res)
			}
		})
	}
}

func TestStatusVendorDegradationAndRecovery(t *testing.T) {
	open := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mills/status" {
			t.Errorf("path=%s", r.URL.Path)
		}
		caps := []map[string]any{{"id": "sqlite_store", "status": "green"}}
		if open {
			caps = append(caps, map[string]any{"id": "anthropic_api", "status": "yellow", "message": "billing"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
	}))
	defer ts.Close()
	s := &millsServer{api: &operatorClient{base: ts.URL, client: httpclient.New(httpclient.DefaultConfig())}}
	for _, wantOpen := range []bool{true, false} {
		open = wantOpen
		res, err := s.handleStatus(context.Background(), nil)
		if err != nil || res.IsError {
			t.Fatalf("status: %v %+v", err, res)
		}
		var out struct {
			Degraded []struct {
				ID      string `json:"id"`
				Message string `json:"message"`
			} `json:"degraded_capabilities"`
		}
		payload, marshalErr := json.Marshal(res.StructuredContent)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err = json.Unmarshal(payload, &out); err != nil {
			t.Fatal(err)
		}
		if wantOpen {
			if len(out.Degraded) != 1 || out.Degraded[0].ID != "anthropic_api" || out.Degraded[0].Message != "billing" {
				t.Fatalf("degraded=%+v", out)
			}
		} else if len(out.Degraded) != 0 {
			t.Fatalf("did not recover: %+v", out)
		}
	}
}

func TestBacklogScopeEnvelopeGuard(t *testing.T) {
	for _, amend := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			files   []any
			dropped []string
			warning string
			bad     bool
		}{
			{name: "fragment", files: []any{"pkg/ordinary/a.go", "changelog.d/item.fixed.md"}, dropped: []string{"changelog.d/item.fixed.md"}},
			{name: "normalized fragment", files: []any{"pkg/ordinary/a.go", "./changelog.d/item.fixed.md"}, dropped: []string{"./changelog.d/item.fixed.md"}},
			{name: "fragment only", files: []any{"changelog.d/item.fixed.md"}, dropped: []string{"changelog.d/item.fixed.md"}, bad: !amend},
			{name: "ancestor", files: []any{"pkg/ordinary/a.go", "pkg/ordinary/sub/b.go"}, warning: "pkg/ordinary/a.go"},
			{name: "mills root", files: []any{"pkg/mills/policy.go", "pkg/mills/gates/x.go"}, warning: "pkg/mills/policy.go"},
			{name: "hud root", files: []any{"internal/hud/root.go"}, warning: "internal/hud/root.go"},
			{name: "cmd root", files: []any{"cmd/root.go"}, warning: "cmd/root.go"},
			{name: "same package", files: []any{"pkg/ordinary/a.go", "pkg/ordinary/b.go"}},
			{name: "prefix sibling", files: []any{"pkg/ordinary/a.go", "pkg/ordinary2/b.go"}},
			{name: "bare directory", files: []any{"pkg"}, bad: true},
			{name: "trailing slash", files: []any{"pkg/a.go/"}, bad: true},
			{name: "fragment directory", files: []any{"changelog.d/"}, bad: true},
		} {
			t.Run(fmt.Sprintf("amend=%t/%s", amend, tc.name), func(t *testing.T) {
				it := scopeItem()
				it["Slices"].([]any)[0].(map[string]any)["files"] = []any{"pkg/ordinary/existing.go"}
				op := &fakeOperator{item: it}
				s, _ := newTestServer(t, op, &fakeProber{})
				args := map[string]any{"id": "bl-test", "title": "title", "spec_doc": "spec", "files": tc.files, "add_files": tc.files}
				handler := s.handleBacklogPost
				if amend {
					handler = s.handleAmendScope
				}
				res, err := handler(context.Background(), args)
				if err != nil || res.IsError != tc.bad {
					t.Fatalf("result: %+v, %v", res, err)
				}
				if tc.bad && len(op.posts) != 0 {
					t.Fatal("invalid scope posted")
				}
				if tc.bad && tc.dropped == nil {
					return
				}
				var out struct {
					Dropped  []string `json:"dropped_files"`
					Warnings []string `json:"warnings"`
				}
				data, err := json.Marshal(res.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &out); err != nil {
					t.Fatal(err)
				}
				if strings.Join(out.Dropped, ",") != strings.Join(tc.dropped, ",") {
					t.Fatalf("dropped: %v", out.Dropped)
				}
				if tc.warning == "" && len(out.Warnings) != 0 {
					t.Fatalf("unexpected warnings: %v", out.Warnings)
				}
				if tc.warning != "" && (!strings.Contains(strings.Join(out.Warnings, " "), tc.warning) || !strings.Contains(strings.Join(out.Warnings, " "), path.Dir(tc.warning))) {
					t.Fatalf("missing envelope warning: %v", out.Warnings)
				}
				for _, body := range op.posts {
					files := body["Slices"].([]any)[0].(map[string]any)["files"]
					if strings.Contains(fmt.Sprint(files), "changelog.d") {
						t.Fatalf("fragment posted: %v", files)
					}
				}
			})
		}
	}
}

func TestAmendScopeEnvelopeRetry(t *testing.T) {
	it := scopeItem()
	it["Slices"].([]any)[0].(map[string]any)["files"] = []any{"pkg/ordinary/sub/a.go", "changelog.d/old.fixed.md"}
	op := &fakeOperator{item: it, conflict: 1, concurrent: func(it map[string]any) {
		it["Title"] = "concurrent title"
		it["Slices"].([]any)[0].(map[string]any)["files"] = []any{"pkg/ordinary/root.go", "changelog.d/concurrent.fixed.md"}
	}}
	s, _ := newTestServer(t, op, &fakeProber{})
	res, err := s.handleAmendScope(context.Background(), map[string]any{"id": "bl-test", "add_files": []any{"pkg/ordinary/sub/new.go", "changelog.d/new.fixed.md"}})
	if err != nil || res.IsError {
		t.Fatalf("result: %+v, %v", res, err)
	}
	if len(op.posts) != 2 || op.item["Title"] != "concurrent title" {
		t.Fatalf("retry lost fields: %v", op.item)
	}
	text := res.Content[0].Text
	for _, want := range []string{"pkg/ordinary/root.go", "changelog.d/concurrent.fixed.md", "changelog.d/new.fixed.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s: %s", want, text)
		}
	}
	if strings.Contains(text, "changelog.d/old.fixed.md") {
		t.Fatalf("stale report: %s", text)
	}
	for _, body := range op.posts {
		if strings.Contains(fmt.Sprint(body["Slices"].([]any)[0]), "changelog.d") {
			t.Fatalf("fragment posted: %v", body)
		}
	}
}
