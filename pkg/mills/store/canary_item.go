package store

import "fmt"

// Canary heartbeat defaults. The store package is the shared home for the
// canary's shape because both the `loom mills pipelines canary` CLI and the
// operator's canary-autopilot scheduler import store (but not the heavy
// pkg/mills core), so a single builder here cannot drift between the two paths.
const (
	// CanaryLabel marks a backlog item as a Mills heartbeat canary — the key
	// the 24h dedupe scan and the auto-merge label override both filter on.
	CanaryLabel = "mills-canary"
	// CanarySafeFixtureLabel marks the item as touching only a safe fixture.
	CanarySafeFixtureLabel = "safe-fixture"
	// CanaryDefaultFixturePath is the deterministic Markdown fixture the canary
	// is constrained to edit.
	CanaryDefaultFixturePath = "testdata/mills-canary/heartbeat.md"
	// CanaryDefaultPriority is the canary's baseline backlog priority.
	CanaryDefaultPriority = "P3"
)

// CanaryHeartbeatItem builds the deterministic Mills heartbeat-canary backlog
// item. It is the single source of truth for the canary's shape, shared by the
// CLI (manual enqueue) and the operator's autopilot scheduler (daily enqueue)
// so the two paths cannot drift.
//
// The item carries the mills-canary + safe-fixture labels (the dedupe + policy
// gate keys), a SpecDoc constraining the agent to a single Markdown fixture,
// and a tight budget. createdBy records which path enqueued it for audit. Empty
// title/priority/path fall back to the canary defaults.
func CanaryHeartbeatItem(id, title, priority, path, createdBy string) BacklogItem {
	if priority == "" {
		priority = CanaryDefaultPriority
	}
	if path == "" {
		path = CanaryDefaultFixturePath
	}
	if title == "" {
		title = "Mills canary: update heartbeat fixture"
	}
	if createdBy == "" {
		createdBy = "mills"
	}
	return BacklogItem{
		ID:       id,
		Title:    title,
		Labels:   []string{CanaryLabel, CanarySafeFixtureLabel},
		State:    BacklogQueued,
		Priority: Priority(priority),
		SpecDoc: fmt.Sprintf(
			"Deterministic Mills canary. Update only `%s`: set the visible Run ID to `%s`, keep the file valid Markdown, and do not touch code or generated assets.",
			path, id,
		),
		Success: SuccessCriteria{
			Tests: []string{
				"go test ./cmd/loom -run Mills",
			},
			ManualCheck: "The merge request touches only the canary fixture path and reaches green CI before merge.",
		},
		Budget: Budget{
			MaxCostUSD:         1,
			MaxTurns:           6,
			MaxPipelineMinutes: 20,
		},
		Slices: []Slice{{
			Name:  "heartbeat-fixture",
			Files: []string{path},
			Tests: []string{"go test ./cmd/loom -run Mills"},
		}},
		CreatedBy: createdBy,
	}
}
