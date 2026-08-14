package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedRegressionEvents writes one recent and one old attribution, plus an
// unrelated event under the same actor, so window filtering and kind filtering
// are both observable.
func seedRegressionEvents(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []*store.Event{
		{
			OccurredAt:  now.Add(-2 * time.Hour),
			Actor:       mills.RegressionAttributionActor,
			Kind:        mills.RegressionAttributedEventKind,
			SubjectKind: "merge_request",
			SubjectID:   "1421",
			Payload: map[string]any{
				"regressed_mr_iid": 1421,
				"merged_sha":       "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
				"revert_sha":       "ff00000000000000000000000000000000000001",
				"revert_title":     `Revert "feat(mills): thing"`,
			},
		},
		{
			OccurredAt:  now.Add(-400 * time.Hour),
			Actor:       mills.RegressionAttributionActor,
			Kind:        mills.RegressionAttributedEventKind,
			SubjectKind: "merge_request",
			SubjectID:   "900",
			Payload:     map[string]any{"regressed_mr_iid": 900},
		},
		{
			OccurredAt: now.Add(-time.Hour),
			Actor:      mills.RegressionAttributionActor,
			Kind:       "reconciler.regression_sweep_failed",
			Payload:    map[string]any{"error": "gitlab 502"},
		},
	} {
		if err := op.store.Events.Append(ctx, e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func getRegressions(t *testing.T, op *operator, query string) (*httptest.ResponseRecorder, regressionsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/regressions"+query, nil))
	var resp regressionsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

func TestHandleRegressionsList_DefaultWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedRegressionEvents(t, op)

	rec, resp := getRegressions(t, op, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Window != regressionsDefaultWindow.String() {
		t.Errorf("window = %q, want %q", resp.Window, regressionsDefaultWindow.String())
	}
	// The 400h-old attribution is outside 336h; the sweep-failure event shares
	// the actor but not the kind.
	if resp.Count != 1 || len(resp.Regressions) != 1 {
		t.Fatalf("count = %d, regressions = %+v", resp.Count, resp.Regressions)
	}
	got := resp.Regressions[0]
	if got.RegressedMRIID != 1421 {
		t.Errorf("regressed_mr_iid = %d, want 1421", got.RegressedMRIID)
	}
	if got.MergedSHA != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Errorf("merged_sha = %q", got.MergedSHA)
	}
	if got.RevertSHA != "ff00000000000000000000000000000000000001" {
		t.Errorf("revert_sha = %q", got.RevertSHA)
	}
	if got.RevertTitle != `Revert "feat(mills): thing"` {
		t.Errorf("revert_title = %q", got.RevertTitle)
	}
	if got.AttributedAt.IsZero() {
		t.Error("attributed_at not populated")
	}
}

func TestHandleRegressionsList_WindowParam(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedRegressionEvents(t, op)

	// Widened past the old attribution.
	rec, resp := getRegressions(t, op, "?window=500h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2 over a 500h window", resp.Count)
	}

	// Narrowed inside the recent attribution.
	rec, resp = getRegressions(t, op, "?window=30m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Count != 0 || len(resp.Regressions) != 0 {
		t.Errorf("count = %d, want 0 over a 30m window (%+v)", resp.Count, resp.Regressions)
	}
}

func TestHandleRegressionsList_InvalidWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, raw := range []string{"?window=soon", "?window=-1h", "?window=0"} {
		rec, _ := getRegressions(t, op, raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", raw, rec.Code)
		}
	}
}
