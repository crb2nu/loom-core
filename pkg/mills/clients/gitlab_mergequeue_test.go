package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCreateQueuePipelinePostsQueueVariablesAndMRIID(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(*http.Request) (int, any) {
			return 201, map[string]any{"id": 71, "sha": "head", "status": "created"}
		},
	})
	got, err := cli.CreateQueuePipeline(context.Background(), "feat/x", 42)
	if err != nil || got.ID != 71 {
		t.Fatalf("CreateQueuePipeline = %+v, %v", got, err)
	}
	var body struct {
		Ref       string              `json:"ref"`
		Variables []map[string]string `json:"variables"`
	}
	if err := json.Unmarshal([]byte(rt.requests[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, variable := range body.Variables {
		vars[variable["key"]] = variable["value"]
	}
	if body.Ref != "feat/x" || vars["MILLS_MERGE_QUEUE"] != "1" || vars["MILLS_MERGE_QUEUE_MR"] != "42" {
		t.Fatalf("queue pipeline body = %#v, vars=%#v", body, vars)
	}
}

func TestPipelineTimingDecodesDurationFields(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines/71": func(*http.Request) (int, any) {
			return 200, map[string]any{"id": 71, "duration": 901.5, "queued_duration": 121.25}
		},
	})
	got, err := cli.PipelineTiming(context.Background(), 71)
	if err != nil || got.Duration == nil || *got.Duration != 901.5 || got.QueuedDuration == nil || *got.QueuedDuration != 121.25 {
		t.Fatalf("PipelineTiming = %+v, %v", got, err)
	}
}

func TestFindActivePipelineSelectsNewestEligibleExactHead(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(*http.Request) (int, any) {
			return 200, []map[string]any{
				{"id": 11, "sha": "head", "ref": "feat/x", "source": "push", "status": "success", "web_url": "https://gl/11"},
				{"id": 15, "sha": "head", "ref": "feat/x", "source": "api", "status": "pending", "web_url": "https://gl/15"},
				{"id": 19, "sha": "head", "ref": "feat/x", "source": "api", "status": "failed"},
				{"id": 20, "sha": "other", "ref": "feat/x", "source": "api", "status": "running"},
				{"id": 21, "sha": "head", "ref": "other", "source": "web", "status": "running"},
				{"id": 22, "sha": "head", "ref": "feat/x", "source": "schedule", "status": "running"},
			}
		},
	})
	got, err := cli.FindActivePipeline(context.Background(), "feat/x", "head")
	if err != nil || !got.Found || got.ID != 15 || got.Status != "pending" || got.WebURL != "https://gl/15" {
		t.Fatalf("FindActivePipeline = %+v, %v; want newest eligible exact match", got, err)
	}
}

func TestFindActivePipelineRejectsTerminalFailures(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(*http.Request) (int, any) {
			return 200, []map[string]any{{"id": 2, "sha": "head", "ref": "feat/x", "source": "api", "status": "canceled"}, {"id": 1, "sha": "head", "ref": "feat/x", "source": "push", "status": "failed"}}
		},
	})
	got, err := cli.FindActivePipeline(context.Background(), "feat/x", "head")
	if err != nil || got.Found {
		t.Fatalf("FindActivePipeline = %+v, %v; want not found", got, err)
	}
}

func TestPipelineProofPrefersExactSHA(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(r *http.Request) (int, any) {
			if r.URL.Query().Get("sha") != "head" {
				t.Fatalf("expected exact lookup first: %s", r.URL.RawQuery)
			}
			return 200, []map[string]any{{"id": 8, "sha": "head", "status": "success", "ref": "feat/x"}}
		},
		"GET /api/v4/projects/services%2Floom-core/repository/compare": func(*http.Request) (int, any) {
			t.Fatal("an exact-SHA pipeline needs no tree comparison")
			return 500, nil
		},
	})
	proof, err := cli.PipelineProof(context.Background(), "head")
	if err != nil || !proof.Found || proof.Source != "sha" || proof.ID != 8 || proof.SHA != "head" {
		t.Fatalf("exact proof = %+v, err=%v", proof, err)
	}
}

