package finishing

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRepo struct {
	entries   []TreeEntry
	treeErr   error
	commitErr error
}

func (f fakeRepo) ListTree(context.Context, string, string, string, bool) ([]TreeEntry, error) {
	return f.entries, f.treeErr
}
func (f fakeRepo) NewestPathCommit(context.Context, string, string, string) (*time.Time, error) {
	return nil, f.commitErr
}

func TestCheckDocsMirror(t *testing.T) {
	src := []TreeEntry{{"docs/a.md", "blob", "a"}, {"docs/b.md", "blob", "b"}, {"docs/nav.yaml", "blob", "x"}, {"docs/ROADMAP_RECONCILIATION_x.md", "blob", "x"}}
	cases := []struct {
		name                  string
		mirror                []TreeEntry
		missing, stale, extra int
		state                 string
	}{
		{"fresh", []TreeEntry{{"content/site/a.md", "blob", "a"}, {"content/site/b.md", "blob", "b"}}, 0, 0, 0, StateFresh},
		{"all drift classes", []TreeEntry{{"content/site/a.md", "blob", "z"}, {"content/site/c.md", "blob", "c"}}, 1, 1, 1, StateDrifted},
		{"ignored", []TreeEntry{{"content/site/a.md", "blob", "a"}, {"content/site/b.md", "blob", "b"}, {"content/site/nav.yaml", "blob", "z"}, {"content/site/roadmap-reconciliation-x.md", "blob", "z"}}, 0, 0, 0, StateFresh},
		{"empty mirror", nil, 2, 0, 0, StateDrifted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDocsMirror(context.Background(), func(context.Context) ([]TreeEntry, error) { return src, nil }, fakeRepo{entries: tc.mirror}, fakeRepo{}, "p", "main", "content/site", time.Now())
			if got.Missing != tc.missing || got.Stale != tc.stale || got.Extra != tc.extra || got.State != tc.state {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestCheckDocsMirrorFailuresAreUnknown(t *testing.T) {
	got := CheckDocsMirror(context.Background(), func(context.Context) ([]TreeEntry, error) { return nil, errors.New("source unavailable") }, fakeRepo{}, fakeRepo{}, "p", "main", "content/site", time.Now())
	if got.State != StateUnknown || got.Reason != "source unavailable" {
		t.Fatalf("got %+v", got)
	}
	got = CheckDocsMirror(context.Background(), func(context.Context) ([]TreeEntry, error) { return nil, nil }, fakeRepo{treeErr: errors.New("api unavailable")}, fakeRepo{}, "p", "main", "content/site", time.Now())
	if got.State != StateUnknown || got.Reason != "api unavailable" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseLSTree(t *testing.T) {
	got, err := ParseLSTree("100644 blob abc123\tdocs/a.md\n")
	if err != nil || len(got) != 1 || got[0].BlobSHA != "abc123" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
