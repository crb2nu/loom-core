package mrwatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// fakeGitLabTransport serves canned JSON keyed on "METHOD path-prefix" and
// records every request path, so tests can assert the exact API call budget.
// Fixtures model the REAL producer bytes: the LIST endpoint returns items
// WITHOUT any pipeline key (verified live 2026-07-18); only the single-MR GET
// carries head_pipeline.
type fakeGitLabTransport struct {
	mu     sync.Mutex
	routes map[string]func(*http.Request) (int, any)
	paths  []string
}

func (f *fakeGitLabTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	matchPath := req.URL.Path
	if req.URL.RawPath != "" {
		matchPath = req.URL.RawPath
	}
	f.paths = append(f.paths, matchPath)
	// Prefer the longest matching prefix so the single-MR routes win over the
	// list route (which is a prefix of them).
	var bestHandler func(*http.Request) (int, any)
	bestLen := -1
	for prefix, handler := range f.routes {
		method, path, _ := strings.Cut(prefix, " ")
		if req.Method == method && strings.HasPrefix(matchPath, path) && len(path) > bestLen {
			bestLen = len(path)
			bestHandler = handler
		}
	}
	if bestHandler != nil {
		status, payload := bestHandler(req)
		buf, _ := json.Marshal(payload)
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(string(buf))),
			Header:     make(http.Header),
		}, nil
	}
	return &http.Response{
		StatusCode: 404,
		Body:       io.NopCloser(strings.NewReader(`{"message":"not found"}`)),
		Header:     make(http.Header),
	}, nil
}

func (f *fakeGitLabTransport) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.paths))
	copy(out, f.paths)
	return out
}

func newSourceStub(t *testing.T, routes map[string]func(*http.Request) (int, any)) (*GitLabSource, *fakeGitLabTransport) {
	t.Helper()
	cli, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:  "https://gitlab.example/api/v4",
		Token:   "tok-123",
		Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rt := &fakeGitLabTransport{routes: routes}
	cli.SetTransport(rt)
	return NewGitLabSource(cli), rt
}

// listItemNoPipeline is a real-shape list item: no pipeline key at all.
func listItemNoPipeline(iid int, branch, updatedAt string) map[string]any {
	return map[string]any{
		"iid":                          iid,
		"title":                        "mr " + branch,
		"state":                        "opened",
		"source_branch":                branch,
		"target_branch":                "main",
		"merge_when_pipeline_succeeds": true,
		"updated_at":                   updatedAt,
	}
}

func TestGitLabSource_EnrichesHeadPipelineFromSingleMRGet(t *testing.T) {
	src, _ := newSourceStub(t, map[string]func(*http.Request) (int, any){
		// LIST: real shape, no pipeline info on any item.
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 200, []any{
				listItemNoPipeline(10, "feat/a", "2026-07-18T12:00:00Z"),
				listItemNoPipeline(11, "feat/b", "2026-07-18T11:00:00Z"),
			}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/10": func(_ *http.Request) (int, any) {
			it := listItemNoPipeline(10, "feat/a", "2026-07-18T12:00:00Z")
			it["head_pipeline"] = map[string]any{
				"id": 999, "status": "failed", "web_url": "https://gitlab.example/pipe/999",
			}
			return 200, it
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/11": func(_ *http.Request) (int, any) {
			// Enrichment failure: this MR degrades to its list view.
			return 500, map[string]any{"message": "boom"}
		},
	})

	infos, err := src.ListOpenMRs(context.Background(), "services/loom-core")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("want 2 MRs, got %d", len(infos))
	}

	// Newest-updated first after the defensive sort.
	a, b := infos[0], infos[1]
	if a.IID != 10 || b.IID != 11 {
		t.Fatalf("order = %d,%d, want 10,11 (newest first)", a.IID, b.IID)
	}

	// MR 10: enriched — head pipeline present, classifies as a CI failure.
	if a.Pipeline == nil || a.Pipeline.Status != "failed" {
		t.Fatalf("enriched MR pipeline = %+v, want failed", a.Pipeline)
	}
	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	state, reason := Classify(a, now, DefaultStaleAfter)
	if state != StateCIFailedDeterministic {
		t.Errorf("enriched MR state = %q (%s), want ci_failed_deterministic", state, reason)
	}

	// MR 11: enrichment failed — degrades to list view (nil pipeline) and
	// still classifies sanely (awaiting_pipeline), never fails the poll.
	if b.Pipeline != nil {
		t.Errorf("enrichment-failed MR should have nil pipeline, got %+v", b.Pipeline)
	}
	state, reason = Classify(b, now, DefaultStaleAfter)
	if state != StateAwaitingPipeline {
		t.Errorf("enrichment-failed MR state = %q (%s), want awaiting_pipeline", state, reason)
	}
}

