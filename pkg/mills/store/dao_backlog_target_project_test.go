package store

import (
	"context"
	"testing"
)

// TestBacklog_TargetProjectRoundTrip is the S1 regression: the per-item
// TargetProject column survives Put→Get (and an upsert that changes it).
func TestBacklog_TargetProjectRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{
		ID:            "MILLS-XREPO-1",
		Title:         "cross-repo item",
		State:         BacklogQueued,
		Priority:      P2,
		CreatedBy:     "test",
		TargetProject: "services/loom-flightdeck",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TargetProject != "services/loom-flightdeck" {
		t.Errorf("TargetProject = %q, want services/loom-flightdeck", got.TargetProject)
	}

	// Upsert to a different target, then clear it — both must round-trip.
	got.TargetProject = "services/flexdeck"
	if err := st.Backlog.Put(ctx, got); err != nil {
		t.Fatalf("put update: %v", err)
	}
	got2, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got2.TargetProject != "services/flexdeck" {
		t.Errorf("after update TargetProject = %q, want services/flexdeck", got2.TargetProject)
	}

	got2.TargetProject = ""
	if err := st.Backlog.Put(ctx, got2); err != nil {
		t.Fatalf("put clear: %v", err)
	}
	got3, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	if got3.TargetProject != "" {
		t.Errorf("after clear TargetProject = %q, want empty (home repo)", got3.TargetProject)
	}
}

func TestRepoBaseAndSameRepo(t *testing.T) {
	baseCases := map[string]string{
		"services/loom-core": "loom-core",
		"loom-core":          "loom-core",
		"LOOM-CORE":          "loom-core",
		"  services/Foo  ":   "foo",
		"platform/gitops/":   "gitops",
		"":                   "",
	}
	for in, want := range baseCases {
		if got := RepoBase(in); got != want {
			t.Errorf("RepoBase(%q) = %q, want %q", in, got, want)
		}
	}

	sameCases := []struct {
		a, b string
		want bool
	}{
		{"services/loom-core", "loom-core", true},
		{"loom-core", "LOOM-CORE", true},
		{"services/loom-core", "services/loom-flightdeck", false},
		{"loom-core", "flexdeck", false},
		{"", "loom-core", false},
		{"loom-core", "", false},
		{"", "", false},
	}
	for _, tc := range sameCases {
		if got := SameRepo(tc.a, tc.b); got != tc.want {
			t.Errorf("SameRepo(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
