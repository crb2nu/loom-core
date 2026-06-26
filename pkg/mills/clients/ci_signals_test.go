package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitLabClient_RecentErrorClusters_ClustersFailedPipelines(t *testing.T) {
	// Two failed pipelines on feat/x (must collapse to one cluster of 2) and one
	// on main.
	body := `[
	  {"id":1,"ref":"feat/x","status":"failed","source":"push","web_url":"https://gl/p/1"},
	  {"id":2,"ref":"feat/x","status":"failed","source":"push","web_url":"https://gl/p/2"},
	  {"id":3,"ref":"main","status":"failed","source":"schedule","web_url":"https://gl/p/3"}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pipelines") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("status") != "failed" {
			t.Errorf("status = %q, want failed", r.URL.Query().Get("status"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := NewGitLabClient(GitLabConfig{APIURL: srv.URL, Token: "t", Project: "services/loom-core"})
	if err != nil {
		t.Fatalf("NewGitLabClient: %v", err)
	}
	sigs, err := c.RecentErrorClusters(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RecentErrorClusters: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("clusters = %d, want 2 (feat/x collapsed, main single): %+v", len(sigs), sigs)
	}
	if sigs[0].Service != "ci/feat/x" || sigs[0].Count != 2 {
		t.Errorf("top cluster = %+v, want {ci/feat/x x2}", sigs[0])
	}
	if sigs[0].Source != "gitlab-ci" {
		t.Errorf("source = %q, want gitlab-ci", sigs[0].Source)
	}
	if !strings.Contains(sigs[0].Sample, "2 failed") || !strings.Contains(sigs[0].Sample, "source=push") {
		t.Errorf("sample = %q, want count + source", sigs[0].Sample)
	}
	if sigs[1].Service != "ci/main" || sigs[1].Count != 1 {
		t.Errorf("second cluster = %+v, want {ci/main x1}", sigs[1])
	}
}

func TestGitLabClient_RecentErrorClusters_NilSafe(t *testing.T) {
	var c *GitLabClient
	sigs, err := c.RecentErrorClusters(context.Background(), time.Now())
	if err != nil || sigs != nil {
		t.Fatalf("nil receiver: sigs=%v err=%v, want nil/nil", sigs, err)
	}
}
