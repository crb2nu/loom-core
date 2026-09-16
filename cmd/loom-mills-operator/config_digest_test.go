package main

import (
	"testing"
	"time"
)

func TestDigestScheduleParsingAndNextRun(t *testing.T) {
	if h, m, err := parseDigestAt("06:30"); err != nil || h != 6 || m != 30 {
		t.Fatalf("parse=%d:%d %v", h, m, err)
	}
	if _, _, err := parseDigestAt("25:00"); err == nil {
		t.Fatal("expected invalid time")
	}
	now := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)
	if got := nextDigestRun(now, "06:00"); !got.Equal(time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	t.Setenv("LOOM_MILLS_DIGEST_AT", "07:15")
	cfg := DefaultConfig()
	cfg.ApplyEnv()
	if cfg.DigestAt != "07:15" {
		t.Fatal(cfg.DigestAt)
	}
}
