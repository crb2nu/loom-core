package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deliveryGenerator(t *testing.T, target string) *Generator {
	t.Helper()
	root := t.TempDir()
	output := filepath.Join(root, "output")
	g := &Generator{
		Registry: &Registry{Skills: []*Skill{{
			Name:    "pilot",
			Common:  &SkillSpec{Description: "Pilot workflow", Instructions: "Run the pilot."},
			Targets: map[string]*TargetSpec{target: {Type: "skill"}},
		}}},
		SourceDir: filepath.Join(root, "source"),
		Target:    target, OutputDir: output, CodexRootDir: output,
	}
	if target == "codex" {
		g.OutputDir = filepath.Join(output, "skills")
	}
	return g
}

func TestDeliveryMissingBundleResourceFailsWithoutPublishingManifest(t *testing.T) {
	for _, target := range []string{"codex", "claude", "gemini", "antigravity", "zed", "opencode"} {
		t.Run(target, func(t *testing.T) {
			for _, kind := range []string{"script", "reference", "asset"} {
				t.Run(kind, func(t *testing.T) {
					g := deliveryGenerator(t, target)
					if err := g.generateForTarget(target); err != nil {
						t.Fatal(err)
					}
					manifestPath := filepath.Join(g.resolveTargetDir(target), ManifestFilename)
					before, err := os.ReadFile(manifestPath)
					if err != nil {
						t.Fatal(err)
					}
					common := g.Registry.Skills[0].Common
					switch kind {
					case "script":
						common.Scripts = []*Script{{Name: "missing", Path: "scripts/missing.sh"}}
					case "reference":
						common.References = []string{"missing.md"}
					case "asset":
						common.Assets = []string{"missing.json"}
					}
					if err := g.generateForTarget(target); err == nil || !strings.Contains(err.Error(), "missing") {
						t.Fatalf("expected actionable missing resource error, got %v", err)
					}
					after, err := os.ReadFile(manifestPath)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) {
						t.Fatal("failed delivery replaced previous manifest")
					}
				})
			}
		})
	}
}

func TestDeliveryMalformedPreviousManifestStopsBeforeGeneration(t *testing.T) {
	g := deliveryGenerator(t, "gemini")
	manifestPath := filepath.Join(g.OutputDir, ManifestFilename)
	writePruneFile(t, manifestPath, "invalid manifest")
	if err := g.generateForTarget("gemini"); err == nil {
		t.Fatal("expected error reading previous manifest")
	}
	if _, err := os.Stat(filepath.Join(g.OutputDir, "skills", "pilot", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("generation modified files before reading previous manifest: %v", err)
	}
}

func TestDeliveryManifestWriteFailureIsReturned(t *testing.T) {
	g := deliveryGenerator(t, "gemini")
	// A regular file can be read as the previous manifest, but cannot be
	// replaced when the platform root has no write permission.
	if err := g.generateForTarget("gemini"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(g.OutputDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(g.OutputDir, 0755) })
	probe, err := os.CreateTemp(g.OutputDir, "write-probe")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("filesystem does not enforce directory permissions")
	}
	if err := g.generateForTarget("gemini"); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("expected manifest publication error, got %v", err)
	}
}

