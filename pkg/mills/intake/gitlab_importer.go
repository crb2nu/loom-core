// Package intake hosts external-source backlog importers for Mills.
// First slice ships the GitLab issue importer (Slice 1a of plan 43).
// Future sources (Loki workspace errors, roadmap pulls, canary
// autopilot) plug in alongside as sibling files in this package.
package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// GitLabIssuesClient is the importer's view of the GitLab REST surface.
// Trimmed to ListIssues so tests can stub it without standing up the
// full GitLabClient. Implemented by *clients.GitLabClient.
type GitLabIssuesClient interface {
	ListIssues(ctx context.Context, opts clients.ListIssuesOpts) ([]clients.IssueListItem, error)
}

// BacklogStore is the importer's view of the backlog DAO. Read-then-
// insert dedup is intentional: a Put would stomp local state changes
// the reconciler may have already made (e.g. priority bumped, state
// flipped to running). Implemented by *store.BacklogDAO.
type BacklogStore interface {
	Get(ctx context.Context, id string) (*store.BacklogItem, error)
	Put(ctx context.Context, item *store.BacklogItem) error
}

const (
	defaultEligibleLabel = "mills-eligible"
	defaultPollInterval  = 5 * time.Minute
	defaultPriority      = store.P2
	importerCreatedBy    = "mills:gitlab-importer"
	// importerIDPrefix prefixes every BacklogItem.ID produced by this
	// importer so other intake sources can't collide. The full id shape
	// is "gl-<project_id>-<issue_iid>".
	importerIDPrefix = "gl-"
)

// GitLabImporterConfig captures the operator-tunable knobs. Defaults
// apply when fields are zero.
type GitLabImporterConfig struct {
	EligibleLabel   string
	PollInterval    time.Duration
	DefaultPriority store.Priority
}

func (c *GitLabImporterConfig) applyDefaults() {
	if c.EligibleLabel == "" {
		c.EligibleLabel = defaultEligibleLabel
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.DefaultPriority == "" {
		c.DefaultPriority = defaultPriority
	}
}

// GitLabImporter polls a GitLab project for open issues bearing the
// eligible label and creates one BacklogItem per fresh issue. It is
// idempotent on re-run: imports are keyed by issue iid and skipped if
// the backlog already has that id, so the reconciler's state
// transitions are never clobbered.
type GitLabImporter struct {
	client GitLabIssuesClient
	store  BacklogStore
	cfg    GitLabImporterConfig
	logger *slog.Logger
	// PlanAuthor, when set, authors a first-class Plan for each newly
	// imported item and stamps its plan_id so the item is born linked
	// (plan store S7b-β), instead of waiting for the boot-time backfill.
	// Nil = disabled (the default); the backfill still links items later.
	// Set by the operator when LOOM_MILLS_PLAN_AUTHORING is enabled and
	// the MCP hub is reachable.
	PlanAuthor PlanAuthor
	// Project scopes authored plans (canonical GitLab path).
	Project string
	// Enabled is the live global admission barrier. Nil preserves the
	// historical always-enabled behavior for standalone callers/tests.
	Enabled func() bool
	active  atomic.Int64
}

// NewGitLabImporter wires a client + store + config. Defaults are
// applied to zero fields.
func NewGitLabImporter(client GitLabIssuesClient, st BacklogStore, cfg GitLabImporterConfig, logger *slog.Logger) *GitLabImporter {
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &GitLabImporter{
		client: client,
		store:  st,
		cfg:    cfg,
		logger: logger,
	}
}

// Run drives Tick on the configured PollInterval until ctx is done.
// Errors from a single tick are logged and the loop continues; a hard
// abort only happens when ctx is canceled.
func (im *GitLabImporter) Run(ctx context.Context) error {
	im.logger.Info("gitlab importer started",
		"eligible_label", im.cfg.EligibleLabel,
		"poll_interval", im.cfg.PollInterval,
	)
	// Tick once immediately so a freshly-deployed operator picks up
	// any pre-existing eligible issues without waiting one interval.
	if _, err := im.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		im.logger.Warn("gitlab importer initial tick failed", "err", err)
	}
	t := time.NewTicker(im.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := im.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				im.logger.Warn("gitlab importer tick failed", "err", err)
			}
		}
	}
}

