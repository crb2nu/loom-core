package sync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/crb2nu/loom/pkg/skills"
)

func profileDeliveryManager(t *testing.T, generatedOnly bool) *Manager {
	t.Helper()
	m := deliveryManager(t)
	p := m.Profiles["gemini"]
	p.Name = "fixture"
	p.SkillsManifest = skills.ManifestFilename
	p.SyncGeneratedOnly = generatedOnly
	p.GeneratedFile = "config.json"
	m.Profiles = map[string]*Profile{"fixture": p}
	writeDeliveryFile(t, filepath.Join(m.ResolveRepoPath(p), p.GeneratedFile), "{}")
	return m
}

func syncDeliveryProfile(m *Manager) error {
	return m.SyncToHome("fixture", false, false, false, false, "", false, "", false)
}

func TestSkillsProfileDeliveryFailurePreservesManifest(t *testing.T) {
	for _, generatedOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory-copy", true: "generated-only"}[generatedOnly], func(t *testing.T) {
			m := profileDeliveryManager(t, generatedOnly)
			p := m.Profiles["fixture"]
			if err := m.regenerateSkills(p); err != nil {
				t.Fatal(err)
			}
			if err := syncDeliveryProfile(m); err != nil {
				t.Fatal(err)
			}
			home := m.ResolveHomePath(p)
			manifestPath := filepath.Join(home, skills.ManifestFilename)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			writeDeliveryFile(t, filepath.Join(home, "skills", "blocked"), "user file")
			writeDeliveryRegistry(t, m, "pilot", "added", "blocked")
			if err := m.regenerateSkills(p); err != nil {
				t.Fatal(err)
			}
			if err := syncDeliveryProfile(m); err == nil {
				t.Fatal("expected skill delivery failure")
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("profile sync published manifest before delivery completed")
			}
		})
	}
}

func TestSkillsProfileDeliveryRetiresManagedFiles(t *testing.T) {
	m := profileDeliveryManager(t, false)
	p := m.Profiles["fixture"]
	if err := m.regenerateSkills(p); err != nil {
		t.Fatal(err)
	}
	if err := syncDeliveryProfile(m); err != nil {
		t.Fatal(err)
	}
	home := m.ResolveHomePath(p)
	neighbor := filepath.Join(home, "skills", "pilot", "custom.md")
	writeDeliveryFile(t, neighbor, "user notes")
	writeDeliveryRegistry(t, m)
	if err := m.regenerateSkills(p); err != nil {
		t.Fatal(err)
	}
	if err := syncDeliveryProfile(m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "pilot", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("retired file remains: %v", err)
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "user notes" {
		t.Fatalf("user neighbor changed: %q, %v", content, err)
	}
}
