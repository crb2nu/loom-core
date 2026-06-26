package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLokiClient_RecentErrorClusters_ClustersAndRanks(t *testing.T) {
	// Canned query_range response: the auth service emits three near-identical
	// error lines that differ only by user id + timestamp (must collapse into
	// ONE cluster of count 3); the proxy emits a single distinct error.
	body := `{"status":"success","data":{"resultType":"streams","result":[
	  {"stream":{"namespace":"logging","app":"auth"},"values":[
	    ["1","2026-06-26T10:00:01Z ERROR token expired for user 12345"],
	    ["2","2026-06-26T10:01:02Z ERROR token expired for user 67890"],
	    ["3","2026-06-26T10:02:03Z ERROR token expired for user 11111"]
	  ]},
	  {"stream":{"namespace":"flexinfer","app":"proxy"},"values":[
	    ["4","2026-06-26T10:00:00Z level=error upstream 0xdeadbeef connection refused"]
	  ]}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/loki/api/v1/query_range") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("query") == "" {
			t.Error("missing query param")
		}
		if r.URL.Query().Get("direction") != "backward" {
			t.Errorf("direction = %q, want backward", r.URL.Query().Get("direction"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewLokiClient(srv.URL, nil)
	if !c.Enabled() {
		t.Fatal("client should be enabled")
	}
	sigs, err := c.RecentErrorClusters(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RecentErrorClusters: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("clusters = %d, want 2 (auth collapsed to 1, proxy 1): %+v", len(sigs), sigs)
	}
	// The auth cluster (3 occurrences) must rank first.
	if sigs[0].Service != "logging/auth" || sigs[0].Count != 3 {
		t.Errorf("top cluster = %+v, want {logging/auth x3}", sigs[0])
	}
	if sigs[0].Source != "loki" {
		t.Errorf("source = %q, want loki", sigs[0].Source)
	}
	if !strings.Contains(sigs[0].Sample, "token expired") {
		t.Errorf("sample = %q, want a representative line", sigs[0].Sample)
	}
	if sigs[1].Service != "flexinfer/proxy" || sigs[1].Count != 1 {
		t.Errorf("second cluster = %+v, want {flexinfer/proxy x1}", sigs[1])
	}
}

func TestLokiClient_Disabled_EmptyURL(t *testing.T) {
	if NewLokiClient("", nil) != nil {
		t.Fatal("empty URL must yield a nil (disabled) client")
	}
	if NewLokiClient("   ", nil) != nil {
		t.Fatal("blank URL must yield a nil client")
	}
}

func TestLokiClient_HTTPError_ReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","error":"loki down"}`))
	}))
	defer srv.Close()
	c := NewLokiClient(srv.URL, nil)
	if _, err := c.RecentErrorClusters(context.Background(), time.Now().Add(-1*time.Hour)); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
}

func TestNormalizeLogSignature_CollapsesVariableData(t *testing.T) {
	a := normalizeLogSignature("2026-06-26T10:00:01Z ERROR token expired for user 12345")
	b := normalizeLogSignature("2026-06-26T11:22:33Z ERROR token expired for user 99999")
	if a != b {
		t.Errorf("signatures should collapse to one cluster:\n a=%q\n b=%q", a, b)
	}
	// A genuinely different message must NOT collapse into the same signature.
	c := normalizeLogSignature("2026-06-26T10:00:01Z ERROR database connection pool exhausted")
	if a == c {
		t.Errorf("distinct messages must not share a signature: %q", a)
	}
}
