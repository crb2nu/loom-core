package clients

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type hookRoundTripper func(*http.Request) (*http.Response, error)

func (f hookRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnsureMillsWebhookCreateUpdateAndUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, list, wantMethod string }{
		{"create", `[]`, http.MethodPost},
		{"update", `[{"id":9,"url":"https://mills.example/api/mills/hooks/gitlab","pipeline_events":false,"merge_requests_events":true}]`, http.MethodPut},
		{"matching hook reapplies secret", `[{"id":9,"url":"https://mills.example/api/mills/hooks/gitlab","pipeline_events":true,"merge_requests_events":true}]`, http.MethodPut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewGitLabClient(GitLabConfig{APIURL: "https://gitlab.example/api/v4", Token: "api", Project: "services/loom-core"})
			if err != nil {
				t.Fatal(err)
			}
			var mutation string
			client.SetTransport(hookRoundTripper(func(r *http.Request) (*http.Response, error) {
				body := tc.list
				if r.Method == http.MethodGet && r.URL.RawQuery != "per_page=100" {
					t.Fatalf("list query=%q want per_page=100", r.URL.RawQuery)
				}
				if r.Method != http.MethodGet {
					mutation = r.Method
					var got projectHookRequest
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					if got.URL != "https://mills.example/api/mills/hooks/gitlab" || got.Token != "secret" || !got.PipelineEvents || !got.MergeRequestEvents {
						t.Fatalf("body=%+v", got)
					}
					body = `{}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			}))
			if err := client.EnsureMillsWebhook(context.Background(), "https://mills.example/", "secret"); err != nil {
				t.Fatal(err)
			}
			if mutation != tc.wantMethod {
				t.Fatalf("mutation=%q want %q", mutation, tc.wantMethod)
			}
		})
	}
}

func TestEnsureMillsWebhookMissingConfigMakesNoCall(t *testing.T) {
	client, _ := NewGitLabClient(GitLabConfig{APIURL: "https://gitlab.example/api/v4", Token: "api", Project: "47"})
	called := false
	client.SetTransport(hookRoundTripper(func(r *http.Request) (*http.Response, error) { called = true; return nil, nil }))
	if err := client.EnsureMillsWebhook(context.Background(), "", "secret"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unexpected GitLab call")
	}
}
