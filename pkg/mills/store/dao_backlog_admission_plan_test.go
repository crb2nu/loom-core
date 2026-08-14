package store

import (
	"context"
	"strings"
	"testing"
)

// The reconciler reads its admission slice through ListByStateLimit on every
// tick, so that read has to stay logarithmic as the queue grows into the
// thousands. idx_backlog_claim_queue(state, priority, created_at, id) is what
// makes it so: SQLite walks the index in the ORDER BY's own order and stops at
// LIMIT, touching `limit` entries out of however many are queued.
//
// The failure mode this pins is a plan regression, not a slow machine. Drop the
// index, reorder its columns, or change the query's ORDER BY out from under it,
// and SQLite falls back to sorting every queued row in a temp b-tree before the
// LIMIT can apply — O(queue) work per tick, which is exactly what the reconciler
// tick's former `latency < 2s` assertion was proxying for. A temp b-tree over
// 10k rows is invisible to a wall-clock bound on a fast idle runner and
// indistinguishable from CPU contention on a busy one; it is unambiguous here.
func TestListByStateLimit_AdmissionReadStaysIndexOrderedAtTenThousandRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedClaimQueue(t, st, "MILLS-ADMIT", 10_000)

	const limit = 4
	query := `SELECT ` + backlogColumns + ` FROM backlog_items
		WHERE state = ? ORDER BY priority ASC, created_at ASC, id ASC LIMIT ?`
	args := []any{string(BacklogQueued), limit}

	if !queryPlanUsesIndex(t, st, "EXPLAIN QUERY PLAN "+query, args, "idx_backlog_claim_queue") {
		t.Fatal("admission read did not use idx_backlog_claim_queue")
	}
	for _, detail := range queryPlan(t, st, "EXPLAIN QUERY PLAN "+query, args) {
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("admission read sorts the whole queue before its LIMIT: %q", detail)
		}
	}

	// The plan is the claim; this is the behavioural check that the query the
	// plan describes is the query the DAO actually runs. Comparing against the
	// unbounded read's own head keeps the assertion about LIMIT — that stopping
	// early returns the same items as ordering everything and taking the first
	// few — rather than about how the fixture's timestamps happen to collate.
	bounded, err := st.Backlog.ListByStateLimit(ctx, BacklogQueued, limit)
	if err != nil {
		t.Fatalf("bounded admission read: %v", err)
	}
	full, err := st.Backlog.ListByState(ctx, BacklogQueued)
	if err != nil {
		t.Fatalf("unbounded admission read: %v", err)
	}
	if len(bounded) != limit || len(full) != 10_000 {
		t.Fatalf("bounded=%d unbounded=%d rows, want %d/10000", len(bounded), len(full), limit)
	}
	for i, item := range bounded {
		if item.ID != full[i].ID {
			t.Fatalf("bounded admission item %d = %s, want %s (head of the full ordering)",
				i, item.ID, full[i].ID)
		}
	}
}
