package council

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stubFactoryExhaust is the council-side FactoryExhaustSource fake. `items`
// seeds the snapshot, `err` forces the degradation branch, and the recorded
// arguments prove Compile passes the configured bounds through rather than
// silently substituting its own.
type stubFactoryExhaust struct {
	items []FactoryExhaustItem
	err   error

	calls int
	since time.Time
	limit int
}

func (s *stubFactoryExhaust) ListFactoryExhaust(_ context.Context, since time.Time, limit int) ([]FactoryExhaustItem, error) {
	s.calls++
	s.since = since
	s.limit = limit
	if s.err != nil {
		return nil, s.err
	}
	return s.items, nil
}

// exhaustFixture builds n flake issues plus one audit digest, aged one hour
// apart so the newest-first ordering is unambiguous. IIDs ascend with recency.
func exhaustFixture(n int) []FactoryExhaustItem {
	out := make([]FactoryExhaustItem, 0, n+1)
	for i := 0; i < n; i++ {
		age := time.Duration(i) * time.Hour
		out = append(out, FactoryExhaustItem{
			Kind:      FactoryExhaustFlakyTest,
			IID:       int64(500 + n - i),
			Title:     fmt.Sprintf("flake: TestCouncilBrief_Section%d", i),
			WebURL:    fmt.Sprintf("https://gitlab.flexinfer.ai/services/loom-core/-/issues/%d", 500+n-i),
			CreatedAt: fixedTime().Add(-48 * time.Hour),
			UpdatedAt: fixedTime().Add(-age),
		})
	}
	out = append(out, FactoryExhaustItem{
		Kind:      FactoryExhaustAuditDigest,
		IID:       499,
		Title:     "Audit advisory digest — 2026-04-25 (UTC)",
		WebURL:    "https://gitlab.flexinfer.ai/services/loom-core/-/issues/499",
		CreatedAt: fixedTime().Add(-30 * time.Hour),
		UpdatedAt: fixedTime().Add(-30 * time.Hour),
	})
	return out
}

func compileWithExhaust(t *testing.T, src *stubFactoryExhaust, mutate func(*BriefSources)) *Brief {
	t.Helper()
	st, repo := seedBriefStore(t)
	sources := BriefSources{Store: st, RepoRoot: repo, Now: fixedTime, FactoryExhaust: src}
	if mutate != nil {
		mutate(&sources)
	}
	b, err := Compile(context.Background(), sources)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return b
}

func TestCompile_RendersFactoryExhaustSection(t *testing.T) {
	src := &stubFactoryExhaust{items: exhaustFixture(2)}
	b := compileWithExhaust(t, src, nil)

	if src.calls != 1 {
		t.Fatalf("ListFactoryExhaust called %d times, want exactly 1", src.calls)
	}
	if !strings.Contains(b.Markdown, FactoryExhaustHeading) {
		t.Fatalf("brief is missing the exhaust heading:\n%s", b.Markdown)
	}
	// Issue refs, not just titles: the council needs something to cite.
	for _, want := range []string{"`#502`", "`#501`", "`#499`", "/-/issues/499"} {
		if !strings.Contains(b.Markdown, want) {
			t.Errorf("brief is missing exhaust ref %q:\n%s", want, b.Markdown)
		}
	}
	for _, want := range []string{"kind=`flaky_test`", "kind=`audit_digest`"} {
		if !strings.Contains(b.Markdown, want) {
			t.Errorf("brief is missing exhaust kind %q:\n%s", want, b.Markdown)
		}
	}
	if b.SourceCounts.FactoryExhaustItems != 3 {
		t.Errorf("SourceCounts.FactoryExhaustItems = %d, want 3", b.SourceCounts.FactoryExhaustItems)
	}
	if b.SourceCounts.FactoryExhaustUnavailable {
		t.Error("a successful fetch marked the source unavailable")
	}
	if strings.Contains(b.Markdown, FactoryExhaustUnavailableBody) {
		t.Error("a successful fetch rendered the unavailable body")
	}
}

