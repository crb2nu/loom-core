// Package pm implements a minimal project-management store. Slice A owns the
// RISKS domain: a single Qdrant-backed collection (pm_risks) holding Risk
// records keyed by a canonical project identifier (GitLab path_with_namespace).
//
// The package is deliberately small and single-responsibility. Writes are
// decoupled from embedding: a Risk MUST persist even when the shared embedder
// is unavailable, so embedding is best-effort and a deterministic fallback
// vector keeps the point filterable by payload.
package pm

import (
	"strings"
	"time"
)

// Valid likelihood / impact levels.
const (
	LevelLow    = "low"
	LevelMedium = "medium"
	LevelHigh   = "high"
)

// Valid risk statuses.
const (
	StatusIdentified = "identified"
	StatusMitigating = "mitigating"
	StatusAccepted   = "accepted"
	StatusClosed     = "closed"
)

// Risk is a single tracked project risk.
type Risk struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"` // canonical GitLab path_with_namespace, e.g. "services/flexdeck"
	Title      string    `json:"title"`
	Likelihood string    `json:"likelihood"` // low | medium | high
	Impact     string    `json:"impact"`     // low | medium | high
	Mitigation string    `json:"mitigation"`
	Owner      string    `json:"owner"`
	Status     string    `json:"status"` // identified | mitigating | accepted | closed
	Links      []string  `json:"links"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// validLevels and validStatuses back the validation helpers.
var (
	validLevels = map[string]bool{
		LevelLow:    true,
		LevelMedium: true,
		LevelHigh:   true,
	}
	validStatuses = map[string]bool{
		StatusIdentified: true,
		StatusMitigating: true,
		StatusAccepted:   true,
		StatusClosed:     true,
	}
)

// IsValidLevel reports whether s is an accepted likelihood/impact level.
func IsValidLevel(s string) bool { return validLevels[strings.ToLower(strings.TrimSpace(s))] }

// IsValidStatus reports whether s is an accepted risk status.
func IsValidStatus(s string) bool { return validStatuses[strings.ToLower(strings.TrimSpace(s))] }

// embedText returns the text used to embed a risk: "Title Mitigation".
func (r Risk) embedText() string {
	return strings.TrimSpace(r.Title + " " + r.Mitigation)
}
