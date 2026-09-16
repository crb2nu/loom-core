package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// queue_held_human must count exactly the queued items the reconciler would
// hold for a human hand-off — the item-level flag AND the per-repo override —
// and nothing else: a free item stays out of the count, and a held item that
// is no longer queued is not part of the queue at all.
func TestStatus_QueueHeldHumanCountsEffectivePolicy(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.markReady()
	ctx := context.Background()

	yes := true
	op.policy.Current().Pipeline.PerRepoOverrides = map[string]mills.RepoExecutionOverride{
		"services/flexdeck": {RequireHumanReview: &yes},
	}
	for _, item := range []*store.BacklogItem{
		{ID: "HELD-ITEM", Title: "item flag", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
			Policy: store.ItemPolicy{RequireHumanReview: true}},
		{ID: "HELD-REPO", Title: "repo override", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
			TargetProject: "flexdeck"},
		{ID: "FREE", Title: "autonomous", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test"},
		{ID: "MERGED-HELD", Title: "not queued", State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test",
			Policy: store.ItemPolicy{RequireHumanReview: true}},
	} {
		if err := op.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		QueueDepth     int `json:"queue_depth"`
		QueueHeldHuman int `json:"queue_held_human"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if got.QueueDepth != 3 || got.QueueHeldHuman != 2 {
		t.Fatalf("queue_depth=%d queue_held_human=%d, want 3 and 2: %s", got.QueueDepth, got.QueueHeldHuman, rec.Body.String())
	}
}
