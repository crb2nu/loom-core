package skills

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manifest tracks generated files for a platform.
type Manifest struct {
	Platform  string            `json:"platform"`
	Generated []string          `json:"generated"`
	Timestamp string            `json:"timestamp"`
	Hashes    map[string]string `json:"hashes,omitempty"` // SHA-256 of delivered bytes; absent in legacy manifests.
}

// ManifestFilename is the standard manifest filename written into each platform dir.
const ManifestFilename = ".loom-skills-manifest.json"

// WriteManifest writes a manifest file into the given directory.
func WriteManifest(dir, platform string, files []string) error {
	m := Manifest{
		Platform:  platform,
		Generated: files,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Hashes:    make(map[string]string, len(files)),
	}
	for _, rel := range files {
		path, err := managedFilePath(dir, rel)
		if err != nil {
			return err
		}
		hash, err := hashRegularFile(path)
		if err != nil {
			return fmt.Errorf("hash delivered file %s: %w", rel, err)
		}
		m.Hashes[rel] = hash
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write so codex's skill dir watcher never observes a truncated manifest.
	return writeFileAtomic(filepath.Join(dir, ManifestFilename), append(data, '\n'), 0o644)
}

// ReadManifest reads a manifest from the given directory, returning nil if not found.
func ReadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Distinguish an intentionally empty generated list (including legacy
	// explicit null) from a truncated-but-valid JSON object or JSON null.
	var shape struct {
		Platform  *string         `json:"platform"`
		Generated json.RawMessage `json:"generated"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, err
	}
	if shape.Platform == nil || strings.TrimSpace(*shape.Platform) == "" || shape.Generated == nil {
		return nil, fmt.Errorf("invalid skills manifest: nonempty platform and explicit generated list are required")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for _, rel := range m.Generated {
		if err := validateManagedPath(rel); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func validateManagedPath(rel string) error {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || rel == "." || rel == ManifestFilename {
		return fmt.Errorf("unsafe managed skill path %q", rel)
	}
	return nil
}

// managedFilePath refuses symlinks within the managed root, including a
// symlink at the leaf. A lexical prefix check alone can escape through an
// otherwise valid path such as skills/linked-to-outside/SKILL.md.
func managedFilePath(dir, rel string) (string, error) {
	if err := validateManagedPath(rel); err != nil {
		return "", err
	}
	path := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("managed skill path %q contains symlink %s", rel, path)
		}
	}
	return path, nil
}

func hashRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
