package store

import "testing"

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
			b:    []string{"pkg/mills/pipeline/escalate.go", "changelog.d/*.md"},
			want: true, witness: "pkg/mills/pipeline",
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
