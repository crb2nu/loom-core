package skills

import (
	"fmt"
	"os"
)

// copyDeliveredFile preserves executable permissions and replaces each file
// atomically, so a failed resource copy cannot truncate the previous version.
func copyDeliveredFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data, info.Mode().Perm())
}

// SyncGeneratedFiles delivers a generated manifest's files to a second root.
// The destination manifest is published only after all copies and stale-file
// removals succeed. Earlier successful copies can remain on a later failure;
// this is per-file atomic delivery, not a transaction over the whole tree.
func SyncGeneratedFiles(sourceDir, destinationDir string) (int, error) {
	manifest, err := ReadManifest(sourceDir)
	if err != nil {
		return 0, fmt.Errorf("read source skill manifest: %w", err)
	}
	if manifest == nil {
		return 0, nil
	}
	previous, err := ReadManifest(destinationDir)
	if err != nil {
		return 0, fmt.Errorf("read destination skill manifest: %w", err)
	}
	for _, rel := range manifest.Generated {
		src, err := managedFilePath(sourceDir, rel)
		if err != nil {
			return 0, fmt.Errorf("source skill file %s: %w", rel, err)
		}
		dst, err := managedFilePath(destinationDir, rel)
		if err != nil {
			return 0, fmt.Errorf("destination skill file %s: %w", rel, err)
		}
		if expected := manifest.Hashes[rel]; expected != "" {
			actual, err := hashRegularFile(src)
			if err != nil {
				return 0, fmt.Errorf("verify source skill file %s: %w", rel, err)
			}
			if actual != expected {
				return 0, fmt.Errorf("source skill file %s changed since generation", rel)
			}
		}
		if err := copyDeliveredFile(src, dst); err != nil {
			return 0, fmt.Errorf("copy skill file %s: %w", rel, err)
		}
	}
	if _, err := PruneManifest(destinationDir, previous, manifest.Generated); err != nil {
		return 0, fmt.Errorf("prune destination skill files: %w", err)
	}
	if err := WriteManifest(destinationDir, manifest.Platform, manifest.Generated); err != nil {
		return 0, fmt.Errorf("write destination skill manifest: %w", err)
	}
	return len(manifest.Generated), nil
}
