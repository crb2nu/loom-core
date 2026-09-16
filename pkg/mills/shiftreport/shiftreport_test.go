package shiftreport

import (
	"strings"
	"testing"
	"time"
)

func TestMainRedExternalHoldReporting(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	active := Compose(Input{GeneratedAt: now, WindowSeconds: 3600, MainRedExternalHold: &MainRedExternalHold{Project: "p", Branch: "main", ActivatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}})
	if !strings.Contains(active.Markdown, "active") || !strings.Contains(active.Markdown, "main_red_external") {
		t.Fatalf("active markdown=%s", active.Markdown)
	}
	expired := Compose(Input{GeneratedAt: now, WindowSeconds: 3600, MainRedExternalHold: &MainRedExternalHold{Project: "p", Branch: "main", ExpiresAt: now, Escalated: true}})
	if !strings.Contains(expired.Markdown, "expired_escalated") {
		t.Fatalf("expired markdown=%s", expired.Markdown)
	}
	pending := Compose(Input{GeneratedAt: now, MainRedExternalHold: &MainRedExternalHold{ExpiresAt: now}})
	if !strings.Contains(pending.Markdown, "pending reconciliation") || strings.Contains(pending.Markdown, "expired_escalated") {
		t.Fatalf("pending markdown=%s", pending.Markdown)
	}
	absent := Compose(Input{GeneratedAt: now, WindowSeconds: 3600})
	if strings.Contains(absent.Markdown, "Merge queue hold") {
		t.Fatalf("absent markdown=%s", absent.Markdown)
	}
}
