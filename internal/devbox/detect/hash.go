package detect

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// depFiles are the dependency files whose content determines the fingerprint hash.
var depFiles = []string{
	"go.mod", "go.sum",
	"pyproject.toml", "uv.lock", "poetry.lock",
	"package.json", "pnpm-lock.yaml", "yarn.lock", "package-lock.json",
	"Cargo.toml", "Cargo.lock",
	".devbox.yaml",
}

// DependencyFiles returns the project-root manifests that determine a
// fingerprint — both its detected languages and its hash — so a caller that
// has no local checkout (git-clone sandboxes) can fetch exactly this set from
// the git host and fingerprint the copy.
func DependencyFiles() []string {
	out := make([]string, len(depFiles))
	copy(out, depFiles)
	return out
}

// RecipeHash folds the generated Dockerfile into a dependency hash so the
// sandbox image identity covers the recipe as well as the inputs. Two
// managers that agree on go.mod but render different Dockerfiles (an older
// binary installing tools from docker.io, a newer one starting from the
// registered base) must not share a tag: on 2026-09-05 a workstation build of
// `loom-core:092ff5c` carried golangci-lint v1 and the hub, trusting the
// registry for that tag, ran every Mills lint check against it. A recipe
// change now yields a new tag and one honest rebuild.
func RecipeHash(depHash string, dockerfile []byte) string {
	h := sha256.New()
	h.Write([]byte(depHash))
	h.Write([]byte{0})
	h.Write(dockerfile)
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// computeHash computes a SHA-256 hash of all dependency files present in the project.
func computeHash(projectDir string, _ *EnvFingerprint) (string, error) {
	h := sha256.New()

	// Sort file names for deterministic hashing
	files := make([]string, len(depFiles))
	copy(files, depFiles)
	sort.Strings(files)

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil {
			continue // skip missing files
		}
		// Write file name as separator to avoid collisions
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}

	return fmt.Sprintf("%x", h.Sum(nil))[:12], nil
}
