package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/finishing"
)

func TestDocsMirrorEndpointBeforeFirstTick(t *testing.T) {
	o := &operator{docsMirror: newDocsMirrorCache()}
	rec := httptest.NewRecorder()
	o.handleDocsMirror(rec, httptest.NewRequest(http.MethodGet, "/api/mills/finishing/docs-mirror", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"unknown"`) || !strings.Contains(rec.Body.String(), `"reason":"not yet computed"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDocsMirrorCacheRefresh(t *testing.T) {
	commitAt := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	now := commitAt.Add(48 * time.Hour)
	c := newDocsMirrorCache()
	c.check = func(context.Context, time.Time) finishing.DocsMirrorDrift {
		return finishing.DocsMirrorDrift{State: finishing.StateDrifted, Missing: 2, Stale: 1, MirrorCommitAt: &commitAt}
	}
	c.refresh(context.Background(), now)
	if got := c.Get(); got.Missing != 2 || got.State != finishing.StateDrifted {
		t.Fatalf("got %+v", got)
	}
	if got := testutil.ToFloat64(mills.FinishingDocsMirrorDriftFiles); got != 3 {
		t.Fatalf("drift gauge=%v", got)
	}
	if got := testutil.ToFloat64(mills.FinishingDocsMirrorAgeSeconds); got != 48*60*60 {
		t.Fatalf("age gauge=%v", got)
	}
}

func TestDocsMirrorConfigDefaultsAndEnv(t *testing.T) {
	c := DefaultConfig()
	if c.DocsMirrorProject != "services/flexinfer-site" || c.DocsMirrorRef != "main" || c.DocsMirrorPath != "content/loom-core-docs" {
		t.Fatalf("defaults %+v", c)
	}
	t.Setenv("LOOM_MILLS_DOCS_MIRROR_PROJECT", "p")
	t.Setenv("LOOM_MILLS_DOCS_MIRROR_REF", "r")
	t.Setenv("LOOM_MILLS_DOCS_MIRROR_PATH", "x")
	c.ApplyEnv()
	if c.DocsMirrorProject != "p" || c.DocsMirrorRef != "r" || c.DocsMirrorPath != "x" {
		t.Fatalf("overrides %+v", c)
	}
}
