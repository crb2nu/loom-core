package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// NB: fixtures model the REAL producer bytes. Verified against the live
// GitLab instance (gitlab.flexinfer.ai, 2026-07-18): the merge-requests LIST
// endpoint returns NO pipeline info on list items — neither `head_pipeline`
// nor `pipeline` — only the single-MR GET includes `head_pipeline`.

func TestListOpenMergeRequests_RealListShape_NoPipelineKeys(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			// Real list-item shape: no head_pipeline, no pipeline key at all.
			return 200, []map[string]any{
				{
					"iid":                          10,
					"title":                        "feat: a",
					"state":                        "opened",
					"web_url":                      "https://gitlab.example/mr/10",
					"source_branch":                "feat/a",
					"target_branch":                "main",
					"draft":                        false,
					"has_conflicts":                false,
					"detailed_merge_status":        "mergeable",
					"merge_when_pipeline_succeeds": true,
					"updated_at":                   "2026-07-18T12:00:00Z",
					"sha":                          "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
				},
				{
					"iid":              11,
					"title":            "wip: b",
					"state":            "opened",
					"source_branch":    "feat/b",
					"work_in_progress": true,
				},
			}
		},
	})

	items, err := cli.ListOpenMergeRequests(context.Background(), 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}

	a := items[0]
	if a.IID != 10 || a.SourceBranch != "feat/a" || !a.MergeWhenPipelineSucceeds {
		t.Errorf("item[0] unexpected: %+v", a)
	}
	// The head sha IS on list items (unlike pipeline info) — the shepherd binds
	// its auto-merge arm to it.
	if a.SHA != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("item[0] sha = %q, want the head commit", a.SHA)
	}
	if items[1].SHA != "" {
		t.Errorf("item[1] sha = %q, want empty when GitLab omits it", items[1].SHA)
	}
	// The true list shape carries no pipeline info — both fields must be nil
	// and EffectiveHeadPipeline must be nil (enrichment's job to fill in).
	if a.HeadPipeline != nil || a.Pipeline != nil || a.EffectiveHeadPipeline() != nil {
		t.Errorf("list items must have nil pipeline info, got head=%+v pipe=%+v", a.HeadPipeline, a.Pipeline)
	}
	if a.UpdatedAt.IsZero() {
		t.Error("item[0] updated_at not parsed")
	}

	if !items[1].IsDraft() {
		t.Error("item[1] work_in_progress should map to IsDraft")
	}

	// Verify the request scoped to the client's project with reused auth.
	if len(rt.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(rt.requests))
	}
	got := rt.requests[0]
	if got.Method != http.MethodGet {
		t.Errorf("method = %s", got.Method)
	}
	if got.Token != "tok-123" {
		t.Errorf("token = %q, want tok-123 (reused mills auth)", got.Token)
	}
}

func TestGetMergeRequest_ReturnsHeadPipeline(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/10": func(_ *http.Request) (int, any) {
			// Single-MR GET is the only endpoint that carries head_pipeline.
			return 200, map[string]any{
				"iid":                          10,
				"title":                        "feat: a",
				"state":                        "opened",
				"source_branch":                "feat/a",
				"target_branch":                "main",
				"detailed_merge_status":        "ci_still_running",
				"merge_when_pipeline_succeeds": true,
				"updated_at":                   "2026-07-18T12:00:00Z",
				"sha":                          armHeadSHA,
				"head_pipeline": map[string]any{
					"id":      999,
					"status":  "running",
					"source":  "push",
					"web_url": "https://gitlab.example/pipe/999",
				},
			}
		},
	})

	item, err := cli.GetMergeRequest(context.Background(), 10)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	hp := item.EffectiveHeadPipeline()
	if hp == nil || hp.ID != 999 || hp.Status != "running" {
		t.Fatalf("head pipeline = %+v, want id=999 status=running", hp)
	}
	if item.DetailedMergeStatus != "ci_still_running" {
		t.Errorf("detailed_merge_status = %q", item.DetailedMergeStatus)
	}
	if item.SHA != armHeadSHA {
		t.Errorf("sha = %q, want the enriched head commit %q", item.SHA, armHeadSHA)
	}
	if len(rt.requests) != 1 || rt.requests[0].Token != "tok-123" {
		t.Errorf("expected 1 authed request, got %+v", rt.requests)
	}
}

func TestGetMergeRequest_ErrorSurfaces(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
	if _, err := cli.GetMergeRequest(context.Background(), 404); err == nil {
		t.Fatal("expected error for missing MR")
	}
}

