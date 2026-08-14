package hud

import (
	"testing"
)

// TestApplySpawnGoCache pins the opt-in shared Go cache contract: an empty
// claim (SPAWN_GO_CACHE_PVC unset — the default) leaves the pod env and
// mounts byte-identical to legacy; a named claim mounts it at /gocache and
// points GOCACHE/GOMODCACHE inside it so cold-cache dependency compiles are
// paid once per fleet, not once per 25-minute spawn deadline (2026-07-26:
// 17/73 failed stage-attempts were deadline kills mid `go test` compile).
func TestApplySpawnGoCache(t *testing.T) {
	env := map[string]string{"GOWORK": "off"}
	if mounts := applySpawnGoCache(env, ""); mounts != nil {
		t.Fatalf("empty claim must return nil mounts, got: %#v", mounts)
	}
	if _, ok := env["GOCACHE"]; ok {
		t.Fatal("empty claim must not touch env")
	}
	if len(env) != 1 {
		t.Fatalf("env mutated by disabled cache: %#v", env)
	}

	mounts := applySpawnGoCache(env, "spawn-go-cache")
	if len(mounts) != 1 || mounts[0].ClaimName != "spawn-go-cache" || mounts[0].MountPath != spawnGoCacheMountPath {
		t.Fatalf("unexpected mounts: %#v", mounts)
	}
	if env["GOCACHE"] != spawnGoCacheMountPath+"/go-build" {
		t.Errorf("GOCACHE = %q", env["GOCACHE"])
	}
	if env["GOMODCACHE"] != spawnGoCacheMountPath+"/gomod" {
		t.Errorf("GOMODCACHE = %q", env["GOMODCACHE"])
	}
	// The env map is the same reference passed as StartOpts.Env, so the
	// GOWORK=off module wiring must survive the cache application.
	if env["GOWORK"] != "off" {
		t.Errorf("pre-existing env clobbered: %#v", env)
	}
}

// TestSpawnGoCachePVC_EnvGate pins the env gate: unset/whitespace = disabled.
func TestSpawnGoCachePVC_EnvGate(t *testing.T) {
	t.Setenv("SPAWN_GO_CACHE_PVC", "  ")
	if got := spawnGoCachePVC(); got != "" {
		t.Errorf("whitespace SPAWN_GO_CACHE_PVC = %q, want empty", got)
	}
	t.Setenv("SPAWN_GO_CACHE_PVC", "spawn-go-cache")
	if got := spawnGoCachePVC(); got != "spawn-go-cache" {
		t.Errorf("SPAWN_GO_CACHE_PVC = %q, want spawn-go-cache", got)
	}
}
