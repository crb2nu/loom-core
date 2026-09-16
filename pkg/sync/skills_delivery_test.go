package sync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/skills"
)

func deliveryManager(t *testing.T) *Manager {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	m := &Manager{RepoRoot: repo, HomeDir: home, WorkspaceRoot: repo,
		Profiles: map[string]*Profile{"gemini": {
			Name: "gemini", RepoDir: ".gemini", HomeDir: ".gemini", SkillsTarget: "gemini",
		}},
	}
	writeDeliveryRegistry(t, m, "pilot")
	return m
}

func writeDeliveryFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeDeliveryRegistry(t *testing.T, m *Manager, names ...string) {
	t.Helper()
	var content strings.Builder
	content.WriteString("version: 1\nskills:")
	if len(names) == 0 {
		content.WriteString(" []")
	}
	content.WriteString("\n")
	for _, name := range names {
		content.WriteString("  - name: " + name + "\n    common:\n      description: Pilot workflow\n      instructions: Run the pilot.\n")
	}
	writeDeliveryFile(t, filepath.Join(m.RepoRoot, "mcp", "context", "skills-registry.yaml"), content.String())
}

func TestSkillsDeliveryPartialFailurePreservesPreviousManifest(t *testing.T) {
	m := deliveryManager(t)
	if err := m.SyncSkills("gemini", false); err != nil {
		t.Fatal(err)
	}
	home := m.ResolveHomePath(m.Profiles["gemini"])
	manifestPath := filepath.Join(home, skills.ManifestFilename)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// The first bundle is deliverable; a file blocks the second bundle's directory.
	writeDeliveryFile(t, filepath.Join(home, "skills", "blocked"), "unmanaged neighbor")
	writeDeliveryRegistry(t, m, "pilot", "added", "blocked")
	if err := m.SyncSkills("gemini", false); err == nil {
		t.Fatal("expected destination copy error")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed sync replaced previous manifest")
	}
	content, err := os.ReadFile(filepath.Join(home, "skills", "blocked"))
	if err != nil || string(content) != "unmanaged neighbor" {
		t.Fatalf("neighbor changed: %q, %v", content, err)
	}
}

func TestSkillsDeliveryPrunesRetiredFilesAndKeepsNeighbors(t *testing.T) {
	for _, removeAll := range []bool{false, true} {
		t.Run(map[bool]string{false: "some", true: "all"}[removeAll], func(t *testing.T) {
			m := deliveryManager(t)
			writeDeliveryRegistry(t, m, "pilot", "retired")
			if err := m.SyncSkills("gemini", false); err != nil {
				t.Fatal(err)
			}
			home := m.ResolveHomePath(m.Profiles["gemini"])
			neighbor := filepath.Join(home, "skills", "retired", "custom.md")
			writeDeliveryFile(t, neighbor, "user notes")
			if removeAll {
				writeDeliveryRegistry(t, m)
			} else {
				writeDeliveryRegistry(t, m, "pilot")
			}
			if err := m.SyncSkills("gemini", false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "skills", "retired", "SKILL.md")); !os.IsNotExist(err) {
				t.Fatalf("retired managed file remains: %v", err)
			}
			if content, err := os.ReadFile(neighbor); err != nil || string(content) != "user notes" {
				t.Fatalf("unmanaged neighbor changed: %q, %v", content, err)
			}
			manifest, err := skills.ReadManifest(home)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if removeAll {
				want = 0
			}
			if manifest == nil || len(manifest.Generated) != want {
				t.Fatalf("unexpected destination manifest: %+v", manifest)
			}
		})
	}
}

func TestSkillsDeliveryInvalidHomeManifestFails(t *testing.T) {
	m := deliveryManager(t)
	home := m.ResolveHomePath(m.Profiles["gemini"])
	manifestPath := filepath.Join(home, skills.ManifestFilename)
	writeDeliveryFile(t, manifestPath, "invalid manifest")
	if err := m.SyncSkills("gemini", false); err == nil {
		t.Fatal("expected destination manifest error")
	}
	if content, err := os.ReadFile(manifestPath); err != nil || string(content) != "invalid manifest" {
		t.Fatalf("invalid destination manifest overwritten: %q, %v", content, err)
	}
}