func TestDeliveryRetirementPreservesModifiedManagedFile(t *testing.T) {
	g := deliveryGenerator(t, "gemini")
	if err := g.generateForTarget("gemini"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(g.OutputDir, "skills", "pilot", "SKILL.md")
	writePruneFile(t, file, "local customization")
	g.Registry.Skills = nil
	if err := g.generateForTarget("gemini"); err == nil {
		t.Fatal("expected modified managed file conflict")
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "local customization" {
		t.Fatalf("modified file was not preserved: %q, %v", content, err)
	}
}

func TestDeliveryManifestRejectsMissingFile(t *testing.T) {
	if err := WriteManifest(t.TempDir(), "gemini", []string{"skills/missing/SKILL.md"}); err == nil {
		t.Fatal("manifest accepted an undelivered file")
	}
}

func TestDeliveryNestedResourcesMatchManifest(t *testing.T) {
	for _, target := range []string{"codex", "claude", "gemini", "antigravity", "zed", "opencode"} {
		t.Run(target, func(t *testing.T) {
			g := deliveryGenerator(t, target)
			common := g.Registry.Skills[0].Common
			common.Scripts = []*Script{{Name: "run", Path: "scripts/nested/run.sh"}}
			common.References = []string{"nested/guide.md"}
			common.Assets = []string{"templates/nested/config.json"}
			resources := []string{"scripts/nested/run.sh", "references/nested/guide.md", "assets/templates/nested/config.json"}
			for _, rel := range resources {
				writePruneFile(t, filepath.Join(g.SourceDir, "pilot", rel), rel)
			}
			if err := os.Chmod(filepath.Join(g.SourceDir, "pilot", resources[0]), 0755); err != nil {
				t.Fatal(err)
			}
			if err := g.generateForTarget(target); err != nil {
				t.Fatal(err)
			}
			root := g.resolveTargetDir(target)
			manifest, err := ReadManifest(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Generated) != 4 || len(manifest.Hashes) != 4 {
				t.Fatalf("incomplete manifest: %+v", manifest)
			}
			for _, rel := range manifest.Generated {
				hash, err := hashRegularFile(filepath.Join(root, rel))
				if err != nil || manifest.Hashes[rel] != hash {
					t.Fatalf("manifest hash mismatch for %s: %v", rel, err)
				}
			}
			for _, rel := range resources {
				content, err := os.ReadFile(filepath.Join(root, "skills", "pilot", rel))
				if err != nil || string(content) != rel {
					t.Fatalf("resource %s was not delivered intact: %q, %v", rel, content, err)
				}
			}
			info, err := os.Stat(filepath.Join(root, "skills", "pilot", resources[0]))
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("script executable permissions not preserved: %v", err)
			}
		})
	}
}

func TestDeliveryTransferRejectsMissingOrChangedSource(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "missing"}[missing], func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			rel := "skills/pilot/SKILL.md"
			file := filepath.Join(source, rel)
			writePruneFile(t, file, "original")
			if err := WriteManifest(source, "gemini", []string{rel}); err != nil {
				t.Fatal(err)
			}
			if _, err := SyncGeneratedFiles(source, destination); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(destination, ManifestFilename)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else {
				writePruneFile(t, file, "changed outside generation")
			}
			if _, err := SyncGeneratedFiles(source, destination); err == nil {
				t.Fatal("accepted source that differs from manifest")
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed transfer replaced destination manifest")
			}
		})
	}
}

func TestDeliveryLegacyManifestCanUpgradeCurrentFiles(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	writePruneFile(t, filepath.Join(source, "skills/pilot/SKILL.md"), "legacy current skill")
	writePruneFile(t, filepath.Join(source, ManifestFilename), `{"platform":"gemini","generated":["skills/pilot/SKILL.md"]}`)
	if count, err := SyncGeneratedFiles(source, destination); err != nil || count != 1 {
		t.Fatalf("legacy manifest cannot be delivered: %d, %v", count, err)
	}
	manifest, err := ReadManifest(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Hashes["skills/pilot/SKILL.md"]) != 64 {
		t.Fatal("legacy current file was not upgraded to hashed ownership")
	}
}

func TestDeliveryPruneRejectsSymlinkAncestor(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	rel := "skills/pilot/SKILL.md"
	writePruneFile(t, filepath.Join(dir, rel), "same bytes")
	if err := WriteManifest(dir, "gemini", []string{rel}); err != nil {
		t.Fatal(err)
	}
	previous, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	writePruneFile(t, filepath.Join(outside, "SKILL.md"), "same bytes")
	if err := os.RemoveAll(filepath.Join(dir, "skills/pilot")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "skills/pilot")); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneManifest(dir, previous, nil); err == nil {
		t.Fatal("prune accepted symlink escape")
	}
	if content, err := os.ReadFile(filepath.Join(outside, "SKILL.md")); err != nil || string(content) != "same bytes" {
		t.Fatalf("file outside managed root changed: %q, %v", content, err)
	}
}

func TestDeliveryCodexManifestFollowsActualOutput(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-home", true: "custom-skills-dir"}[custom], func(t *testing.T) {
			g := deliveryGenerator(t, "codex")
			g.CodexRootDir = ""
			g.OutputDir = ""
			g.CodexHome = filepath.Join(t.TempDir(), ".codex")
			g.RepoRoot = t.TempDir()
			manifestRoot := g.CodexHome
			if custom {
				manifestRoot = t.TempDir()
				g.OutputDir = filepath.Join(manifestRoot, "custom-bundles")
			}
			if err := g.generateForTarget("codex"); err != nil {
				t.Fatal(err)
			}
			manifest, err := ReadManifest(manifestRoot)
			if err != nil || manifest == nil {
				t.Fatalf("manifest not written alongside output: %v", err)
			}
			for _, rel := range manifest.Generated {
				if _, err := os.Stat(filepath.Join(manifestRoot, rel)); err != nil {
					t.Fatalf("manifest lists undelivered path %s: %v", rel, err)
				}
			}
		})
	}
}