// Tick performs one importer pass and returns the number of newly-
// created backlog items. Safe to call from tests; the Run loop just
// drives this on a ticker.
func (im *GitLabImporter) Tick(ctx context.Context) (int, error) {
	im.active.Add(1)
	defer im.active.Add(-1)
	if im.Enabled != nil && !im.Enabled() {
		return 0, nil
	}
	issues, err := im.client.ListIssues(ctx, clients.ListIssuesOpts{
		Labels:  []string{im.cfg.EligibleLabel},
		State:   "opened",
		PerPage: 100,
	})
	if err != nil {
		return 0, fmt.Errorf("list issues: %w", err)
	}
	imported := 0
	for _, issue := range issues {
		item := issueToBacklog(issue, im.cfg.DefaultPriority)
		// Skip closed/locked issues defensively; GitLab's labels filter
		// can leak issues whose state changed mid-query window.
		if issue.State != "" && issue.State != "opened" {
			continue
		}
		// Existence check: if reconciler/council has already touched
		// this item, leave its state alone.
		existing, getErr := im.store.Get(ctx, item.ID)
		if getErr == nil && existing != nil {
			continue
		}
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			im.logger.Warn("gitlab importer get failed",
				"id", item.ID, "err", getErr)
			continue
		}
		// Born-linked: author a Plan before the first Put when inline
		// authoring is enabled, so the persisted item already carries a
		// plan_id. Best-effort — a failure leaves the item unlinked and
		// it still imports (the backfill links it later).
		im.maybeAuthorPlan(ctx, item)
		if err := im.store.Put(ctx, item); err != nil {
			im.logger.Warn("gitlab importer put failed",
				"id", item.ID, "iid", issue.IID, "err", err)
			continue
		}
		imported++
		im.logger.Info("gitlab importer created backlog item",
			"id", item.ID, "iid", issue.IID, "title", issue.Title,
			"priority", item.Priority)
	}
	return imported, nil
}

// ActiveOperations reports importer passes currently executing.
func (im *GitLabImporter) ActiveOperations() int64 {
	if im == nil {
		return 0
	}
	return im.active.Load()
}

// maybeAuthorPlan authors a Plan for item and stamps item.PlanID when an
// inline PlanAuthor is configured. Best-effort: a nil author or any
// failure is a no-op (the item still imports; the boot backfill can link
// it later), so plan authoring never blocks intake.
func (im *GitLabImporter) maybeAuthorPlan(ctx context.Context, item *store.BacklogItem) {
	if im.PlanAuthor == nil || item == nil || item.PlanID != "" {
		return
	}
	planID, err := im.PlanAuthor.AuthorPlan(ctx, item, im.Project)
	if err != nil {
		im.logger.Warn("gitlab importer plan authoring failed",
			"id", item.ID, "err", err)
		return
	}
	if planID != "" {
		item.PlanID = planID
		im.logger.Info("gitlab importer authored plan for item",
			"id", item.ID, "plan_id", planID)
	}
}

// issueToBacklog converts a GitLab issue into a BacklogItem with the
// importer's deterministic ID and a queued state ready for the
// reconciler.
func issueToBacklog(issue clients.IssueListItem, dflt store.Priority) *store.BacklogItem {
	iid := issue.IID
	item := &store.BacklogItem{
		ID:             fmt.Sprintf("%s%d-%d", importerIDPrefix, issue.ProjectID, issue.IID),
		GitLabIssueIID: &iid,
		Title:          issue.Title,
		Labels:         issue.Labels,
		State:          store.BacklogQueued,
		Priority:       extractPriority(issue.Labels, dflt),
		SpecDoc:        issue.Description,
		CreatedBy:      importerCreatedBy,
	}
	return item
}

// extractPriority scans labels for "priority:P0..P3" (case-insensitive)
// and returns the highest priority found. Missing or malformed → dflt.
func extractPriority(labels []string, dflt store.Priority) store.Priority {
	// Iterate in label order, keep the highest priority encountered.
	// P0 > P1 > P2 > P3, so we compare alphabetically (P0 < P1 lexically).
	best := store.Priority("")
	for _, l := range labels {
		lower := strings.ToLower(strings.TrimSpace(l))
		const pfx = "priority:"
		if !strings.HasPrefix(lower, pfx) {
			continue
		}
		raw := strings.ToUpper(strings.TrimSpace(lower[len(pfx):]))
		switch store.Priority(raw) {
		case store.P0, store.P1, store.P2, store.P3:
			if best == "" || string(raw) < string(best) {
				best = store.Priority(raw)
			}
		}
	}
	if best == "" {
		return dflt
	}
	return best
}
