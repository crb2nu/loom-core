package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The client pages deeper ONLY while the window lacks a terminal pipeline or
// a green Mills-touching one, and follows commit diffs across pages — the two
// blind spots that made a 20-pipeline docs burst (or a >100-file commit) read
// as "no Mills commit" and freeze image lag at a stale or falsely-fresh zero.
func TestGitLabClientPagesUntilMillsGreenAndFollowsDiffPages(t *testing.T) {
	committed := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	requests := map[string]int{}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode: %v", err)
		}
	}
	type listRow struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The project id arrives path-escaped (grp%2Fproj); route on the
		// escaped form so the %2F never splits into path segments.
		path := r.URL.EscapedPath()
		page := r.URL.Query().Get("page")
		switch {
		case strings.HasSuffix(path, "/projects/grp%2Fproj/pipelines"):
			requests["list:"+page]++
			switch page {
			case "1":
				// A full page of docs-only successes: terminal, none touch Mills.
				rows := make([]listRow, 0, pipelinesPerPage)
				for i := 0; i < pipelinesPerPage; i++ {
					rows = append(rows, listRow{ID: int64(100 + i), SHA: fmt.Sprintf("docs%02d", i), Ref: "main", Status: "success"})
				}
				writeJSON(w, rows)
			case "2":
				// Short page holding the Mills-touching green — the client
				// must stop after this page.
				writeJSON(w, []listRow{{ID: 200, SHA: "millssha", Ref: "main", Status: "success"}})
			default:
				t.Errorf("unexpected list page %q", page)
				writeJSON(w, []listRow{})
			}
		case strings.Contains(path, "/pipelines/"):
			requests["detail"]++
			idPart := path[strings.LastIndex(path, "/")+1:]
			id, err := strconv.ParseInt(idPart, 10, 64)
			if err != nil {
				t.Errorf("detail id %q: %v", idPart, err)
			}
			sha := "millssha"
			if id != 200 {
				sha = "docs" + idPart[len(idPart)-2:]
			}
			writeJSON(w, map[string]any{"id": id, "sha": sha, "ref": "main", "status": "success", "finished_at": committed.Format(time.RFC3339)})
		case strings.HasSuffix(path, "/diff"):
			requests["diff:"+page]++
			shaPart := path[strings.LastIndex(strings.TrimSuffix(path, "/diff"), "/")+1 : len(path)-len("/diff")]
			if shaPart != "millssha" {
				writeJSON(w, []map[string]string{{"new_path": "docs/OTHER.md", "old_path": "docs/OTHER.md"}})
				return
			}
			// The Mills path hides on diff page 2 of a >100-file commit.
			if page == "1" {
				rows := make([]map[string]string, 0, diffPerPage)
				for i := 0; i < diffPerPage; i++ {
					rows = append(rows, map[string]string{"new_path": fmt.Sprintf("internal/x/file%03d.go", i)})
				}
				writeJSON(w, rows)
				return
			}
			writeJSON(w, []map[string]string{{"new_path": "pkg/mills/reconciler.go"}})
		case strings.Contains(path, "/repository/commits/"):
			requests["commit"]++
			writeJSON(w, map[string]string{"committed_date": committed.Format(time.RFC3339)})
		default:
			t.Errorf("unexpected request %s", path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &GitLabClient{BaseURL: srv.URL, Token: "t", Project: "grp/proj", HTTP: srv.Client()}
	pipes, err := c.Pipelines(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(pipes) != pipelinesPerPage+1 {
		t.Fatalf("pipelines = %d, want %d (two pages, stopped after the Mills green)", len(pipes), pipelinesPerPage+1)
	}
	var mills *Pipeline
	for i := range pipes {
		if pipes[i].SHA == "millssha" {
			mills = &pipes[i]
		}
	}
	if mills == nil || mills.MillsCommitAt == nil || !mills.MillsCommitAt.Equal(committed) {
		t.Fatalf("mills pipeline not resolved: %+v", mills)
	}
	if requests["list:1"] != 1 || requests["list:2"] != 1 || requests["list:3"] != 0 {
		t.Fatalf("list pages fetched = %v, want exactly pages 1 and 2", requests)
	}
	if requests["diff:2"] != 1 {
		t.Fatalf("diff page 2 never fetched — large commits misread as Mills-free: %v", requests)
	}
}
