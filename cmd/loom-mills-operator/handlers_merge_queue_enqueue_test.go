package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleMergeQueueEnqueue_PermissionAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowed   bool
		lookupErr error
		status    int
		outcome   string
	}{
		{"allowed", true, nil, 202, "enqueued"},
		{"denied", false, nil, 403, "forbidden"},
		{"unknown", false, errors.New("upstream detail must not leak"), 503, "unavailable"},
		{"unwired", false, nil, 503, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, cleanup := newTestOperator(t)
			defer cleanup()
			op.policy.Current().MergeQueue.Enabled = true
			if tc.name != "unwired" {
				op.mergeQueuePermission = func(ctx context.Context, project string, iid int64) (bool, error) {
					if project != "platform/gitops" || iid != 718 {
						t.Fatalf("wrong candidate: %s !%d", project, iid)
					}
					return tc.allowed, tc.lookupErr
				}
			}
			body := `{"producer":"mcp_gitlab","idempotency_key":"admission","project":"platform/gitops","mr_iid":718,"source_branch":"fix/test","target_branch":"main","observed_sha":"head"}`
			rec := httptest.NewRecorder()
			op.handleMergeQueueEnqueue(rec, httptest.NewRequest("POST", "/api/mills/merge-queue/enqueue", strings.NewReader(body)))
			var result map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || result["outcome"] != tc.outcome {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !tc.allowed {
				if result["reason"] == "" || strings.Contains(rec.Body.String(), "upstream detail") {
					t.Fatalf("unsafe or missing reason: %s", rec.Body.String())
				}
				for _, table := range []string{"merge_queue", "backlog_items", "pipeline_runs"} {
					var count int
					if err := op.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("%s contains %d rejected rows", table, count)
					}
				}
			}
		})
	}
}
