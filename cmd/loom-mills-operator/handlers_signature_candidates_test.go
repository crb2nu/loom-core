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

// seedSignatureCandidates writes one recent and one old candidate, plus a
// sweep-failure event under the same actor, so window filtering and kind
// filtering are both observable.
func seedSignatureCandidates(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []*store.Event{
		{
			OccurredAt:  now.Add(-2 * time.Hour),
			Actor:       mills.SignatureMinerActor,
			Kind:        mills.SignatureCandidateEventKind,
			SubjectKind: "signature_phrase",
			SubjectID:   "9f1c2b3a4d5e6f70",
			Payload: map[string]any{
				"phrase":       "fatal knitter sidecar refused sync token for shard",
				"member_count": 4,
				"sample_evidence": []string{
					"fatal: knitter sidecar refused sync token for shard 7 after 21s",
					"fatal: knitter sidecar refused sync token for shard 3 after 9s",
				},
				"first_seen":         now.Add(-90 * time.Hour).Format(time.RFC3339),
				"last_seen":          now.Add(-3 * time.Hour).Format(time.RFC3339),
				"window_match_count": 6,
			},
		},
		{
			OccurredAt:  now.Add(-400 * time.Hour),
			Actor:       mills.SignatureMinerActor,
			Kind:        mills.SignatureCandidateEventKind,
			SubjectKind: "signature_phrase",
			SubjectID:   "0011223344556677",
			Payload:     map[string]any{"phrase": "ancient spooler wedged on lease", "member_count": 3},
		},
		{
			OccurredAt: now.Add(-time.Hour),
			Actor:      mills.SignatureMinerActor,
			Kind:       "reconciler.signature_mining_sweep_failed",
			Payload:    map[string]any{"error": "store closed"},
		},
	} {
		if err := op.store.Events.Append(ctx, e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func getSignatureCandidates(t *testing.T, op *operator, query string) (*httptest.ResponseRecorder, signatureCandidatesResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/signature-candidates"+query, nil))
	var resp signatureCandidatesResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

func TestHandleSignatureCandidatesList_DefaultWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedSignatureCandidates(t, op)

	rec, resp := getSignatureCandidates(t, op, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Window != signatureCandidatesDefaultWindow.String() {
		t.Errorf("window = %q, want %q", resp.Window, signatureCandidatesDefaultWindow.String())
	}
	// The 400h-old candidate is outside 336h; the sweep-failure event shares
	// the actor but not the kind.
	if resp.Count != 1 || len(resp.Candidates) != 1 {
		t.Fatalf("count = %d, candidates = %+v", resp.Count, resp.Candidates)
	}
	got := resp.Candidates[0]
	if got.Fingerprint != "9f1c2b3a4d5e6f70" {
		t.Errorf("fingerprint = %q", got.Fingerprint)
	}
	if got.Phrase != "fatal knitter sidecar refused sync token for shard" {
		t.Errorf("phrase = %q", got.Phrase)
	}
	if got.MemberCount != 4 {
		t.Errorf("member_count = %d, want 4", got.MemberCount)
	}
	if got.WindowMatchCount != 6 {
		t.Errorf("window_match_count = %d, want 6", got.WindowMatchCount)
	}
	if len(got.SampleEvidence) != 2 {
		t.Errorf("sample_evidence = %+v, want 2 snippets", got.SampleEvidence)
	}
	if got.FirstSeen == "" || got.LastSeen == "" {
		t.Errorf("first/last seen = (%q, %q), want RFC3339 stamps", got.FirstSeen, got.LastSeen)
	}
	if got.ProposedAt.IsZero() {
		t.Error("proposed_at not populated")
	}
}

func TestHandleSignatureCandidatesList_WindowParam(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedSignatureCandidates(t, op)

	rec, resp := getSignatureCandidates(t, op, "?window=500h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2 over a 500h window", resp.Count)
	}

	rec, resp = getSignatureCandidates(t, op, "?window=30m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if resp.Count != 0 || len(resp.Candidates) != 0 {
		t.Errorf("count = %d, want 0 over a 30m window (%+v)", resp.Count, resp.Candidates)
	}
}

func TestHandleSignatureCandidatesList_InvalidWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, raw := range []string{"?window=soon", "?window=-1h", "?window=0"} {
		rec, _ := getSignatureCandidates(t, op, raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", raw, rec.Code)
		}
	}
}
