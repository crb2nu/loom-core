package clients

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// NB: fixtures model the REAL producer bytes — GitLab's commits list returns
// `message` with the full body (the revert trailer lives there, not in
// `title`) and `created_at` with a numeric UTC offset rather than "Z".
func TestListBranchCommits_ShapeAndQuery(t *testing.T) {
	var gotQuery string
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/repository/commits": func(r *http.Request) (int, any) {
			gotQuery = r.URL.RawQuery
			return 200, []map[string]any{
				{
					"id":         "ff00000000000000000000000000000000000001",
					"short_id":   "ff000000000",
					"title":      `Revert "feat(mills): thing"`,
					"message":    "Revert \"feat(mills): thing\"\n\nThis reverts commit a1b2c3d4e5f60718293a4b5c6d7e8f9012345678.\n",
					"created_at": "2026-08-04T12:00:00.000+00:00",
					"web_url":    "https://gitlab.example/commit/ff00000000000000000000000000000000000001",
				},
				{
					"id":      "ff00000000000000000000000000000000000002",
					"title":   "feat(mills): thing",
					"message": "feat(mills): thing\n",
				},
			}
		},
	})
	_ = rt

	since := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	commits, err := cli.ListBranchCommits(context.Background(), "main", since, 100)
	if err != nil {
		t.Fatalf("list commits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("want 2 commits, got %d", len(commits))
	}
	if !strings.Contains(commits[0].Message, "This reverts commit ") {
		t.Errorf("message must carry the full body, got %q", commits[0].Message)
	}
	if commits[0].CreatedAt.UTC() != time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) {
		t.Errorf("created_at = %s, want 2026-08-04T12:00:00Z", commits[0].CreatedAt)
	}
	for _, want := range []string{"ref_name=main", "per_page=100", "since=2026-07-28T12%3A00%3A00Z"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestListBranchCommits_RefRequired(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){})
	_ = rt

	if _, err := cli.ListBranchCommits(context.Background(), "  ", time.Time{}, 0); err == nil {
		t.Fatal("want an error for an empty ref")
	}
}

// TestMergedMRLandedSHA pins the preference order a merged list item resolves
// its landed commit with — it must match what the merge path records.
func TestMergedMRLandedSHA(t *testing.T) {
	cases := []struct {
		name string
		item MergeRequestListItem
		want string
	}{
		{"merged commit wins", MergeRequestListItem{MergedCommitSHA: "a", MergeCommitSHA: "b", SquashCommitSHA: "c", SHA: "d"}, "a"},
		{"merge commit next", MergeRequestListItem{MergeCommitSHA: "b", SquashCommitSHA: "c", SHA: "d"}, "b"},
		{"squash commit next", MergeRequestListItem{SquashCommitSHA: "c", SHA: "d"}, "c"},
		{"head sha last", MergeRequestListItem{SHA: "d"}, "d"},
		{"none reported", MergeRequestListItem{}, ""},
		{"whitespace is not an identity", MergeRequestListItem{MergedCommitSHA: "  ", SHA: "d"}, "d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.LandedSHA(); got != tc.want {
				t.Errorf("LandedSHA() = %q, want %q", got, tc.want)
			}
		})
	}
}