// The section is evidence, not an instruction to act: the preamble must leave
// the proposal decision with the council rather than mandating fixes.
func TestCompile_FactoryExhaustSectionLeavesTheDecisionToTheCouncil(t *testing.T) {
	b := compileWithExhaust(t, &stubFactoryExhaust{items: exhaustFixture(1)}, nil)
	body := factoryExhaustSectionBody(t, b)
	if !strings.Contains(body, "ignore them when they do not") {
		t.Errorf("exhaust preamble does not leave the decision to the council:\n%s", body)
	}
}

func TestCompile_FactoryExhaustHonorsBounds(t *testing.T) {
	src := &stubFactoryExhaust{items: exhaustFixture(20)}
	b := compileWithExhaust(t, src, func(s *BriefSources) {
		s.FactoryExhaustLimit = 3
		s.FactoryExhaustLookback = 48 * time.Hour
	})

	if src.limit != 3 {
		t.Errorf("source received limit %d, want 3", src.limit)
	}
	if want := fixedTime().Add(-48 * time.Hour); !src.since.Equal(want) {
		t.Errorf("source received since %s, want %s", src.since, want)
	}
	if b.SourceCounts.FactoryExhaustItems != 3 {
		t.Fatalf("FactoryExhaustItems = %d, want 3 (over-returning source must be re-bounded)",
			b.SourceCounts.FactoryExhaustItems)
	}
	// Bounding must keep the NEWEST, which is why the brief re-sorts before it
	// truncates instead of trusting the source's order.
	body := factoryExhaustSectionBody(t, b)
	if n := strings.Count(body, "\n- "); n != 3 {
		t.Errorf("rendered %d exhaust lines, want 3:\n%s", n, body)
	}
	if !strings.Contains(body, "`#520`") {
		t.Errorf("bounded section dropped the newest item:\n%s", body)
	}
	if strings.Contains(body, "`#501`") {
		t.Errorf("bounded section kept an older item past the cap:\n%s", body)
	}
}

func TestCompile_FactoryExhaustDefaultBounds(t *testing.T) {
	src := &stubFactoryExhaust{items: exhaustFixture(30)}
	b := compileWithExhaust(t, src, nil)

	if src.limit != defaultFactoryExhaustLimit {
		t.Errorf("default limit = %d, want %d", src.limit, defaultFactoryExhaustLimit)
	}
	if want := fixedTime().Add(-defaultFactoryExhaustLookback); !src.since.Equal(want) {
		t.Errorf("default since = %s, want %s", src.since, want)
	}
	if b.SourceCounts.FactoryExhaustItems != defaultFactoryExhaustLimit {
		t.Errorf("FactoryExhaustItems = %d, want the default cap %d",
			b.SourceCounts.FactoryExhaustItems, defaultFactoryExhaustLimit)
	}
}

// A fetch failure must degrade the SECTION, never the brief: an omitted section
// would read to the council as "the factory is clean".
func TestCompile_FactoryExhaustFetchFailureDegrades(t *testing.T) {
	src := &stubFactoryExhaust{err: errors.New("gitlab: 503 service unavailable")}
	b := compileWithExhaust(t, src, nil)

	if !strings.Contains(b.Markdown, FactoryExhaustUnavailableBody) {
		t.Fatalf("fetch failure did not render the unavailable body:\n%s", b.Markdown)
	}
	if !b.SourceCounts.FactoryExhaustUnavailable {
		t.Error("fetch failure did not set SourceCounts.FactoryExhaustUnavailable")
	}
	if b.SourceCounts.FactoryExhaustItems != 0 {
		t.Errorf("FactoryExhaustItems = %d, want 0 on a failed fetch", b.SourceCounts.FactoryExhaustItems)
	}
	// Everything else still compiled — this is the intents-missing degradation
	// shape, not an abort.
	if !strings.Contains(b.Markdown, "Roadmap intents") {
		t.Errorf("fetch failure damaged the rest of the brief:\n%s", b.Markdown)
	}
	if b.SourceCounts.Intents == 0 {
		t.Error("fetch failure suppressed the roadmap intents count")
	}
}

