package store

import (
	"strings"
	"testing"
)

func TestCanaryHeartbeatItem_Shape(t *testing.T) {
	item := CanaryHeartbeatItem("MILLS-CANARY-X", "", "", "", "unit-test")

	// Labels are the dedupe + auto-merge gate keys — a regression here would
	// silently break both the 24h dedupe scan and the canary label override.
	if !hasLabel(item.Labels, CanaryLabel) || !hasLabel(item.Labels, CanarySafeFixtureLabel) {
		t.Errorf("labels = %v; want both %q and %q", item.Labels, CanaryLabel, CanarySafeFixtureLabel)
	}
	if item.State != BacklogQueued {
		t.Errorf("state = %q; want queued", item.State)
	}
	if item.CreatedBy != "unit-test" {
		t.Errorf("created_by = %q; want unit-test", item.CreatedBy)
	}

	// Empty priority/path/title fall back to the canary defaults.
	if string(item.Priority) != CanaryDefaultPriority {
		t.Errorf("priority = %q; want %q", item.Priority, CanaryDefaultPriority)
	}
	if item.Title == "" {
		t.Error("title should default, not stay empty")
	}
	if len(item.Slices) != 1 || len(item.Slices[0].Files) != 1 || item.Slices[0].Files[0] != CanaryDefaultFixturePath {
		t.Errorf("slice files = %+v; want single %q", item.Slices, CanaryDefaultFixturePath)
	}
	if !strings.Contains(item.SpecDoc, CanaryDefaultFixturePath) {
		t.Errorf("spec doc must constrain the agent to %q; got %q", CanaryDefaultFixturePath, item.SpecDoc)
	}

	// Explicit overrides are honored.
	custom := CanaryHeartbeatItem("ID2", "Custom title", "P1", "testdata/mills-canary/other.md", "autopilot")
	if string(custom.Priority) != "P1" || custom.Title != "Custom title" {
		t.Errorf("overrides dropped: priority=%q title=%q", custom.Priority, custom.Title)
	}
	if custom.Slices[0].Files[0] != "testdata/mills-canary/other.md" {
		t.Errorf("custom path dropped: %v", custom.Slices[0].Files)
	}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
