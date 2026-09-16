package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/webhookbus"
)

func TestGitLabWebhookAuthenticationAndRouting(t *testing.T) {
	b := webhookbus.New(2)
	o := &operator{webhookBus: b, webhookSecret: "hook-secret"}
	h := o.httpMux()

	bad := httptest.NewRequest(http.MethodPost, "/api/mills/hooks/gitlab", strings.NewReader(`{}`))
	bad.Header.Set("X-Gitlab-Token", "wrong")
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d", badRec.Code)
	}

	ch, stop := b.Subscribe("services/loom-core", "abc", 0)
	defer stop()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/hooks/gitlab", strings.NewReader(`{"project":{"path_with_namespace":"services/loom-core"},"object_attributes":{"sha":"abc"}}`))
	req.Header.Set("X-Gitlab-Token", "hook-secret")
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pipeline status=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("pipeline event not published")
	}
}

func TestGitLabWebhookMRUnknownAndMalformed(t *testing.T) {
	b := webhookbus.New(2)
	o := &operator{webhookBus: b, webhookSecret: "s"}
	ch, stop := b.Subscribe("p", "", 7)
	defer stop()

	unknown := httptest.NewRequest(http.MethodPost, "/api/mills/hooks/gitlab", strings.NewReader(`not-json`))
	unknown.Header.Set("X-Gitlab-Token", "s")
	unknown.Header.Set("X-Gitlab-Event", "Job Hook")
	rr := httptest.NewRecorder()
	o.httpMux().ServeHTTP(rr, unknown)
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown status=%d", rr.Code)
	}

	mr := httptest.NewRequest(http.MethodPost, "/api/mills/hooks/gitlab", strings.NewReader(`{"project":{"path_with_namespace":"p"},"object_attributes":{"iid":7,"last_commit":{"id":"head"}}}`))
	mr.Header.Set("X-Gitlab-Token", "s")
	mr.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	rr = httptest.NewRecorder()
	o.httpMux().ServeHTTP(rr, mr)
	if rr.Code != http.StatusOK {
		t.Fatalf("mr status=%d", rr.Code)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("MR event not published")
	}

	malformed := httptest.NewRequest(http.MethodPost, "/api/mills/hooks/gitlab", strings.NewReader(`{`))
	malformed.Header.Set("X-Gitlab-Token", "s")
	malformed.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	rr = httptest.NewRecorder()
	o.httpMux().ServeHTTP(rr, malformed)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d", rr.Code)
	}
}
