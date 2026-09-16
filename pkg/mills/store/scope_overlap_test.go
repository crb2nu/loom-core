package store

import (
	"context"
	"errors"
	"testing"
)

func scopeItem(id string, files ...string) *BacklogItem {
	return &BacklogItem{ID: id, Slices: []Slice{{Name: "impl", Files: files}}}
}

func TestBacklogScopesOverlap_ChangelogExcluded(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []string
		want    bool
		witness string
	}{
		{
			name: "changelog.d globs never collide",
			a:    []string{"changelog.d/*.md"},
			b:    []string{"changelog.d/*.md"},
			want: false,
		},
		{
			name: "disjoint code scopes with shared changelog glob do not overlap",
			a:    []string{"internal/hud/spawn.go", "changelog.d/*.md"},
			b:    []string{".gitlab-ci.yml", "changelog.d/*.md"},
			want: false,
		},
		{
			name: "distinct literal fragment slugs do not overlap",
			a:    []string{"changelog.d/feat-a.added.md"},
			b:    []string{"changelog.d/fix-b.fixed.md"},
			want: false,
		},
		{
			name: "identical literal fragment path still collides",
			a:    []string{"changelog.d/same.fixed.md"},
			b:    []string{"changelog.d/same.fixed.md"},
			want: true, witness: "changelog.d/same.fixed.md",
		},
		{
			name: "bare changelog.d path contributes nothing",
			a:    []string{"changelog.d"},
			b:    []string{"changelog.d", "pkg/mills/reconciler.go"},
			want: false,
		},
		{
			name: "real code overlap still detected",
			a:    []string{"pkg/mills/pipeline/runner.go", "changelog.d/*.md"},
			b:    []string{"pkg/mills/pipeline/runner.go", "changelog.d/*.md"},
			want: true, witness: "pkg/mills/pipeline/runner.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, witness := BacklogScopesOverlap(scopeItem("A", tc.a...), scopeItem("B", tc.b...), "")
			if got != tc.want {
				t.Fatalf("overlap=%v want %v (witness=%q)", got, tc.want, witness)
			}
			if tc.want && witness != tc.witness {
				t.Errorf("witness=%q want %q", witness, tc.witness)
			}
		})
	}
}

