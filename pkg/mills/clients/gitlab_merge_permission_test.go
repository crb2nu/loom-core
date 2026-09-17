package clients

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGitLabCheckMergePermission(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		body             any
		allowed, wantErr bool
	}{
		{"allowed", 200, map[string]any{"user": map[string]any{"can_merge": true}}, true, false},
		{"denied", 200, map[string]any{"user": map[string]any{"can_merge": false}}, false, false},
		{"missing user", 200, map[string]any{}, false, true},
		{"missing permission", 200, map[string]any{"user": map[string]any{}}, false, true},
		{"null permission", 200, map[string]any{"user": map[string]any{"can_merge": nil}}, false, true},
		{"invalid permission", 200, map[string]any{"user": map[string]any{"can_merge": "true"}}, false, true},
		{"unauthenticated", 401, map[string]any{}, false, false},
		{"forbidden", 403, map[string]any{}, false, false},
		{"hidden or absent", 404, map[string]any{}, false, true},
		{"rate limited", 429, map[string]any{}, false, true},
		{"upstream failed", 502, map[string]any{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){"GET /api/v4/projects/platform%2Fgitops/merge_requests/718": func(*http.Request) (int, any) { return tc.status, tc.body }})
			allowed, err := client.ForProject("platform/gitops").CheckMergePermission(context.Background(), 718)
			if allowed != tc.allowed || (err != nil) != tc.wantErr {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
			if len(rt.requests) != 1 || rt.requests[0].Method != "GET" || rt.requests[0].Path != "/api/v4/projects/platform%2Fgitops/merge_requests/718" || rt.requests[0].Token != "tok-123" || rt.requests[0].Body != "" {
				t.Fatalf("wrong permission identity/request: %+v", rt.requests)
			}
		})
	}
}

func TestGitLabCheckMergePermission_InvalidAndFailedReads(t *testing.T) {
	client, rt := newGitLabStub(t, nil)
	if allowed, err := client.CheckMergePermission(context.Background(), 0); allowed || err == nil || len(rt.requests) != 0 {
		t.Fatalf("invalid iid: allowed=%v err=%v", allowed, err)
	}
	client.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("invalid JSON")), Header: make(http.Header)}, nil
	}))
	if allowed, err := client.CheckMergePermission(context.Background(), 1); allowed || err == nil {
		t.Fatalf("malformed: allowed=%v err=%v", allowed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }))
	if allowed, err := client.CheckMergePermission(ctx, 1); allowed || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: allowed=%v err=%v", allowed, err)
	}
}
