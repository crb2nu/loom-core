package clients

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

var exhaustNow = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func exhaustIssue(iid int64, label, title string, updated time.Time) IssueListItem {
	return IssueListItem{
		IID:       iid,
		Title:     title,
		Labels:    []string{label},
		State:     "opened",
		WebURL:    "https://gitlab.flexinfer.ai/services/loom-core/-/issues/" + strconv.FormatInt(iid, 10),
		CreatedAt: updated.Add(-72 * time.Hour).Format(time.RFC3339),
		UpdatedAt: updated.Format(time.RFC3339),
	}
}

// exhaustStub serves both label queries off the one issues endpoint, branching
// on the `labels` parameter the way GitLab does.
func exhaustStub(t *testing.T, byLabel map[string][]IssueListItem, fail map[string]bool) (*GitLabClient, *recordingTransport) {
	t.Helper()
	return newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(req *http.Request) (int, any) {
			label := req.URL.Query().Get("labels")
			if req.URL.Query().Get("state") != "opened" {
				return 400, map[string]string{"message": "expected state=opened"}
			}
			if fail[label] {
				return 503, map[string]string{"message": "service unavailable"}
			}
			return 200, byLabel[label]
		},
	})
}

func TestListFactoryExhaust_MergesBothLabelsNewestFirst(t *testing.T) {
	cli, rt := exhaustStub(t, map[string][]IssueListItem{
		council.FactoryExhaustFlakyTestLabel: {
			exhaustIssue(601, council.FactoryExhaustFlakyTestLabel, "flake: TestAlpha", exhaustNow.Add(-6*time.Hour)),
			exhaustIssue(602, council.FactoryExhaustFlakyTestLabel, "flake: TestBravo", exhaustNow.Add(-1*time.Hour)),
		},
		council.FactoryExhaustAuditDigestLabel: {
			exhaustIssue(700, council.FactoryExhaustAuditDigestLabel, "Audit advisory digest — 2026-08-04 (UTC)", exhaustNow.Add(-3*time.Hour)),
		},
	}, nil)

	got, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-14*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(got), got)
	}
	// Two label queries, not one: GitLab ANDs `labels`, and no issue carries
	// both.
	if len(rt.requests) != 2 {
		t.Fatalf("made %d requests, want 2 (one per label)", len(rt.requests))
	}
	wantOrder := []int64{602, 700, 601}
	for i, want := range wantOrder {
		if got[i].IID != want {
			t.Fatalf("order = %v, want newest-first %v", []int64{got[0].IID, got[1].IID, got[2].IID}, wantOrder)
		}
	}
	if got[0].Kind != council.FactoryExhaustFlakyTest {
		t.Errorf("kind = %q, want %q", got[0].Kind, council.FactoryExhaustFlakyTest)
	}
	if got[1].Kind != council.FactoryExhaustAuditDigest {
		t.Errorf("kind = %q, want %q", got[1].Kind, council.FactoryExhaustAuditDigest)
	}
	if got[0].UpdatedAt.IsZero() || got[0].CreatedAt.IsZero() {
		t.Errorf("timestamps not decoded: %+v", got[0])
	}
}

func TestListFactoryExhaust_BoundsWindowAndCount(t *testing.T) {
	cli, _ := exhaustStub(t, map[string][]IssueListItem{
		council.FactoryExhaustFlakyTestLabel: {
			exhaustIssue(601, council.FactoryExhaustFlakyTestLabel, "flake: TestFresh", exhaustNow.Add(-1*time.Hour)),
			exhaustIssue(602, council.FactoryExhaustFlakyTestLabel, "flake: TestAlsoFresh", exhaustNow.Add(-2*time.Hour)),
			// Open for months, untouched — real but not this tick's demand.
			exhaustIssue(603, council.FactoryExhaustFlakyTestLabel, "flake: TestStale", exhaustNow.Add(-90*24*time.Hour)),
		},
	}, nil)

	got, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-14*24*time.Hour), 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (limit)", len(got))
	}
	if got[0].IID != 601 {
		t.Errorf("kept #%d, want the newest (#601)", got[0].IID)
	}

	all, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-14*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d items, want 2 — the 90-day-stale issue is outside the window: %+v", len(all), all)
	}
}

// A partial result is worse than none: a council reading "1 open flake" when
// the audit digests were unreachable draws exactly the wrong conclusion, so any
// sub-query failure fails the whole call and the brief states the uncertainty.
func TestListFactoryExhaust_PartialFailureFailsTheCall(t *testing.T) {
	cli, _ := exhaustStub(t, map[string][]IssueListItem{
		council.FactoryExhaustFlakyTestLabel: {
			exhaustIssue(601, council.FactoryExhaustFlakyTestLabel, "flake: TestAlpha", exhaustNow.Add(-1*time.Hour)),
		},
	}, map[string]bool{council.FactoryExhaustAuditDigestLabel: true})

	got, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-14*24*time.Hour), 10)
	if err == nil {
		t.Fatalf("want an error when one label query fails, got %d items", len(got))
	}
	if got != nil {
		t.Errorf("want nil items alongside the error, got %+v", got)
	}
	if !strings.Contains(err.Error(), council.FactoryExhaustAuditDigestLabel) {
		t.Errorf("error %v does not name the failing label", err)
	}
}

func TestListFactoryExhaust_ZeroLimitFetchesNothing(t *testing.T) {
	cli, rt := exhaustStub(t, nil, nil)
	got, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d items, want 0", len(got))
	}
	if len(rt.requests) != 0 {
		t.Errorf("made %d requests, want 0 — a zero cap must not hit GitLab", len(rt.requests))
	}
}

// Unparseable timestamps must not silently exclude an issue: an unknown time is
// "unknown", not "epoch".
func TestListFactoryExhaust_UnparseableTimestampsSurvive(t *testing.T) {
	cli, _ := exhaustStub(t, map[string][]IssueListItem{
		council.FactoryExhaustFlakyTestLabel: {{
			IID: 601, Title: "flake: TestNoTimes", State: "opened",
			Labels: []string{council.FactoryExhaustFlakyTestLabel},
			WebURL: "https://gitlab.flexinfer.ai/services/loom-core/-/issues/601",
		}},
	}, nil)

	got, err := cli.ListFactoryExhaust(context.Background(), exhaustNow.Add(-14*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (timestamp-less issue must not be filtered out)", len(got))
	}
}
