package main

import (
	"context"
	"encoding/json"
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

func TestHandlePipelineRunRetryAttempts(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedGradeRun(t, op, store.BacklogRunning)
	ctx := context.Background()
	run, err := op.store.Pipeline.GetRun(ctx, "RUN-GRADE-HTTP")
	if err != nil {
		t.Fatal(err)
	}
	run.Attempts = 7
	if err := op.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for i, cls := range []string{"transient", "substrate", "real", "exhausted", ""} {
		art := map[string]any{}
		if cls != "" {
			art["retry_class"] = cls
			art["effective_attempts"] = i
		}
		if err := op.store.Pipeline.PutStage(ctx, &store.StageResult{PipelineRunID: run.ID, Stage: "tests", Attempt: i + 1, Artifacts: art}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/"+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run    map[string]any
		Stages []*store.StageResult
		Gates  []any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Run["Attempts"] != float64(7) || body.Run["EffectiveAttempts"] != nil || body.Gates == nil || len(body.Stages) != 5 {
		t.Fatalf("response: %s", rec.Body.String())
	}
	for i, cls := range []string{"transient", "substrate", "real", "exhausted"} {
		if body.Stages[i].Artifacts["retry_class"] != cls || body.Stages[i].Artifacts["effective_attempts"] != float64(i) {
			t.Fatalf("stage: %+v", body.Stages[i])
		}
	}
	if _, ok := body.Stages[4].Artifacts["effective_attempts"]; ok {
		t.Fatal("legacy count invented")
	}
}

func TestHandlePipelineRunEmptyAttempts(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedGradeRun(t, op, store.BacklogRunning)
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/RUN-GRADE-HTTP", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stages", "gates"} {
		if string(body[key]) != "[]" {
			t.Fatalf("%s = %s, want []", key, body[key])
		}
	}
}
