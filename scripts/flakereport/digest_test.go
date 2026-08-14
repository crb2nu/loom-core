package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func markedIssue(iid int, title, testID string, hits int, last string) Issue {
	return Issue{
		IID:    iid,
		Title:  title,
		WebURL: "https://gl/issues/x",
		State:  "opened",
		Description: "body\n\n<!-- loom-flake-dedup: " + testID +
			" hits=" + strconv.Itoa(hits) + " last=" + last + " -->\n",
	}
}

func TestBuildDigestRowsRanksByHits(t *testing.T) {
	issues := []Issue{
		markedIssue(1, "flake: TestQuiet", "pkg/a.TestQuiet", 1, "2026-08-01T00:00:00Z"),
		markedIssue(2, "flake: TestLoud", "pkg/b.TestLoud", 9, "2026-08-02T00:00:00Z"),
		markedIssue(3, "flake: TestMid", "pkg/c.TestMid", 4, "2026-08-03T00:00:00Z"),
	}
	rows := BuildDigestRows(issues)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].TestID != "pkg/b.TestLoud" || rows[0].Hits != 9 {
		t.Errorf("top row = %+v, want the 9-hit entry first", rows[0])
	}
	if rows[2].TestID != "pkg/a.TestQuiet" {
		t.Errorf("bottom row = %+v, want the 1-hit entry last", rows[2])
	}
}

// An issue that a human filed by hand under the label has no marker. It must
// still appear, so the digest reflects the whole label rather than only the
// entries this tool created.
func TestBuildDigestRowsKeepsUnmarkedIssues(t *testing.T) {
	rows := BuildDigestRows([]Issue{
		{IID: 7, Title: "flake: TestHandFiled", State: "opened", Description: "no marker here"},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Hits != 0 {
		t.Errorf("Hits = %d, want 0 for an unmarked issue", rows[0].Hits)
	}
	body := RenderDigest(rows, time.Now().UTC())
	if !strings.Contains(body, "flake: TestHandFiled") {
		t.Error("digest omitted the unmarked issue")
	}
	if !strings.Contains(body, "| ? |") {
		t.Errorf("unmarked hits should render as '?': %q", body)
	}
}

func TestRenderDigestEmpty(t *testing.T) {
	body := RenderDigest(nil, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(body, "No open `flaky-test` issues") {
		t.Errorf("empty digest body = %q", body)
	}
	if !strings.Contains(body, "loom-flake-digest") {
		t.Error("digest body missing its marker")
	}
}

func TestRenderDigestTotalsOccurrences(t *testing.T) {
	rows := BuildDigestRows([]Issue{
		markedIssue(1, "flake: TestA", "pkg/a.TestA", 2, "2026-08-01T00:00:00Z"),
		markedIssue(2, "flake: TestB", "pkg/b.TestB", 5, "2026-08-02T00:00:00Z"),
	})
	body := RenderDigest(rows, time.Now().UTC())
	if !strings.Contains(body, "**2 open flake(s), 7 recorded rerun-pass occurrence(s).**") {
		t.Errorf("digest header did not total the hits: %q", body)
	}
	// Ranked table order.
	if strings.Index(body, "pkg/b.TestB") > strings.Index(body, "pkg/a.TestA") {
		t.Error("digest table is not ranked by hits")
	}
}

func TestRunDigestCreatesThenUpdatesRollingIssue(t *testing.T) {
	fake := newFakeGitLab(t)
	fake.seed(markedIssue(1, "flake: TestA", "pkg/a.TestA", 3, "2026-08-01T00:00:00Z"), FlakeLabel)
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	quiet := func(string, ...any) {}

	if err := RunDigest(context.Background(), gl, now, quiet); err != nil {
		t.Fatalf("RunDigest (first): %v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d issues, want the digest issue", len(fake.created))
	}
	digest := fake.created[0]
	if digest.Title != DigestTitle {
		t.Errorf("digest title = %q, want %q", digest.Title, DigestTitle)
	}
	// The digest issue must NOT carry the flaky-test label, or it would list
	// itself on the next run.
	if lbl := fake.labels[digest.IID]; strings.Contains(lbl, FlakeLabel) {
		t.Errorf("digest issue carries %q (would self-list): %q", FlakeLabel, lbl)
	}
	if !strings.Contains(digest.Description, "pkg/a.TestA") {
		t.Errorf("digest body missing the open flake: %q", digest.Description)
	}

	// Second run: update in place, do not file a second digest issue.
	if err := RunDigest(context.Background(), gl, now.AddDate(0, 0, 7), quiet); err != nil {
		t.Fatalf("RunDigest (second): %v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d digest issues across 2 runs, want 1", len(fake.created))
	}
	if _, ok := fake.updates[digest.IID]; !ok {
		t.Error("digest description was not refreshed on the second run")
	}
	if len(fake.comments[digest.IID]) != 1 {
		t.Errorf("digest snapshot comments = %d, want 1", len(fake.comments[digest.IID]))
	}
}