// Policy-off is expressed by leaving the source nil (the runner's shape). The
// section must be absent entirely — not empty, and not "unavailable", which
// would misreport a deliberate opt-out as an outage.
func TestCompile_FactoryExhaustDisabledOmitsSection(t *testing.T) {
	st, repo := seedBriefStore(t)
	b, err := Compile(context.Background(), BriefSources{Store: st, RepoRoot: repo, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(b.Markdown, FactoryExhaustHeading) {
		t.Errorf("nil exhaust source still rendered the section:\n%s", b.Markdown)
	}
	if strings.Contains(b.Markdown, FactoryExhaustUnavailableBody) {
		t.Errorf("nil exhaust source rendered the unavailable body:\n%s", b.Markdown)
	}
	for _, s := range b.Sections {
		if s.Heading == FactoryExhaustHeading {
			t.Errorf("nil exhaust source appended a %q section", s.Heading)
		}
	}
	if b.SourceCounts.FactoryExhaustItems != 0 || b.SourceCounts.FactoryExhaustUnavailable {
		t.Errorf("nil exhaust source left counts %+v", b.SourceCounts)
	}
}

// An empty (but healthy) source is also silent: nothing to report is not the
// same fact as a failure, and the council should not read a heading for it.
func TestCompile_FactoryExhaustEmptyOmitsSection(t *testing.T) {
	b := compileWithExhaust(t, &stubFactoryExhaust{}, nil)
	if strings.Contains(b.Markdown, FactoryExhaustHeading) {
		t.Errorf("empty exhaust snapshot rendered a section:\n%s", b.Markdown)
	}
	if b.SourceCounts.FactoryExhaustUnavailable {
		t.Error("empty exhaust snapshot marked the source unavailable")
	}
}

func TestSortFactoryExhaust_NewestFirstWithCreatedAtFallback(t *testing.T) {
	items := []FactoryExhaustItem{
		{IID: 1, UpdatedAt: fixedTime().Add(-10 * time.Hour)},
		{IID: 2, CreatedAt: fixedTime().Add(-1 * time.Hour)}, // no UpdatedAt
		{IID: 3, UpdatedAt: fixedTime().Add(-5 * time.Hour)},
		{IID: 4, UpdatedAt: fixedTime().Add(-5 * time.Hour)}, // tie with 3
	}
	SortFactoryExhaust(items)
	got := []int64{items[0].IID, items[1].IID, items[2].IID, items[3].IID}
	want := []int64{2, 4, 3, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sort order = %v, want %v (ties break on descending iid)", got, want)
		}
	}
}

func TestFactoryExhaustItem_Ref(t *testing.T) {
	if got := (FactoryExhaustItem{IID: 42}).Ref(); got != "#42" {
		t.Errorf("Ref() = %q, want %q", got, "#42")
	}
	url := "https://gitlab.flexinfer.ai/services/loom-core/-/issues/42"
	if got := (FactoryExhaustItem{WebURL: url}).Ref(); got != url {
		t.Errorf("iid-less Ref() = %q, want the web url", got)
	}
}

// factoryExhaustSectionBody returns the rendered body of the exhaust section,
// failing the test if it is absent.
func factoryExhaustSectionBody(t *testing.T, b *Brief) string {
	t.Helper()
	for _, s := range b.Sections {
		if s.Heading == FactoryExhaustHeading {
			return s.Body
		}
	}
	t.Fatalf("brief has no %q section: %+v", FactoryExhaustHeading, b.Sections)
	return ""
}
