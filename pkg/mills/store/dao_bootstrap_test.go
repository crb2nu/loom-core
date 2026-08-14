package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBootstrapDAO_InsertGetList(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := &BootstrappedProject{
		Project:   "services/procmodel",
		PlanID:    "plan-abc",
		WebURL:    "https://gitlab.example/services/procmodel",
		CreatedBy: "mills:project-bootstrap",
		CreatedAt: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
	}
	if err := st.Bootstrap.Insert(ctx, first); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := st.Bootstrap.Get(ctx, "services/procmodel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlanID != "plan-abc" || got.WebURL != first.WebURL || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("got %+v", got)
	}

	// A second row lists AFTER the first (oldest-first demand order).
	second := &BootstrappedProject{
		Project:   "services/other",
		PlanID:    "plan-def",
		CreatedAt: first.CreatedAt.Add(time.Hour),
	}
	if err := st.Bootstrap.Insert(ctx, second); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	rows, err := st.Bootstrap.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].Project != "services/procmodel" || rows[1].Project != "services/other" {
		t.Errorf("list order = %v", projects(rows))
	}
}

func TestBootstrapDAO_InsertConflict(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	p := &BootstrappedProject{Project: "services/procmodel", PlanID: "plan-abc"}
	if err := st.Bootstrap.Insert(ctx, p); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Re-minting the same path is a conflict, not an upsert: the original
	// plan linkage must survive.
	err := st.Bootstrap.Insert(ctx, &BootstrappedProject{Project: "services/procmodel", PlanID: "plan-zzz"})
	if !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("re-insert err = %v, want ErrAlreadyBootstrapped", err)
	}
	got, err := st.Bootstrap.Get(ctx, "services/procmodel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlanID != "plan-abc" {
		t.Errorf("plan_id = %q, want original plan-abc", got.PlanID)
	}
}

func TestBootstrapDAO_GetUnknownIsNotFound(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Bootstrap.Get(context.Background(), "services/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func projects(rows []*BootstrappedProject) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Project)
	}
	return out
}
