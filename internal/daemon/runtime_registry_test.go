package daemon

import (
	"os"
	"testing"

	"github.com/crb2nu/loom/pkg/registry"
)

func TestRuntimeRegistryForTarget_NormalizesTargetSpecificSpecs(t *testing.T) {
	reg := &registry.Registry{
		Version: 1,
		Servers: []*registry.Server{
			{
				Name:       "agent_context",
				Categories: []string{"memory"},
				Common: &registry.TargetSpec{
					Command: "base-cmd",
					Args:    []any{"serve"},
					Env: map[string]string{
						"COMMON_ONLY": "1",
						"SHARED":      "common",
					},
					AlwaysAllow: []string{"agent_context"},
				},
				Targets: map[string]*registry.TargetSpec{
					"codex": {
						Command: "codex-cmd",
						Env: map[string]string{
							"TARGET_ONLY": "1",
							"SHARED":      "target",
						},
					},
				},
			},
		},
	}

	normalized, err := runtimeRegistryForTarget(reg, "codex")
	if err != nil {
		t.Fatalf("runtimeRegistryForTarget() error = %v", err)
	}

	if len(normalized.Servers) != 1 {
		t.Fatalf("normalized server count = %d, want 1", len(normalized.Servers))
	}
	server := normalized.Servers[0]
	if server.Targets != nil {
		t.Fatalf("normalized Targets should be nil, got %#v", server.Targets)
	}
	if server.Common == nil {
		t.Fatal("normalized Common spec is nil")
	}
	if got, want := server.Common.Command, "codex-cmd"; got != want {
		t.Fatalf("normalized command = %q, want %q", got, want)
	}
	if got, want := server.Common.Env["COMMON_ONLY"], "1"; got != want {
		t.Fatalf("COMMON_ONLY = %q, want %q", got, want)
	}
	if got, want := server.Common.Env["TARGET_ONLY"], "1"; got != want {
		t.Fatalf("TARGET_ONLY = %q, want %q", got, want)
	}
	if got, want := server.Common.Env["SHARED"], "target"; got != want {
		t.Fatalf("SHARED = %q, want %q", got, want)
	}

	spec, err := normalized.GetServerSpec("agent_context", "codex")
	if err != nil {
		t.Fatalf("normalized GetServerSpec() error = %v", err)
	}
	if got, want := spec.Env["SHARED"], "target"; got != want {
		t.Fatalf("normalized spec SHARED = %q, want %q", got, want)
	}
}

func TestRuntimeRegistryForTarget_ClonesCommonEnv(t *testing.T) {
	reg := &registry.Registry{
		Version: 1,
		Servers: []*registry.Server{
			{
				Name: "gitlab",
				Common: &registry.TargetSpec{
					Env: map[string]string{
						"TOKEN": "abc",
					},
				},
			},
		},
	}

	normalized, err := runtimeRegistryForTarget(reg, "codex")
	if err != nil {
		t.Fatalf("runtimeRegistryForTarget() error = %v", err)
	}

	normalized.Servers[0].Common.Env["TOKEN"] = "changed"
	if got, want := reg.Servers[0].Common.Env["TOKEN"], "abc"; got != want {
		t.Fatalf("original registry env mutated to %q, want %q", got, want)
	}
}

func TestApplyCatalogState_NoStateFileLeavesRegistryUnfiltered(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no catalog-state.yaml under this home

	reg := &registry.Registry{
		Servers: []*registry.Server{{Name: "tavily"}, {Name: "icc"}},
	}
	got := applyCatalogState(reg, nil)
	if len(got.Servers) != 2 {
		t.Fatalf("servers = %d, want 2 (absent state file means all enabled)", len(got.Servers))
	}
}

func TestApplyCatalogState_DropsDisabledServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := home + "/.config/loom"
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := "disabled_servers:\n    - icc\n    - grafana\n"
	if err := os.WriteFile(stateDir+"/catalog-state.yaml", []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{
		Servers: []*registry.Server{{Name: "tavily"}, {Name: "icc"}, {Name: "grafana"}, {Name: "gitlab"}},
	}
	got := applyCatalogState(reg, nil)
	names := make([]string, 0, len(got.Servers))
	for _, s := range got.Servers {
		names = append(names, s.Name)
	}
	want := []string{"tavily", "gitlab"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("servers = %v, want %v", names, want)
	}
}

func TestApplyCatalogState_NilRegistry(t *testing.T) {
	if got := applyCatalogState(nil, nil); got != nil {
		t.Fatalf("nil registry should pass through, got %v", got)
	}
}