func TestSkillsDeliveryHomeGenerationFailurePreservesRepoMirror(t *testing.T) {
	m := deliveryManager(t)
	// First generate a managed mirror, then migrate to home delivery with a
	// missing resource. The last usable mirror must survive failed delivery.
	if err := m.SyncSkills("gemini", true); err != nil {
		t.Fatal(err)
	}
	m.Profiles["gemini"].SkillsDirectToHome = true
	registryPath := filepath.Join(m.RepoRoot, "mcp", "context", "skills-registry.yaml")
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliveryFile(t, registryPath, string(registry)+"      references: [missing.md]\n")
	if err := m.SyncSkills("gemini", false); err == nil {
		t.Fatal("expected resource delivery failure")
	}
	if _, err := os.Stat(filepath.Join(m.ResolveRepoPath(m.Profiles["gemini"]), "skills", "pilot", "SKILL.md")); err != nil {
		t.Fatalf("failed home migration removed usable repo mirror: %v", err)
	}
}

func TestSkillsDeliveryHomeCleanupPreservesUnmanagedNeighbors(t *testing.T) {
	m := deliveryManager(t)
	if err := m.SyncSkills("gemini", true); err != nil {
		t.Fatal(err)
	}
	repoPath := m.ResolveRepoPath(m.Profiles["gemini"])
	neighbor := filepath.Join(repoPath, "skills", "pilot", "custom.md")
	writeDeliveryFile(t, neighbor, "user notes")
	m.Profiles["gemini"].SkillsDirectToHome = true
	if err := m.SyncSkills("gemini", false); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "user notes" {
		t.Fatalf("home migration changed unmanaged repo neighbor: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "skills", "pilot", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("managed repo mirror remains after successful migration: %v", err)
	}
}

func TestSkillsDeliveryLegacyCleanupPreservesAmbiguousFiles(t *testing.T) {
	m := deliveryManager(t)
	repoPath := m.ResolveRepoPath(m.Profiles["gemini"])
	file := filepath.Join(repoPath, "skills", "pilot", "SKILL.md")
	writeDeliveryFile(t, file, "potential local edit")
	manifestPath := filepath.Join(repoPath, skills.ManifestFilename)
	before := `{"platform":"gemini","generated":["skills/pilot/SKILL.md"]}`
	writeDeliveryFile(t, manifestPath, before)
	m.Profiles["gemini"].SkillsDirectToHome = true
	if err := m.SyncSkills("gemini", false); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("expected legacy ownership conflict, got %v", err)
	}
	if content, err := os.ReadFile(file); err != nil || string(content) != "potential local edit" {
		t.Fatalf("legacy file changed: %q, %v", content, err)
	}
	if content, err := os.ReadFile(manifestPath); err != nil || string(content) != before {
		t.Fatalf("legacy ownership manifest changed: %q, %v", content, err)
	}
}

func TestSkillsDeliveryEmptyRegistryRetiresHomeWithoutRepoManifest(t *testing.T) {
	m := deliveryManager(t)
	if err := m.SyncSkills("gemini", false); err != nil {
		t.Fatal(err)
	}
	p := m.Profiles["gemini"]
	if err := os.RemoveAll(m.ResolveRepoPath(p)); err != nil {
		t.Fatal(err)
	}
	writeDeliveryRegistry(t, m)
	if err := m.SyncSkills("gemini", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.ResolveHomePath(p), "skills/pilot/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("empty registry left stale home skill after repo mirror was removed: %v", err)
	}
}

func TestSkillsDeliveryCleanupKeepsHomeThroughRepoAlias(t *testing.T) {
	for _, workspaceAlias := range []bool{false, true} {
		t.Run(map[bool]string{false: "repo", true: "workspace"}[workspaceAlias], func(t *testing.T) {
			m := deliveryManager(t)
			p := m.Profiles["gemini"]
			p.SkillsDirectToHome = true
			home := m.ResolveHomePath(p)
			if err := os.MkdirAll(home, 0755); err != nil {
				t.Fatal(err)
			}
			alias := m.ResolveRepoPath(p)
			if workspaceAlias {
				m.WorkspaceRoot = t.TempDir()
				alias = filepath.Join(m.WorkspaceRoot, p.RepoDir)
			}
			if err := os.Symlink(home, alias); err != nil {
				t.Fatal(err)
			}
			if err := m.SyncSkills("gemini", false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "skills/pilot/SKILL.md")); err != nil {
				t.Fatalf("cleanup removed home skill through alias: %v", err)
			}
			if manifest, err := skills.ReadManifest(home); err != nil || manifest == nil || len(manifest.Generated) != 1 {
				t.Fatalf("cleanup removed home manifest through alias: %+v, %v", manifest, err)
			}
		})
	}
}