// ----- mrwatch shepherd write actions (M4) -----

func TestRetryPipeline_PostsToRetryEndpoint(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/pipelines/42/retry": func(_ *http.Request) (int, any) {
			return 201, map[string]any{"id": 42, "status": "running"}
		},
	})
	if err := cli.RetryPipeline(context.Background(), 42); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(rt.requests))
	}
	got := rt.requests[0]
	if got.Method != http.MethodPost || got.Token != "tok-123" {
		t.Errorf("request = %+v, want POST with reused auth", got)
	}
}

func TestRetryPipeline_RejectsZeroID(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
	if err := cli.RetryPipeline(context.Background(), 0); err == nil {
		t.Fatal("expected error for zero pipeline id")
	}
}

func TestCreatePipelineForRef_PostsRefAndReturnsID(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			return 201, map[string]any{"id": 7001, "status": "created", "web_url": "https://gitlab.example/pipe/7001"}
		},
	})
	id, err := cli.CreatePipelineForRef(context.Background(), "feat/x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != 7001 {
		t.Errorf("id = %d, want 7001", id)
	}
	if len(rt.requests) != 1 || rt.requests[0].Method != http.MethodPost {
		t.Fatalf("expected 1 POST, got %+v", rt.requests)
	}
	// The ref is posted in the body (transport consumes it before the handler).
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(rt.requests[0].Body), &body); err != nil {
		t.Fatalf("decode recorded body: %v", err)
	}
	if body.Ref != "feat/x" {
		t.Errorf("ref = %q, want feat/x", body.Ref)
	}
}

func TestCreatePipelineForRef_RejectsEmptyRef(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
	if _, err := cli.CreatePipelineForRef(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty ref")
	}
}

// armHeadSHA is a realistic 40-hex head commit for the arm tests.
const armHeadSHA = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

func TestArmAutoMerge_UsesAutoMergeQueryParam(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/10/merge": func(r *http.Request) (int, any) {
			// The arm must use auto_merge=true (the legacy MWPS body param 409s
			// on this instance), never merge_when_pipeline_succeeds — AND it must
			// pin the observed head sha so GitLab rejects the arm (409) if the
			// source branch moved since the caller observed it.
			q := r.URL.Query()
			if got := q.Get("auto_merge"); got != "true" {
				t.Errorf("auto_merge = %q, want true", got)
			}
			if got := q.Get("sha"); got != armHeadSHA {
				t.Errorf("sha = %q, want the observed head %q", got, armHeadSHA)
			}
			if q.Get("merge_when_pipeline_succeeds") != "" {
				t.Errorf("legacy mwps param must not be sent: %q", r.URL.RawQuery)
			}
			// The exact query the shepherd relies on, in order.
			if want := "auto_merge=true&sha=" + armHeadSHA; r.URL.RawQuery != want {
				t.Errorf("query = %q, want %q", r.URL.RawQuery, want)
			}
			return 200, map[string]any{"iid": 10, "state": "opened"}
		},
	})
	if err := cli.ArmAutoMerge(context.Background(), 10, armHeadSHA); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if len(rt.requests) != 1 || rt.requests[0].Method != http.MethodPut {
		t.Errorf("expected 1 PUT, got %+v", rt.requests)
	}
}

func TestArmAutoMerge_405SurfacesStatusForDeferral(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/10/merge": func(_ *http.Request) (int, any) {
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
	})
	err := cli.ArmAutoMerge(context.Background(), 10, armHeadSHA)
	if err == nil {
		t.Fatal("expected 405 error")
	}
	code, ok := GitLabHTTPStatus(err)
	if !ok || code != 405 {
		t.Errorf("GitLabHTTPStatus = (%d,%v), want (405,true) so the shepherd can defer", code, ok)
	}
}

// TestArmAutoMerge_409SurfacesStatusForHeadMoved: GitLab answers 409 when the
// sha precondition does not match the current head of the source branch. The
// status must surface so the shepherd can classify it as "head moved" rather
// than a generic error.
func TestArmAutoMerge_409SurfacesStatusForHeadMoved(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/10/merge": func(_ *http.Request) (int, any) {
			return 409, map[string]any{"message": "SHA does not match HEAD of source branch"}
		},
	})
	err := cli.ArmAutoMerge(context.Background(), 10, armHeadSHA)
	if err == nil {
		t.Fatal("expected 409 error")
	}
	code, ok := GitLabHTTPStatus(err)
	if !ok || code != 409 {
		t.Errorf("GitLabHTTPStatus = (%d,%v), want (409,true) so the shepherd can defer to the next poll", code, ok)
	}
}

