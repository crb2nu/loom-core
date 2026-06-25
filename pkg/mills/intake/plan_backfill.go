package intake

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// PlanAuthor authors a first-class Plan for a backlog item and returns
// its plan_id. Implemented by *clients.PlanClient.
type PlanAuthor interface {
	AuthorPlan(ctx context.Context, item *store.BacklogItem, project string) (string, error)
}

// PlanBackfillStore is the backfiller's view of the backlog DAO: list
// every item, then persist one whose PlanID it just stamped. Implemented
// by *store.BacklogDAO.
type PlanBackfillStore interface {
	List(ctx context.Context) ([]*store.BacklogItem, error)
	Put(ctx context.Context, item *store.BacklogItem) error
}

// PlanBackfiller authors a Plan for each backlog item that has no PlanID
// yet and stamps the returned id back onto the item. It is the one-shot
// migration half of plan-store ↔ Mills convergence (plan store S7b):
// existing backlog predates the Plan store, so nothing links them until
// this runs. Re-running is safe — already-linked items are skipped, and
// the authored plan id is deterministic so a retry upserts the same Plan.
// Best-effort per item: a single author/put failure is logged and the
// loop continues, mirroring the importer's resilience.
type PlanBackfiller struct {
	Store   PlanBackfillStore
	Author  PlanAuthor
	Project string
	Logger  *slog.Logger
}

// Run authors plans for all un-linked backlog items and returns the
// number newly linked. A nil Store/Author is a no-op (returns 0), so a
// caller can wire it unconditionally and gate on configuration.
func (b *PlanBackfiller) Run(ctx context.Context) (int, error) {
	if b == nil || b.Store == nil || b.Author == nil {
		return 0, nil
	}
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}
	items, err := b.Store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("plan backfill: list backlog: %w", err)
	}
	linked := 0
	for _, item := range items {
		if item == nil || item.PlanID != "" {
			continue
		}
		planID, err := b.Author.AuthorPlan(ctx, item, b.Project)
		if err != nil {
			logger.Warn("plan backfill: author failed", "id", item.ID, "err", err)
			continue
		}
		if planID == "" {
			logger.Warn("plan backfill: empty plan_id", "id", item.ID)
			continue
		}
		item.PlanID = planID
		if err := b.Store.Put(ctx, item); err != nil {
			logger.Warn("plan backfill: stamp put failed", "id", item.ID, "plan_id", planID, "err", err)
			continue
		}
		linked++
		logger.Info("plan backfill: linked backlog item to plan", "id", item.ID, "plan_id", planID)
	}
	if linked > 0 {
		logger.Info("plan backfill complete", "linked", linked, "scanned", len(items))
	}
	return linked, nil
}
