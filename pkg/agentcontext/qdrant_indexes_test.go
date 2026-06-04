package agentcontext

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// captureIndexServer records every PUT .../index body so a test can assert
// which (field_name, field_schema) pairs ensureRegisteredIndexes issued.
type indexCall struct {
	field  string
	schema string
}

func captureIndexServer(t *testing.T, calls *[]indexCall) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/index") {
			body, _ := io.ReadAll(r.Body)
			var parsed struct {
				FieldName   string `json:"field_name"`
				FieldSchema string `json:"field_schema"`
			}
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			*calls = append(*calls, indexCall{field: parsed.FieldName, schema: parsed.FieldSchema})
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
	}))
}

// TestEnsureRegisteredIndexes_SessionsDatetime guards the !341 port: the
// sessions collection must register started_at/ended_at as datetime indexes
// (range-filter support for the time-windowed reaper/list) on top of its
// keyword indexes.
func TestEnsureRegisteredIndexes_SessionsDatetime(t *testing.T) {
	t.Parallel()
	var calls []indexCall
	server := captureIndexServer(t, &calls)
	defer server.Close()

	client := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "sess_v1", "Cosine")
	client.SetKind(CollSessions)

	if err := client.ensureRegisteredIndexes(context.Background()); err != nil {
		t.Fatalf("ensureRegisteredIndexes: %v", err)
	}

	keyword := map[string]bool{}
	datetime := map[string]bool{}
	for _, c := range calls {
		switch c.schema {
		case "keyword":
			keyword[c.field] = true
		case "datetime":
			datetime[c.field] = true
		default:
			t.Errorf("unexpected field_schema %q for %q", c.schema, c.field)
		}
	}

	for _, f := range []string{"agent_id", "namespace", "status"} {
		if !keyword[f] {
			t.Errorf("missing keyword index for %q", f)
		}
	}
	for _, f := range []string{"started_at", "ended_at"} {
		if !datetime[f] {
			t.Errorf("missing datetime index for %q", f)
		}
	}
}

// TestEnsureRegisteredIndexes_NonSessionsNoDatetime confirms only the sessions
// kind gets datetime indexes; an unrelated kind issues keyword indexes only.
func TestEnsureRegisteredIndexes_NonSessionsNoDatetime(t *testing.T) {
	t.Parallel()
	var calls []indexCall
	server := captureIndexServer(t, &calls)
	defer server.Close()

	client := NewQdrantClient(httpclient.NewDefault(), server.URL, "", "fc_v1", "Cosine")
	client.SetKind(CollFileClaims)

	if err := client.ensureRegisteredIndexes(context.Background()); err != nil {
		t.Fatalf("ensureRegisteredIndexes: %v", err)
	}

	for _, c := range calls {
		if c.schema == "datetime" {
			t.Errorf("file-claims kind should not register datetime index, got %q", c.field)
		}
	}
	if len(calls) == 0 {
		t.Fatal("expected keyword index calls for file-claims kind")
	}
}

// TestDatetimeIndexesByKind_OnlySessions pins the registry contents so a future
// edit that drops or widens datetime indexing fails loudly.
func TestDatetimeIndexesByKind_OnlySessions(t *testing.T) {
	t.Parallel()
	if len(datetimeIndexesByKind) != 1 {
		t.Fatalf("datetimeIndexesByKind has %d kinds, want 1 (sessions only)", len(datetimeIndexesByKind))
	}
	got := append([]string(nil), datetimeIndexesByKind[CollSessions]...)
	sort.Strings(got)
	want := []string{"ended_at", "started_at"}
	if len(got) != len(want) {
		t.Fatalf("sessions datetime fields = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("sessions datetime fields[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
