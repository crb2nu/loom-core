package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestWatchCreateListValidationAndAuthentication(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("watch-secret")
	defer setAdminToken("")

	post := func(body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/mills/watches", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, req)
		return rec
	}
	valid := []string{
		`{"subject_kind":"backlog_item","subject_id":"BL-1","terminal_condition":"merged","ttl_seconds":60}`,
		`{"subject_kind":"pipeline_run","subject_id":"RUN-1","terminal_condition":"done","ttl_seconds":60}`,
	}
	if got := post(valid[0], ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got.Code)
	}
	for _, body := range valid {
		if got := post(body, "watch-secret"); got.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", got.Code, got.Body.String())
		}
	}
	invalid := []string{
		`{"subject_kind":"backlog_item","subject_id":"bad id","terminal_condition":"merged","ttl_seconds":60}`,
		`{"subject_kind":"pipeline_run","subject_id":"RUN-1","terminal_condition":"merged","ttl_seconds":60}`,
		`{"subject_kind":"pipeline_run","subject_id":"RUN-1","terminal_condition":"done","ttl_seconds":0}`,
		`{"subject_kind":"pipeline_run","subject_id":"RUN-1","terminal_condition":"done","ttl_seconds":31536001}`,
	}
	for _, body := range invalid {
		if got := post(body, "watch-secret"); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid create status=%d body=%s", got.Code, got.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/watches?state=active&subject_kind=backlog_item&subject_id=BL-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var watches []*store.Watch
	if err := json.Unmarshal(rec.Body.Bytes(), &watches); err != nil || len(watches) != 1 || watches[0].SubjectID != "BL-1" {
		t.Fatalf("filtered watches=%v err=%v", watches, err)
	}
}

func TestWatchDuplicateResolutionConflicts(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("watch-secret")
	defer setAdminToken("")
	req := httptest.NewRequest(http.MethodPost, "/api/mills/watches", strings.NewReader(`{"subject_kind":"pipeline_run","subject_id":"RUN-1","terminal_condition":"done","ttl_seconds":60}`))
	req.Header.Set("Authorization", "Bearer watch-secret")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	var watch store.Watch
	if err := json.Unmarshal(rec.Body.Bytes(), &watch); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{http.StatusOK, http.StatusConflict} {
		req = httptest.NewRequest(http.MethodPost, "/api/mills/watches/"+watch.ID+"/resolve", nil)
		req.Header.Set("Authorization", "Bearer watch-secret")
		rec = httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("resolution %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}
