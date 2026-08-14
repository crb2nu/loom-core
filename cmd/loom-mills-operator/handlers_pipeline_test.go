package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func seedGradeRun(t *testing.T, op *operator, state store.BacklogState) {
	t.Helper()
	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{ID: "BL-GRADE-HTTP", Title: "taste", State: state, Priority: store.P2, CreatedBy: "test", PlanID: "PLAN-HTTP"}); err != nil {
		t.Fatal(err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: "RUN-GRADE-HTTP", BacklogID: "BL-GRADE-HTTP", Template: "test", State: store.PipelineDone, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func postGrade(op *operator, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/RUN-GRADE-HTTP/grade", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	return rec
}

func TestHandlePipelineGrade_AuthValidationAndRegrade(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("grade-secret")
	defer setAdminToken("")
	seedGradeRun(t, op, store.BacklogMerged)
	if rec := postGrade(op, `{"grade":"keep"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth = %d", rec.Code)
	}
	if rec := postGrade(op, `{"grade":"keep"}`, "wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong auth = %d", rec.Code)
	}
	if rec := postGrade(op, `{"grade":"love"}`, "grade-secret"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid grade = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postGrade(op, `{"grade":"keep","note":"line one\nline two"}`, "grade-secret"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("multiline note = %d: %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{"grade":"keep","note":"good"}`, `{"grade":"meh","note":"mixed"}`} {
		if rec := postGrade(op, body, "grade-secret"); rec.Code != http.StatusOK {
			t.Fatalf("grade = %d: %s", rec.Code, rec.Body.String())
		}
	}
	item, err := op.store.Backlog.Get(context.Background(), "BL-GRADE-HTTP")
	if err != nil || item.Grade != "meh" {
		t.Fatalf("grade head = %+v, %v", item, err)
	}
	events, err := op.store.Events.ListBySubject(context.Background(), "pipeline_run", "RUN-GRADE-HTTP", 10)
	if err != nil || len(events) != 2 || events[0].Payload["prior_grade"] != "keep" {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestHandlePipelineGrade_RejectsNonTerminalItem(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("grade-secret")
	defer setAdminToken("")
	seedGradeRun(t, op, store.BacklogRunning)
	if rec := postGrade(op, `{"grade":"keep"}`, "grade-secret"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-terminal = %d: %s", rec.Code, rec.Body.String())
	}
}
