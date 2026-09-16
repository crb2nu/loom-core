package mills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyManager_ProtectedPathsReloadKeepsLastKnownGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pipeline:\n  protected_paths: [cmd/loomd/**]\ncross_repo:\n  demand_projects: [services/flexdeck]\n")
	manager, err := NewPolicyManager(context.Background(), path, PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	write("pipeline:\n  protected_paths: [cmd/loomd/**]\n  protected_paths_per_repo:\n    services/flexdeck: [internal/rbac/**]\ncross_repo:\n  demand_projects: [services/flexdeck]\n")
	if err := manager.Reload(); err != nil {
		t.Fatalf("valid reload: %v", err)
	}
	good := manager.Current()
	paths, err := good.ResolveProtectedPaths("flexdeck")
	if err != nil || len(paths) != 1 || paths[0] != "internal/rbac/**" {
		t.Fatalf("published replacement = %v, %v", paths, err)
	}

	write("pipeline:\n  protected_paths_per_repo:\n    flexdeck: ['[']\n")
	if err := manager.Reload(); err == nil {
		t.Fatal("invalid reload unexpectedly succeeded")
	}
	if manager.Current() != good {
		t.Fatal("invalid reload replaced the last-known-good policy")
	}
}