// The sibling-slice signature: near-identical titles, disjoint deliverables.
// …spawn-state-pruning-with-hud-pressure-s-2 (HUD metrics, never merged) must
// not read as covered by bl-hud-spawn-state-pressure-prune-20260726 (prune
// mechanics, merged) just because the titles rhyme.
func TestMergedCanonicalCovers(t *testing.T) {
	cases := []struct {
		name       string
		candidate  *BacklogItem
		canonical  *BacklogItem
		delivered  []string
		want       ScopeCoverage
		wantReason string
	}{
		{
			name:      "sliceless candidate is unknown",
			candidate: scopeItem("C"),
			canonical: scopeItem("M", "internal/spawn/controller.go"),
			delivered: []string{"internal/spawn/controller.go"},
			want:      ScopeCoverageUnknown,
		},
		{
			name: "delivered files disjoint from declared slice files",
			candidate: scopeItem("C",
				"internal/hud/monitor/spawn_pressure.go",
				"internal/hud/fleetview/spawn_pressure.go"),
			canonical: scopeItem("M"),
			delivered: []string{
				"internal/spawn/controller.go", "internal/spawn/store.go",
				"pkg/mills/pipeline/spawn_class.go",
			},
			want: ScopeCoverageDisjoint, wantReason: "no_scope_intersection",
		},
		{
			name:      "delivered file covering a declared file",
			candidate: scopeItem("C", "internal/hud/monitor/spawn_pressure.go"),
			canonical: scopeItem("M"),
			delivered: []string{
				"internal/spawn/controller.go",
				"internal/hud/monitor/spawn_pressure.go",
			},
			want: ScopeCoverageOverlap, wantReason: "internal/hud/monitor/spawn_pressure.go",
		},
		{
			name:      "declared slices are the fallback when nothing was captured",
			candidate: scopeItem("C", "internal/hud/monitor/spawn_pressure.go"),
			canonical: scopeItem("M", "internal/hud/monitor/fleet.go"),
			want:      ScopeCoverageOverlap, wantReason: "internal/hud/monitor",
		},
		{
			name:      "no capture and no declared slices is unverifiable",
			candidate: scopeItem("C", "internal/hud/monitor/spawn_pressure.go"),
			canonical: scopeItem("M"),
			want:      ScopeCoverageDisjoint, wantReason: "canonical_delivery_unverifiable",
		},
		{
			name:      "changelog-only capture stays disjoint from code slices",
			candidate: scopeItem("C", "internal/hud/monitor/spawn_pressure.go"),
			canonical: scopeItem("M"),
			delivered: []string{"changelog.d/some-slug.fixed.md"},
			want:      ScopeCoverageDisjoint, wantReason: "no_scope_intersection",
		},
		{
			name: "different target repos never cover each other",
			candidate: func() *BacklogItem {
				it := scopeItem("C", "internal/hud/monitor/spawn_pressure.go")
				it.TargetProject = "services/loom-flightdeck"
				return it
			}(),
			canonical: scopeItem("M"),
			delivered: []string{"internal/hud/monitor/spawn_pressure.go"},
			want:      ScopeCoverageDisjoint, wantReason: "target_project_mismatch",
		},
		{
			name:      "glob-declared scope overlaps via its static dir",
			candidate: scopeItem("C", "internal/hud/monitor/*.go"),
			canonical: scopeItem("M"),
			delivered: []string{"internal/hud/monitor/fleet.go"},
			want:      ScopeCoverageOverlap, wantReason: "internal/hud/monitor",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := MergedCanonicalCovers(tc.candidate, tc.canonical, tc.delivered, "services/loom-core")
			if got != tc.want {
				t.Fatalf("coverage=%s want %s (reason=%q)", got, tc.want, reason)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason=%q want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestClaimPipelineStart_FileGranularity(t *testing.T) {
	for _, tc := range []struct {
		name, active, queued, witness string
	}{
		{"siblings", "pkg/mills/policy.go", "pkg/mills/reconciler.go", ""},
		{"descendant", "pkg/mills/policy.go", "pkg/mills/spin/spin.go", ""},
		{"same file", "./pkg/mills/policy.go", "pkg/mills/policy.go", "pkg/mills/policy.go"},
		{"active glob", "pkg/mills/pipeline/*.go", "pkg/mills/pipeline/dispatcher.go", "pkg/mills/pipeline"},
		{"queued glob", "pkg/mills/pipeline/dispatcher.go", "pkg/mills/pipeline/*.go", "pkg/mills/pipeline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			active := seedClaimBacklog(t, st, "active")
			active.State = BacklogRunning
			active.Slices = []Slice{{Name: "s", Files: []string{tc.active}}}
			queued := seedClaimBacklog(t, st, "queued")
			queued.Slices = []Slice{{Name: "s", Files: []string{tc.queued}}}
			for _, item := range []*BacklogItem{active, queued} {
				if err := st.Backlog.Put(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			req := claimTestRequest(queued.ID)
			req.ExpectedRevision = queued.Revision
			_, err := st.ClaimPipelineStart(ctx, req)
			if tc.witness == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var conflict *ScopeConflictError
			if !errors.As(err, &conflict) || conflict.Witness != tc.witness || conflict.BlockerID != active.ID {
				t.Fatalf("want scope conflict, got %v", err)
			}
			if hit, witness := BacklogScopesOverlap(active, queued, ""); !hit || witness != tc.witness {
				t.Fatalf("overlap=(%v, %q), want %q", hit, witness, tc.witness)
			}
		})
	}
}

func TestScopeEnvelopeFiltersTestCommands(t *testing.T) {
	for _, command := range []string{"go test ./pkg/mills/ -run 'Scope|Envelope'", "cd internal/hud/frontend && pnpm test", "pnpm test", "npm test", "make test", "go\ttest", "go\ntest", "go\u00a0test", "a&&b", "a|b", "a;b", "a>b", ""} {
		t.Run(command, func(t *testing.T) {
			item := &BacklogItem{Slices: []Slice{{Tests: []string{command}}}}
			if got := scopeForBacklog(item); !got.empty() {
				t.Fatalf("command contributed scope: %+v", got)
			}
			left := &BacklogItem{Slices: []Slice{{Files: []string{"pkg/alpha/a.go"}, Tests: []string{command}}}}
			right := &BacklogItem{Slices: []Slice{{Files: []string{"pkg/beta/b.go"}, Tests: []string{command}}}}

			if hit, witness := BacklogScopesOverlap(left, right, "services/loom-core"); hit || witness != "" {
				t.Fatalf("command overlap: %v %q", hit, witness)
			}
		})
	}
}

func TestScopeEnvelopePreservesTestDeclarations(t *testing.T) {
	item := &BacklogItem{Slices: []Slice{{Files: []string{"dir with spaces/file.go"}, Tests: []string{"pkg/alpha/a_test.go", "pkg/beta/*_test.go"}}}}
	got := scopeForBacklog(item)
	for _, file := range []string{"dir with spaces/file.go", "pkg/alpha/a_test.go"} {
		if _, ok := got.files[file]; !ok {
			t.Errorf("missing literal %q", file)
		}
	}
	if _, ok := got.literalDirs["pkg/alpha"]; !ok {
		t.Error("missing test directory")
	}
	if _, ok := got.globDirs["pkg/beta"]; !ok {
		t.Error("missing test glob directory")
	}
}
