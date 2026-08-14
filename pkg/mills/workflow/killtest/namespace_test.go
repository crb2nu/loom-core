package killtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func testNamespaceListJSON(names ...string) string {
	items := make([]string, 0, len(names))
	for index, name := range names {
		items = append(items, fmt.Sprintf(
			`{"metadata":{"name":%q,"uid":"namespace-uid-%d","resourceVersion":%q},"status":{"phase":"Active"}}`,
			name, index+1, fmt.Sprintf("%d", index+10),
		))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func TestValidateActiveNamespaces(t *testing.T) {
	const valid = `{"items":[
		{"metadata":{"name":"loom-mills","uid":"uid-1","resourceVersion":"10"},"status":{"phase":"Active"}},
		{"metadata":{"name":"loom-hub","uid":"uid-2","resourceVersion":"11"},"status":{"phase":"Active"}}
	]}`
	if err := validateActiveNamespaces(valid, "loom-mills", "loom-hub"); err != nil {
		t.Fatalf("validateActiveNamespaces() error = %v", err)
	}

	tests := map[string]struct {
		raw  string
		want string
	}{
		"terminating": {
			raw:  strings.Replace(valid, `"resourceVersion":"10"`, `"resourceVersion":"10","deletionTimestamp":"2026-07-14T00:00:00Z"`, 1),
			want: "not active",
		},
		"terminal phase": {
			raw:  strings.Replace(valid, `"phase":"Active"`, `"phase":"Terminating"`, 1),
			want: "not active",
		},
		"missing uid": {
			raw:  strings.Replace(valid, `"uid":"uid-1",`, "", 1),
			want: "incomplete object identity",
		},
		"missing resource version": {
			raw:  strings.Replace(valid, `,"resourceVersion":"10"`, "", 1),
			want: "incomplete object identity",
		},
		"omitted": {
			raw:  `{"items":[{"metadata":{"name":"loom-mills","uid":"uid-1","resourceVersion":"10"},"status":{"phase":"Active"}}]}`,
			want: "omitted required object",
		},
		"unexpected": {
			raw:  strings.Replace(valid, `"name":"loom-hub"`, `"name":"attacker"`, 1),
			want: "unexpected object",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateActiveNamespaces(test.raw, "loom-mills", "loom-hub")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateActiveNamespaces() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateActiveNamespacesRejectsInvalidConfiguration(t *testing.T) {
	const valid = `{"items":[{"metadata":{"name":"loom-mills","uid":"uid-1","resourceVersion":"10"},"status":{"phase":"Active"}}]}`
	for name, expected := range map[string][]string{
		"none":      nil,
		"empty":     {""},
		"duplicate": {"loom-mills", "loom-mills"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateActiveNamespaces(valid, expected...); err == nil {
				t.Fatal("validateActiveNamespaces() error = nil")
			}
		})
	}
}

func TestPreflightRejectsTerminatingNamespaceBeforeOtherReads(t *testing.T) {
	raw := testNamespaceListJSON(s1cOperatorNamespace, "loom-hub", s1cSpawnNamespace, "monitoring")
	raw = strings.Replace(raw, `"resourceVersion":"10"`,
		`"resourceVersion":"10","deletionTimestamp":"2026-07-14T00:00:00Z"`, 1)
	h := New(Config{})
	calls := 0
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		calls++
		if !strings.Contains(strings.Join(args, " "), "get ns") {
			return "", fmt.Errorf("unexpected call: %v", args)
		}
		return raw, nil
	}

	_, err := h.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("Preflight() error = %v, want terminating namespace rejection", err)
	}
	if calls != 1 {
		t.Fatalf("Preflight() calls = %d, want namespace read only", calls)
	}
}
