package clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListTreePaginatesAndMapsBlobID(t *testing.T) {
	pages := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Fflexinfer-site/repository/tree": func(r *http.Request) (int, any) {
			pages++
			if r.URL.Query().Get("path") != "content/loom-core-docs" || r.URL.Query().Get("recursive") != "true" || r.URL.Query().Get("ref") != "main" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("query=%s", r.URL.RawQuery)
			}
			if pages == 1 {
				rows := make([]map[string]string, 100)
				for i := range rows {
					rows[i] = map[string]string{"path": fmt.Sprintf("p%d", i), "type": "blob", "id": "sha"}
				}
				return 200, rows
			}
			return 200, []map[string]string{{"path": "last", "type": "blob", "id": "final"}}
		},
	})
	got, err := cli.ListTree(context.Background(), "services/flexinfer-site", "main", "content/loom-core-docs", true)
	if err != nil || pages != 2 || len(got) != 101 || got[100].BlobSHA != "final" {
		t.Fatalf("pages=%d len=%d err=%v", pages, len(got), err)
	}
}

func TestNewestPathCommitQueryAndError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){"GET /api/v4/projects/services%2Fflexinfer-site/repository/commits": func(r *http.Request) (int, any) {
		for _, v := range []string{"path=content%2Floom-core-docs", "ref_name=main", "per_page=1"} {
			if !strings.Contains(r.URL.RawQuery, v) {
				t.Errorf("query %q missing %q", r.URL.RawQuery, v)
			}
		}
		return 200, []map[string]string{{"committed_date": "2026-09-08T12:00:00Z"}}
	}})
	got, err := cli.NewestPathCommit(context.Background(), "services/flexinfer-site", "main", "content/loom-core-docs")
	if err != nil || got == nil || !got.Equal(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