func TestArmAutoMerge_RejectsZeroIID(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
	if err := cli.ArmAutoMerge(context.Background(), 0, armHeadSHA); err == nil {
		t.Fatal("expected error for zero iid")
	}
}

// TestArmAutoMerge_RejectsMissingSHA: without an observed head the arm is
// refused locally — no request is issued, so an unpinned arm can never reach
// GitLab even if a caller forgets to carry the SHA.
func TestArmAutoMerge_RejectsMissingSHA(t *testing.T) {
	for _, sha := range []string{"", "   "} {
		cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
		if err := cli.ArmAutoMerge(context.Background(), 10, sha); err == nil {
			t.Fatalf("sha %q: expected refusal", sha)
		}
		if len(rt.requests) != 0 {
			t.Errorf("sha %q: must not issue a request, got %+v", sha, rt.requests)
		}
	}
}

func TestListOpenMergeRequests_PerPageClamped(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(r *http.Request) (int, any) {
			if pp := r.URL.Query().Get("per_page"); pp != "100" {
				t.Errorf("per_page = %q, want clamped to 100", pp)
			}
			return 200, []map[string]any{}
		},
	})
	if _, err := cli.ListOpenMergeRequests(context.Background(), 5000); err != nil {
		t.Fatalf("list: %v", err)
	}
}

// TestListMergedMergeRequests_ScopesAndParsesMergedAt: the merged listing must
// ask GitLab for state=merged bounded by updated_after, and must surface
// merged_at — the HUD registry anchors its retention window on it.
func TestListMergedMergeRequests_ScopesAndParsesMergedAt(t *testing.T) {
	since := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(r *http.Request) (int, any) {
			q := r.URL.Query()
			if got := q.Get("state"); got != "merged" {
				t.Errorf("state = %q, want merged", got)
			}
			if got := q.Get("updated_after"); got != since.Format(time.RFC3339) {
				t.Errorf("updated_after = %q, want %q", got, since.Format(time.RFC3339))
			}
			return 200, []map[string]any{
				{
					"iid":           10,
					"title":         "feat: a",
					"state":         "merged",
					"source_branch": "feat/a",
					"target_branch": "main",
					"updated_at":    "2026-08-02T10:00:00Z",
					"merged_at":     "2026-08-02T09:59:00Z",
				},
			}
		},
	})

	items, err := cli.ListMergedMergeRequests(context.Background(), 50, since)
	if err != nil {
		t.Fatalf("list merged: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if items[0].State != "merged" {
		t.Errorf("state = %q, want merged", items[0].State)
	}
	want := time.Date(2026, 8, 2, 9, 59, 0, 0, time.UTC)
	if !items[0].MergedAt.Equal(want) {
		t.Errorf("merged_at = %v, want %v", items[0].MergedAt, want)
	}
}

// TestListMergedMergeRequests_ZeroSinceOmitsBound: a zero `since` must not send
// an updated_after param at all (unbounded listing), and per_page still clamps.
func TestListMergedMergeRequests_ZeroSinceOmitsBound(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(r *http.Request) (int, any) {
			if _, ok := r.URL.Query()["updated_after"]; ok {
				t.Errorf("updated_after sent for a zero since: %s", r.URL.RawQuery)
			}
			if pp := r.URL.Query().Get("per_page"); pp != "100" {
				t.Errorf("per_page = %q, want clamped to 100", pp)
			}
			return 200, []map[string]any{}
		},
	})
	if _, err := cli.ListMergedMergeRequests(context.Background(), 5000, time.Time{}); err != nil {
		t.Fatalf("list merged: %v", err)
	}
}

// TestListOpenMergeRequests_NullMergedAtStaysZero: open MRs carry
// "merged_at": null; that must decode to the zero time, not an error.
func TestListOpenMergeRequests_NullMergedAtStaysZero(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 200, []map[string]any{
				{"iid": 10, "state": "opened", "source_branch": "feat/a", "merged_at": nil},
			}
		},
	})
	items, err := cli.ListOpenMergeRequests(context.Background(), 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || !items[0].MergedAt.IsZero() {
		t.Errorf("merged_at = %v, want zero", items[0].MergedAt)
	}
}