// This GitLab returns no tree_id on the single-commit API, so tree equality is
// proven with the compare API: an empty straight diff between two commits means
// identical trees. Unequal candidates are skipped, equal ones prove the head.
func TestPipelineProofAcceptsEqualSpeculativeTree(t *testing.T) {
	compares := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(r *http.Request) (int, any) {
			if r.URL.Query().Get("sha") != "" {
				return 200, []map[string]any{}
			}
			return 200, []map[string]any{
				{"id": 10, "sha": "other", "status": "success", "ref": "feat/y"},
				{"id": 9, "sha": "spec", "status": "success", "ref": "mills-mq/spec-2-deadbeef"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/repository/compare": func(r *http.Request) (int, any) {
			compares++
			q := r.URL.Query()
			if q.Get("from") != "head" || q.Get("straight") != "true" {
				t.Fatalf("compare query = %s", r.URL.RawQuery)
			}
			if q.Get("to") == "spec" {
				return 200, map[string]any{"diffs": []any{}, "compare_same_ref": false}
			}
			return 200, map[string]any{"diffs": []any{map[string]any{"new_path": "a.go"}}, "compare_same_ref": false}
		},
	})
	proof, err := cli.PipelineProof(context.Background(), "head")
	if err != nil || proof.Source != "speculative" || proof.SHA != "spec" || proof.ID != 9 {
		t.Fatalf("tree proof = %+v, err=%v", proof, err)
	}
	if compares != 2 {
		t.Fatalf("compares = %d, want 2 (one per candidate)", compares)
	}
	// Second lookup is served from the memo: no further compare calls.
	if _, err := cli.PipelineProof(context.Background(), "head"); err != nil || compares != 2 {
		t.Fatalf("memoized lookup: compares=%d err=%v", compares, err)
	}
}

// The MR-pipelines endpoint lists BRANCH pipelines alongside merge-request
// pipelines, and the branch husk can be newer than the green MR pipeline
// (flexinfer !1004: recovery pipeline 24616 "manual" minted after MR pipeline
// 24615 "success"). The source filter must keep the newer husk from
// shadowing the proof, and the SHA filter must exclude pipelines for prior
// heads.
func TestMRPipelineStatusFiltersSourceAndSHA(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/1004/pipelines": func(*http.Request) (int, any) {
			return 200, []map[string]any{
				{"id": 24616, "sha": "sha-head", "ref": "feat/fx", "status": "manual", "source": "push", "web_url": "https://gl/p/24616"},
				{"id": 24615, "sha": "sha-head", "ref": "refs/merge-requests/1004/head", "status": "success", "source": "merge_request_event", "web_url": "https://gl/p/24615"},
				{"id": 24001, "sha": "sha-old", "ref": "refs/merge-requests/1004/head", "status": "failed", "source": "merge_request_event", "web_url": "https://gl/p/24001"},
			}
		},
	})

	ps, err := cli.MRPipelineStatus(context.Background(), 1004, "sha-head")
	if err != nil {
		t.Fatalf("MRPipelineStatus: %v", err)
	}
	if !ps.Found || ps.ID != 24615 || ps.Status != "success" || ps.WebURL != "https://gl/p/24615" {
		t.Fatalf("expected the green MR pipeline despite the newer push husk, got %+v", ps)
	}

	// A head with no matching MR pipeline resolves not-found, not an error.
	ps, err = cli.MRPipelineStatus(context.Background(), 1004, "sha-unseen")
	if err != nil || ps.Found {
		t.Fatalf("unmatched sha must be a clean not-found, got %+v err=%v", ps, err)
	}
	if len(rt.requests) != 2 {
		t.Fatalf("expected 2 pipeline list requests, saw %d", len(rt.requests))
	}
}

// Among several MR pipelines for the same head (retries), the newest wins —
// a re-run supersedes an older red verdict exactly like branch resolution.
func TestMRPipelineStatusPrefersNewestForHead(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/7/pipelines": func(*http.Request) (int, any) {
			return 200, []map[string]any{
				{"id": 10, "sha": "sha-h", "status": "failed", "source": "merge_request_event"},
				{"id": 12, "sha": "sha-h", "status": "success", "source": "merge_request_event"},
			}
		},
	})
	ps, err := cli.MRPipelineStatus(context.Background(), 7, "sha-h")
	if err != nil || !ps.Found || ps.ID != 12 || ps.Status != "success" {
		t.Fatalf("newest matching pipeline must win, got %+v err=%v", ps, err)
	}
}

func TestMRPipelineStatusValidatesInput(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	if _, err := cli.MRPipelineStatus(context.Background(), 0, "sha"); err == nil {
		t.Fatalf("zero MRIID must error")
	}
	if _, err := cli.MRPipelineStatus(context.Background(), 5, "  "); err == nil {
		t.Fatalf("blank sha must error")
	}
	if len(rt.requests) != 0 {
		t.Fatalf("validation failures must not hit the API, saw %v", rt.requests)
	}
}
