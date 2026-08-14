package killtest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseActivePodNamesFindsEntireSpawnFleet(t *testing.T) {
	raw := `{"items":[
		{"metadata":{"name":"spawn-unlabeled"},"status":{"phase":"Running"}},
		{"metadata":{"name":"api"},"status":{"phase":"Running"}},
		{"metadata":{"name":"spawn-terminal"},"status":{"phase":"Succeeded"}},
		{"metadata":{"name":"owned-nonprefixed","labels":{"app.kubernetes.io/managed-by":"loom-spawn"}},"status":{"phase":"Pending"}},
		{"metadata":{"name":"id-labeled-nonprefixed","labels":{"loom.dev/spawn-id":"abc"}},"status":{"phase":"Unknown"}}
	]}`

	names, err := parseActivePodNames(raw)
	if err != nil {
		t.Fatalf("parseActivePodNames() error = %v", err)
	}
	want := []string{"id-labeled-nonprefixed", "owned-nonprefixed", "spawn-unlabeled"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("parseActivePodNames() = %v, want %v", names, want)
	}
}

func TestParseActivePodNamesPreservesDecodeErrors(t *testing.T) {
	if _, err := parseActivePodNames(`{"items":[`); err == nil {
		t.Fatal("parseActivePodNames() accepted malformed JSON")
	}
}

func TestActiveSpawnPodNamesQueriesAllNamespacePods(t *testing.T) {
	h := New(Config{SpawnNS: "devbox"})
	var command string
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command = strings.Join(args, " ")
		return `{"items":[{"metadata":{"name":"spawn-unlabeled"},"status":{"phase":"Running"}}]}`, nil
	}

	names, err := h.activeSpawnPodNames(context.Background())
	if err != nil {
		t.Fatalf("activeSpawnPodNames() error = %v", err)
	}
	if want := "-n devbox get pods -o json"; command != want {
		t.Fatalf("activeSpawnPodNames() command = %q, want %q", command, want)
	}
	if !reflect.DeepEqual(names, []string{"spawn-unlabeled"}) {
		t.Fatalf("activeSpawnPodNames() = %v", names)
	}
}

func TestActiveSpawnPodNamesPreservesKubectlErrors(t *testing.T) {
	want := errors.New("list denied")
	h := New(Config{SpawnNS: "devbox"})
	h.kubectlFn = func(context.Context, ...string) (string, error) {
		return "", want
	}

	if _, err := h.activeSpawnPodNames(context.Background()); !errors.Is(err, want) {
		t.Fatalf("activeSpawnPodNames() error = %v, want %v", err, want)
	}
}
