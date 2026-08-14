package clients

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestListMergedWork_ProjectsCouncilCorpus covers the adapter that makes
// *GitLabClient a council.MergedWorkSource: the merged listing is scoped by the
// council's own lookback, titles ride through verbatim (normalization belongs
// to textsim so both sides of the comparison agree), a title-less row is
// dropped rather than seeding a corpus entry that matches nothing, and a row
// with a null merged_at falls back to updated_at so the gray band's recency
// gate still has a timestamp to read.
func TestListMergedWork_ProjectsCouncilCorpus(t *testing.T) {
	since := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(r *http.Request) (int, any) {
			q := r.URL.Query()
			if got := q.Get("state"); got != "merged" {
				t.Errorf("state = %q, want merged", got)
			}
			if got := q.Get("updated_after"); got != since.Format(time.RFC3339) {
				t.Errorf("updated_after = %q, want %q", got, since.Format(time.RFC3339))
			}
			if got := q.Get("per_page"); got != "100" {
				t.Errorf("per_page = %q, want 100", got)
			}
			return 200, []map[string]any{
				{
					"iid":        1419,
					"title":      "feat(hud): add embedder Grafana panel and alert — embedder-alerting",
					"state":      "merged",
					"web_url":    "https://gitlab.example/mr/1419",
					"updated_at": "2026-08-02T10:00:00Z",
					"merged_at":  "2026-08-02T09:59:00Z",
				},
				{
					"iid":        1424,
					"title":      "docs(roadmap): tick off overnight deliveries",
					"state":      "merged",
					"updated_at": "2026-08-03T08:00:00Z",
					"merged_at":  nil,
				},
				{"iid": 1425, "title": "   ", "state": "merged"},
			}
		},
	})

	work, err := cli.ListMergedWork(context.Background(), since)
	if err != nil {
		t.Fatalf("list merged work: %v", err)
	}
	if len(work) != 2 {
		t.Fatalf("want 2 entries (title-less row dropped), got %d: %+v", len(work), work)
	}
	if work[0].IID != 1419 || work[0].Title != "feat(hud): add embedder Grafana panel and alert — embedder-alerting" {
		t.Errorf("entry[0] = %+v, want the verbatim merged title", work[0])
	}
	if want := time.Date(2026, 8, 2, 9, 59, 0, 0, time.UTC); !work[0].MergedAt.Equal(want) {
		t.Errorf("entry[0].MergedAt = %v, want %v", work[0].MergedAt, want)
	}
	if want := time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC); !work[1].MergedAt.Equal(want) {
		t.Errorf("entry[1].MergedAt = %v, want the updated_at fallback %v", work[1].MergedAt, want)
	}
}

// TestListMergedWork_NilClientIsInert: the operator leaves the source nil when
// GitLab is unconfigured, and a nil *GitLabClient must answer empty rather than
// panic — grounding is inert without GitLab, never fail-closed.
func TestListMergedWork_NilClientIsInert(t *testing.T) {
	var cli *GitLabClient
	work, err := cli.ListMergedWork(context.Background(), time.Time{})
	if err != nil || work != nil {
		t.Fatalf("nil client: got (%v, %v), want (nil, nil)", work, err)
	}
}