func TestGitLabSource_MaxEnrichBoundsAPICalls(t *testing.T) {
	src, rt := newSourceStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 200, []any{
				listItemNoPipeline(11, "feat/b", "2026-07-18T11:00:00Z"), // older
				listItemNoPipeline(10, "feat/a", "2026-07-18T12:00:00Z"), // newest
			}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/10": func(_ *http.Request) (int, any) {
			it := listItemNoPipeline(10, "feat/a", "2026-07-18T12:00:00Z")
			it["head_pipeline"] = map[string]any{"id": 1, "status": "success"}
			return 200, it
		},
	})
	src.SetMaxEnrich(1)

	infos, err := src.ListOpenMRs(context.Background(), "services/loom-core")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Exactly 1 enrichment call, spent on the newest-updated MR.
	var enrichCalls int
	for _, p := range rt.requestPaths() {
		if strings.HasSuffix(p, "/merge_requests/10") || strings.HasSuffix(p, "/merge_requests/11") {
			enrichCalls++
		}
	}
	if enrichCalls != 1 {
		t.Fatalf("enrichment calls = %d, want 1 (bounded)", enrichCalls)
	}
	if infos[0].IID != 10 || infos[0].Pipeline == nil {
		t.Errorf("newest MR must be enriched first: %+v", infos[0])
	}
	if infos[1].IID != 11 || infos[1].Pipeline != nil {
		t.Errorf("over-cap MR must stay list-view: %+v", infos[1])
	}
}

func TestMapListItem(t *testing.T) {
	ts := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	it := clients.MergeRequestListItem{
		IID:                       7,
		Title:                     "feat: x",
		State:                     "opened",
		WebURL:                    "https://gitlab.example/mr/7",
		SourceBranch:              "feat/x",
		TargetBranch:              "main",
		WorkInProgress:            true, // legacy draft alias
		HasConflicts:              true,
		DetailedMergeStatus:       "conflict",
		MergeWhenPipelineSucceeds: true,
		SHA:                       "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
		UpdatedAt:                 ts,
		Pipeline:                  &clients.MergeRequestPipeline{ID: 3, Status: "running", WebURL: "u"},
	}

	got := mapListItem("services/loom-core", it)

	if got.Repo != "services/loom-core" {
		t.Errorf("repo = %q", got.Repo)
	}
	if !got.Draft {
		t.Error("work_in_progress must map to Draft")
	}
	if !got.HasConflicts || got.DetailedMergeStatus != "conflict" {
		t.Error("conflict fields not mapped")
	}
	if got.Pipeline == nil || got.Pipeline.Status != "running" {
		t.Errorf("pipeline fallback not mapped: %+v", got.Pipeline)
	}
	if !got.UpdatedAt.Equal(ts) {
		t.Errorf("updated_at = %v", got.UpdatedAt)
	}
	// The observed head sha must survive the mapping — the shepherd's arm is
	// refused without it.
	if got.SHA != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("sha = %q, want the observed head commit", got.SHA)
	}
}

func TestMapListItem_NoPipeline(t *testing.T) {
	got := mapListItem("r", clients.MergeRequestListItem{IID: 1, State: "opened"})
	if got.Pipeline != nil {
		t.Errorf("expected nil pipeline, got %+v", got.Pipeline)
	}
}

// TestGitLabSource_ListsMergedWithoutEnrichment: the merged path is one list
// call per project and NO single-MR GETs — a merged MR's CI state is irrelevant
// and enrichment would double the per-poll API budget.
func TestGitLabSource_ListsMergedWithoutEnrichment(t *testing.T) {
	var _ MergedLister = (*GitLabSource)(nil)

	src, rt := newSourceStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(r *http.Request) (int, any) {
			if got := r.URL.Query().Get("state"); got != "merged" {
				t.Errorf("state = %q, want merged", got)
			}
			return 200, []map[string]any{
				{
					"iid":           10,
					"state":         "merged",
					"source_branch": "feat/a",
					"updated_at":    "2026-08-02T10:00:00Z",
					"merged_at":     "2026-08-02T09:59:00Z",
				},
			}
		},
	})

	since := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	infos, err := src.ListMergedMRs(context.Background(), "services/loom-core", since)
	if err != nil {
		t.Fatalf("list merged: %v", err)
	}
	if len(infos) != 1 || infos[0].State != "merged" {
		t.Fatalf("infos = %+v, want one merged MR", infos)
	}
	want := time.Date(2026, 8, 2, 9, 59, 0, 0, time.UTC)
	if !infos[0].MergedAt.Equal(want) {
		t.Errorf("merged_at = %v, want %v", infos[0].MergedAt, want)
	}
	if got := len(rt.requestPaths()); got != 1 {
		t.Errorf("API calls = %d, want 1 (list only, no enrichment): %v", got, rt.requestPaths())
	}
}
