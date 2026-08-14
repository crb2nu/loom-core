package mills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestEnqueueBacklogItem_Success: a 201 from the operator round-trips the
// persisted item, and the request carries the right method, path, bearer, and
// body.
func TestEnqueueBacklogItem_Success(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotQuery string
	var gotItem store.BacklogItem

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotItem)

		// Echo back with operator-applied defaults.
		out := gotItem
		out.State = store.BacklogQueued
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	cfg := Config{BaseURL: srv.URL, AdminToken: "sekret"}
	item := store.BacklogItem{
		ID:     "pattern-stamp-widget",
		Title:  "Pattern stamp: pattern-go-rest-service — widget",
		PlanID: "plan-stamp-go-rest-service-widget",
		Labels: []string{"mills-pattern-stamp"},
	}

	created, err := EnqueueBacklogItem(context.Background(), cfg, item, false)
	if err != nil {
		t.Fatalf("EnqueueBacklogItem: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/mills/backlog" {
		t.Errorf("path = %s, want /api/mills/backlog", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty (force=false)", gotQuery)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("auth = %q, want 'Bearer sekret'", gotAuth)
	}
	if gotItem.PlanID != item.PlanID || gotItem.ID != item.ID {
		t.Errorf("operator received id=%q plan=%q, want id=%q plan=%q", gotItem.ID, gotItem.PlanID, item.ID, item.PlanID)
	}
	if len(gotItem.Labels) != 1 || gotItem.Labels[0] != "mills-pattern-stamp" {
		t.Errorf("labels = %v, want [mills-pattern-stamp]", gotItem.Labels)
	}
	if created == nil || created.State != store.BacklogQueued {
		t.Errorf("created state = %v, want queued", created)
	}
}

// TestEnqueueBacklogItem_Force adds ?force=1 to bypass operator dedupe.
func TestEnqueueBacklogItem_Force(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(store.BacklogItem{ID: "x"})
	}))
	defer srv.Close()

	if _, err := EnqueueBacklogItem(context.Background(), Config{BaseURL: srv.URL}, store.BacklogItem{ID: "x", Title: "t"}, true); err != nil {
		t.Fatalf("EnqueueBacklogItem: %v", err)
	}
	if gotQuery != "force=1" {
		t.Errorf("query = %q, want force=1", gotQuery)
	}
}

// TestEnqueueBacklogItem_OperatorRejects folds the operator's error body into
// the returned error so callers can see WHY the item was rejected.
func TestEnqueueBacklogItem_OperatorRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("duplicate in-flight item"))
	}))
	defer srv.Close()

	_, err := EnqueueBacklogItem(context.Background(), Config{BaseURL: srv.URL}, store.BacklogItem{ID: "x", Title: "t"}, false)
	if err == nil {
		t.Fatal("expected an error on non-2xx")
	}
	if !strings.Contains(err.Error(), "duplicate in-flight item") {
		t.Errorf("error = %q, want it to include the operator body", err.Error())
	}
}

// TestEnqueueBacklogItem_NoBaseURL fails fast when the operator isn't wired.
func TestEnqueueBacklogItem_NoBaseURL(t *testing.T) {
	_, err := EnqueueBacklogItem(context.Background(), Config{}, store.BacklogItem{ID: "x"}, false)
	if err == nil {
		t.Fatal("expected an error when BaseURL is empty")
	}
}
