package store

import (
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dsnPragmas returns the `_pragma` values a DSN carries, decoded the way the
// driver sees them.
func dsnPragmas(t *testing.T, dsn string) []string {
	t.Helper()
	_, rawQuery, ok := strings.Cut(dsn, "?")
	if !ok {
		t.Fatalf("dsn %q has no query string", dsn)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("parse dsn query %q: %v", rawQuery, err)
	}
	return q["_pragma"]
}

// TestBuildDSNForSynchronousByBinaryKind pins the 2026-09-15 test:unit fix:
// production DSNs keep synchronous=NORMAL, test binaries get synchronous=OFF,
// the other per-connection pragmas survive, and journal_mode stays OUT of the
// DSN — the driver applies DSN pragmas in sorted order, which would run the
// WAL switch before synchronous and fsync a brand-new file under SQLite's
// default FULL. Open sets WAL once after the DSN pragmas are in force.
func TestBuildDSNForSynchronousByBinaryKind(t *testing.T) {
	cases := []struct {
		name         string
		inTestBinary bool
		want         string
		reject       string
	}{
		{name: "production", inTestBinary: false, want: "synchronous(NORMAL)", reject: "synchronous(OFF)"},
		{name: "go test", inTestBinary: true, want: "synchronous(OFF)", reject: "synchronous(NORMAL)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pragmas := dsnPragmas(t, buildDSNFor("/tmp/mills.db", tc.inTestBinary))
			if !slices.Contains(pragmas, tc.want) {
				t.Fatalf("pragmas %q lack %s", pragmas, tc.want)
			}
			if slices.Contains(pragmas, tc.reject) {
				t.Fatalf("pragmas %q must not carry %s", pragmas, tc.reject)
			}
			for _, keep := range []string{"foreign_keys(ON)", "busy_timeout(5000)", "temp_store(MEMORY)"} {
				if !slices.Contains(pragmas, keep) {
					t.Fatalf("pragmas %q dropped %s", pragmas, keep)
				}
			}
			for _, p := range pragmas {
				if strings.HasPrefix(strings.ToLower(p), "journal_mode") {
					t.Fatalf("journal_mode must not be a DSN pragma (driver sorts it ahead of synchronous): %q", pragmas)
				}
			}
		})
	}
}

// TestBuildDSNForKeepsExistingQuery guards the separator logic: a path that
// already carries query parameters (in-memory shared-cache DSNs) must get its
// pragmas appended with '&', not a second '?'.
func TestBuildDSNForKeepsExistingQuery(t *testing.T) {
	dsn := buildDSNFor("file:mem?mode=memory&cache=shared", true)
	if strings.Count(dsn, "?") != 1 {
		t.Fatalf("expected a single '?' in %q", dsn)
	}
	if !strings.HasPrefix(dsn, "file:mem?mode=memory&cache=shared&") {
		t.Fatalf("existing query must be preserved verbatim: %q", dsn)
	}
	if got := dsnPragmas(t, dsn); !slices.Contains(got, "synchronous(OFF)") {
		t.Fatalf("pragmas %q lost synchronous(OFF) behind an existing query", got)
	}
}

// TestDSNTestBinariesSkipFsync asserts the settings actually reach live
// connections: under `go test` a fresh file-backed store reports
// synchronous=0 (OFF) on both the read pool and the ledger writer, and is in
// WAL mode even though journal_mode is no longer a DSN pragma. A regression
// here (typo, a driver that stops honouring `_pragma`, the WAL switch lost)
// would silently reintroduce the fsync-per-store cost that timed out four
// test:unit jobs on 2026-09-15, or drop production's WAL concurrency.
func TestDSNTestBinariesSkipFsync(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if st.writer == st.db {
		t.Fatal("file-backed store must have a distinct ledger writer handle")
	}
	var synchronous int
	if err := st.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 0 { // 0 == OFF
		t.Fatalf("read pool synchronous = %d under go test, want 0 (OFF): every store open/close would fsync again", synchronous)
	}
	if err := st.writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 0 {
		t.Fatalf("ledger writer synchronous = %d under go test, want 0 (OFF)", synchronous)
	}
	var journal string
	if err := st.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Fatalf("read pool journal_mode = %q, want wal: Open must still switch a fresh file to WAL", journal)
	}
	if err := st.writer.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Fatalf("ledger writer journal_mode = %q, want wal: the persistent header switch must reach the second handle", journal)
	}
}

// TestOpenInMemoryToleratesNoWAL pins the in-memory exemption: SQLite cannot
// put a memory database in WAL mode and answers "memory" to the switch, which
// Open must accept for :memory: paths rather than failing closed.
func TestOpenInMemoryToleratesNoWAL(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("open :memory: store: %v", err)
	}
	defer st.Close()
	var journal string
	if err := st.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "memory") {
		t.Fatalf("in-memory journal_mode = %q, want memory", journal)
	}
}
