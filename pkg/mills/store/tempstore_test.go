package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDSNAppliesMemoryTempStore guards the fix for the 2026-08-01 operator
// crashloop: migration 019's table rebuild needed SQLite temp storage, and
// under readOnlyRootFilesystem every candidate temp dir (SQLITE_TMPDIR,
// TMPDIR, /var/tmp, /tmp) is unwritable, so boot died with
// SQLITE_IOERR_GETTEMPPATH. temp_store=MEMORY removes the temp-file path
// entirely. The container condition cannot be reproduced in a test (the
// host's /tmp is always writable and SQLite falls back to it regardless of
// TMPDIR), so this asserts the pragma actually reaches pooled connections —
// the regression that would silently reintroduce the crashloop.
func TestDSNAppliesMemoryTempStore(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var mode int
	if err := st.db.QueryRowContext(context.Background(), `PRAGMA temp_store`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 { // 2 == MEMORY
		t.Fatalf("temp_store = %d, want 2 (MEMORY) — table-rebuild migrations crashloop the read-only-rootfs operator without it", mode)
	}
}
