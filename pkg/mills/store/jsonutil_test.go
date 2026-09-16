package store

import (
	"testing"
	"time"
)

// TestTimeRFC3339FixedWidthKeepsByteOrderChronological pins the storage
// invariant every ORDER BY / range predicate on a *_at column relies on: the
// stored text is fixed width, so SQLite's byte-wise TEXT collation agrees with
// chronological order even inside one second.
func TestTimeRFC3339FixedWidthKeepsByteOrderChronological(t *testing.T) {
	base := time.Date(2026, 9, 15, 23, 2, 26, 0, time.UTC)
	// Chronological. The .848100 / .848150 / .848191 trio is the shape the
	// flaky operator test produced: RFC3339Nano trims .848100000 to ".8481",
	// which byte-sorts after ".84815" because 'Z' > '5'.
	times := []time.Time{
		{},
		base.Add(-time.Second),
		base,
		base.Add(500 * time.Millisecond),
		base.Add(848100000),
		base.Add(848150000),
		base.Add(848191000),
		base.Add(848191001),
		base.Add(999999999),
		base.Add(time.Second),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	var prev string
	for i, tm := range times {
		s := timeRFC3339(tm)
		if len(s) != timeLayoutWidth {
			t.Errorf("%d: timeRFC3339(%v) = %q, want %d bytes", i, tm, s, timeLayoutWidth)
		}
		back, err := parseTime(s)
		if err != nil || !back.Equal(tm) {
			t.Errorf("%d: parseTime(%q) = %v, %v; want %v", i, s, back, err, tm)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Errorf("%d: %q is not RFC3339Nano-parseable: %v", i, s, err)
		}
		if i > 0 && prev >= s {
			t.Errorf("%d: %q must byte-sort before %q", i, prev, s)
		}
		prev = s
	}

	// Premise of the bug, kept so the rationale cannot silently rot: the
	// trimmed form inverts the .848100 / .848150 pair.
	trimmedA := base.Add(848100000).Format(time.RFC3339Nano)
	trimmedB := base.Add(848150000).Format(time.RFC3339Nano)
	if trimmedA < trimmedB {
		t.Fatalf("premise: RFC3339Nano %q should byte-sort after %q", trimmedA, trimmedB)
	}

	// Non-UTC inputs are normalised, so the same instant always stores the
	// same bytes.
	est := time.FixedZone("EST", -5*3600)
	if got, want := timeRFC3339(base.In(est)), timeRFC3339(base); got != want {
		t.Errorf("timeRFC3339 in EST = %q, want %q", got, want)
	}
}

// TestParseTimeAcceptsLegacyForms keeps rows written before migration 042 (or
// by the RFC3339 fallback) readable.
func TestParseTimeAcceptsLegacyForms(t *testing.T) {
	want := time.Date(2026, 9, 15, 23, 2, 26, 848100000, time.UTC)
	for _, s := range []string{
		"2026-09-15T23:02:26.848100000Z",
		"2026-09-15T23:02:26.8481Z",
		"2026-09-15T23:02:26.848100+00:00",
	} {
		got, err := parseTime(s)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	whole := time.Date(2026, 9, 15, 23, 2, 26, 0, time.UTC)
	if got, err := parseTime("2026-09-15T23:02:26Z"); err != nil || !got.Equal(whole) {
		t.Errorf("parseTime(no fraction) = %v, %v; want %v", got, err, whole)
	}
	if got, err := parseTime(""); err != nil || !got.IsZero() {
		t.Errorf("parseTime(\"\") = %v, %v; want zero time", got, err)
	}
}