func TestDeliveryVerifiedBundleRollbackPreservesNeighbors(t *testing.T) {
	v1, v2, installed := t.TempDir(), t.TempDir(), t.TempDir()
	rel := "skills/pilot/SKILL.md"
	writePruneFile(t, filepath.Join(v1, rel), "version one")
	writePruneFile(t, filepath.Join(v2, rel), "version two")
	writePruneFile(t, filepath.Join(v2, "skills/new/SKILL.md"), "new in version two")
	if err := WriteManifest(v1, "gemini", []string{rel}); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(v2, "gemini", []string{rel, "skills/new/SKILL.md"}); err != nil {
		t.Fatal(err)
	}
	neighbor := filepath.Join(installed, "skills/pilot/custom.md")
	writePruneFile(t, neighbor, "local notes")
	for _, source := range []string{v1, v2, v1} {
		if _, err := SyncGeneratedFiles(source, installed); err != nil {
			t.Fatal(err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(installed, rel)); err != nil || string(content) != "version one" {
		t.Fatalf("rollback content: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(installed, "skills/new/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("rollback did not prune version-two-only file: %v", err)
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "local notes" {
		t.Fatalf("rollback changed neighbor: %q, %v", content, err)
	}
	manifest, err := ReadManifest(installed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ReadManifest(v1)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Generated) != 1 || manifest.Hashes[rel] != want.Hashes[rel] {
		t.Fatalf("rollback manifest: %+v", manifest)
	}
}

func TestDeliveryBundleRejectsSymlinksBeforeWriting(t *testing.T) {
	for _, target := range []string{"codex", "claude", "gemini", "antigravity", "zed", "opencode"} {
		t.Run(target, func(t *testing.T) {
			for _, linked := range []string{"", "scripts", "references", "assets"} {
				t.Run(map[bool]string{true: "bundle", false: linked}[linked == ""], func(t *testing.T) {
					g := deliveryGenerator(t, target)
					root := g.resolveTargetDir(target)
					outside := t.TempDir()
					writePruneFile(t, filepath.Join(outside, "SKILL.md"), "outside sentinel")
					link := filepath.Join(root, "skills/pilot", linked)
					if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, link); err != nil {
						t.Fatal(err)
					}
					// A source resource must not be copied through the bundle link.
					g.Registry.Skills[0].Common.Scripts = []*Script{{Name: "run", Path: "scripts/run.sh"}}
					writePruneFile(t, filepath.Join(g.SourceDir, "pilot/scripts/run.sh"), "echo pilot")
					if err := g.generateForTarget(target); err == nil {
						t.Fatal("expected symlink destination error")
					}
					content, err := os.ReadFile(filepath.Join(outside, "SKILL.md"))
					if err != nil || string(content) != "outside sentinel" {
						t.Fatalf("outside file changed: %q, %v", content, err)
					}
					entries, err := os.ReadDir(outside)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 {
						t.Fatalf("generation created files or directories outside root: %v", entries)
					}
					if linked != "" {
						if _, err := os.Stat(filepath.Join(root, "skills/pilot/SKILL.md")); !os.IsNotExist(err) {
							t.Fatalf("generation started writing before preflight: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestDeliveryRejectsIncompleteSourceManifest(t *testing.T) {
	for _, invalid := range []string{`null`, `{}`, `{"platform":"gemini"}`, `{"generated":[]}`} {
		t.Run(invalid, func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			rel := "skills/pilot/SKILL.md"
			writePruneFile(t, filepath.Join(source, rel), "known good")
			if err := WriteManifest(source, "gemini", []string{rel}); err != nil {
				t.Fatal(err)
			}
			if _, err := SyncGeneratedFiles(source, destination); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(destination, ManifestFilename)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			writePruneFile(t, filepath.Join(source, ManifestFilename), invalid)
			if _, err := SyncGeneratedFiles(source, destination); err == nil {
				t.Fatal("incomplete manifest was accepted as empty desired state")
			}
			after, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("incomplete source replaced destination manifest")
			}
			if content, err := os.ReadFile(filepath.Join(destination, rel)); err != nil || string(content) != "known good" {
				t.Fatalf("incomplete source removed destination file: %q, %v", content, err)
			}
		})
	}
}
