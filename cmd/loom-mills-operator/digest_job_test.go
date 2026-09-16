package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/finishing/digest"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type digestRunResponse struct {
	Digest        digest.Digest `json:"digest"`
	Inserted      bool          `json:"inserted"`
	DeliveryError string        `json:"delivery_error"`
}

func postDigestRun(t *testing.T, op *operator, path string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer secret-abc")
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	return rec
}

func TestFinishingDigestAppendOnceHooksAndRead(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	op.shiftNow = func() time.Time { return day.AddDate(0, 0, 1).Add(6 * time.Hour) }
	digests, pushes := 0, 0
	op.reconciler = &mills.Reconciler{
		DigestEvent: func(_ context.Context, e store.Event) error {
			if e.Kind != finishingDigestKind || e.SubjectKind != finishingDigestKind || e.SubjectID != "2026-09-10" || e.Payload["day"] != "2026-09-10" {
				t.Errorf("digest hook event = %+v", e)
			}
			digests++
			return nil
		},
		MobilePushEvent: func(_ context.Context, e store.Event) error {
			if e.Kind != finishingDigestKind || e.SubjectID != "2026-09-10" {
				t.Errorf("mobile push hook event = %+v", e)
			}
			pushes++
			return nil
		},
	}
	// Two invocations for the same day: one stored row, one delivery per hook.
	for i, wantInserted := range []bool{true, false} {
		d, inserted, err := op.runFinishingDigest(context.Background(), day)
		if err != nil || inserted != wantInserted || d.Day != "2026-09-10" {
			t.Fatalf("run %d: digest=%+v inserted=%v err=%v", i, d, inserted, err)
		}
	}
	if digests != 1 || pushes != 1 {
		t.Fatalf("hooks digest=%d push=%d: exactly one delivery to each hook across two invocations", digests, pushes)
	}
	events, err := op.store.Events.ListSinceByKinds(context.Background(), []string{finishingDigestKind}, day, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	if !events[0].OccurredAt.Equal(day.AddDate(0, 0, 1)) {
		t.Fatalf("digest occurred_at = %s, want the day boundary", events[0].OccurredAt)
	}
	for _, path := range []string{"/api/mills/finishing/digest?day=2026-09-10", "/api/mills/finishing/digest"} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		var got digest.Digest
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Day != "2026-09-10" || got.Markdown != "No finished goods for 2026-09-10." || got.Summary != got.Markdown {
			t.Fatalf("%s digest=%+v err=%v", path, got, err)
		}
	}
}

func TestFinishingDigestReadRejectsMalformedAndMissing(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	for path, want := range map[string]int{"/api/mills/finishing/digest?day=nope": http.StatusBadRequest, "/api/mills/finishing/digest?day=2026-09-10": http.StatusNotFound, "/api/mills/finishing/digest": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s=%d want %d", path, rec.Code, want)
		}
	}
}

func TestFinishingDigestRunEndpoint(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.shiftNow = func() time.Time { return time.Date(2026, 9, 11, 6, 30, 0, 0, time.UTC) }
	setAdminToken("secret-abc")
	defer setAdminToken("")
	if rec := postDigestRun(t, op, "/api/mills/finishing/digest/run?day=2026-09-10", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rerun status=%d", rec.Code)
	}
	for _, path := range []string{
		"/api/mills/finishing/digest/run?day=nope",
		"/api/mills/finishing/digest/run?day=2026-09-11", // today has not ended: a partial ledger must never become the day's digest
	} {
		if rec := postDigestRun(t, op, path, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d want 400 body=%s", path, rec.Code, rec.Body.String())
		}
	}
	for i, wantInserted := range []bool{true, false} {
		rec := postDigestRun(t, op, "/api/mills/finishing/digest/run", true) // defaults to yesterday
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		var got digestRunResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Inserted != wantInserted || got.Digest.Day != "2026-09-10" || got.DeliveryError != "" {
			t.Fatalf("run %d resp=%+v err=%v", i, got, err)
		}
	}
}

func TestFinishingDigestHookFailureKeepsStoredDigest(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.shiftNow = func() time.Time { return time.Date(2026, 9, 11, 6, 30, 0, 0, time.UTC) }
	hooks := 0
	op.reconciler = &mills.Reconciler{DigestEvent: func(context.Context, store.Event) error { hooks++; return errors.New("hud down") }}
	setAdminToken("secret-abc")
	defer setAdminToken("")
	rec := postDigestRun(t, op, "/api/mills/finishing/digest/run?day=2026-09-10", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got digestRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.Inserted || got.Digest.Day != "2026-09-10" || got.DeliveryError == "" {
		t.Fatalf("resp=%+v err=%v", got, err)
	}
	if _, err := op.store.Events.FirstBySubjectKind(context.Background(), finishingDigestKind, "2026-09-10", finishingDigestKind); err != nil {
		t.Fatalf("stored digest missing after hook failure: %v", err)
	}
	// Append-once means the failed delivery is not replayed by a rerun.
	rec = postDigestRun(t, op, "/api/mills/finishing/digest/run?day=2026-09-10", true)
	var again digestRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &again); rec.Code != http.StatusOK || err != nil || again.Inserted || again.DeliveryError != "" || again.Digest.Day != "2026-09-10" || hooks != 1 {
		t.Fatalf("rerun status=%d resp=%+v err=%v hooks=%d", rec.Code, again, err, hooks)
	}
}

func TestDigestSchedulerNeverComposesAtBoot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	// Past today's slot, where a catch-up-at-boot design would compose
	// yesterday. The spec routes a missed day through the admin rerun, so
	// the scheduler's first compose is the next slot, never the boot path.
	op.shiftNow = func() time.Time { return time.Date(2026, 9, 11, 6, 30, 0, 0, time.UTC) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- op.runDigestScheduler(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := op.store.Events.LatestByKind(context.Background(), finishingDigestKind); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("scheduler composed on the boot path: err=%v", err)
	}
}
