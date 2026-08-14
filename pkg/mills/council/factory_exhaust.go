package council

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FactoryExhaustKind partitions the mill's own machine-readable maintenance
// demand by which producer filed it. Both kinds are issues this factory opened
// about itself; they differ in what a fix looks like, so the brief labels them
// separately and the metric counts them apart.
type FactoryExhaustKind string

const (
	// FactoryExhaustFlakyTest is one quarantined test, filed per test name by
	// scripts/flakereport (title "flake: <TestName>", label "flaky-test").
	FactoryExhaustFlakyTest FactoryExhaustKind = "flaky_test"

	// FactoryExhaustAuditDigest is a rolling audit-advisory digest, filed one
	// per UTC day by pkg/mills/audit (title "Audit advisory digest — <date>
	// (UTC)", label "audit-digest"). Its findings arrive as comments, so the
	// brief surfaces the digest as a pointer rather than trying to inline it.
	FactoryExhaustAuditDigest FactoryExhaustKind = "audit_digest"
)

// Labels the exhaust source queries. Both are duplicated at their producers
// (scripts/flakereport.FlakeLabel, pipeline.AuditDigestLabel) — the producers
// are a CI-side script and the pipeline package respectively, and neither is
// importable from here without a dependency the council does not want. Kept as
// literals in one place on the consumer side so drift shows up in one file.
const (
	FactoryExhaustFlakyTestLabel   = "flaky-test"
	FactoryExhaustAuditDigestLabel = "audit-digest"
)

// FactoryExhaustItem is one piece of the factory's own exhaust: an issue this
// mill filed about its own health that is still open. Minimal by design — the
// brief renders a reference, not the issue body, because the council's job here
// is to notice the demand exists, not to fix it from the brief.
type FactoryExhaustItem struct {
	Kind      FactoryExhaustKind
	IID       int64
	Title     string
	WebURL    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ref is the audit/brief shorthand for one exhaust item: the GitLab issue
// shorthand when we have an iid, the web url otherwise.
func (i FactoryExhaustItem) Ref() string {
	if i.IID > 0 {
		return "#" + strconv.FormatInt(i.IID, 10)
	}
	return strings.TrimSpace(i.WebURL)
}

// FactoryExhaustSource lists the open self-maintenance issues the factory has
// filed since a cutoff, newest first, capped at limit. Consumer-side interface
// (same shape as MergedWorkSource) so the brief stays decoupled from GitLab:
// *clients.GitLabClient implements it, tests use a stub, and a nil source
// simply omits the section.
type FactoryExhaustSource interface {
	ListFactoryExhaust(ctx context.Context, since time.Time, limit int) ([]FactoryExhaustItem, error)
}

// Bounds on one exhaust snapshot. The brief is a planning header, not a work
// queue: ten open items is enough for the council to notice a backlog of
// self-maintenance without crowding out roadmap intents, and 14 days matches
// the merged-work lookback so both grounding corpora describe the same era.
const (
	defaultFactoryExhaustLimit    = 10
	defaultFactoryExhaustLookback = 14 * 24 * time.Hour
)

// FactoryExhaustUnavailableBody is the section body rendered when the source
// errored. Stated rather than omitted, deliberately: an omitted section reads
// to the council as "the factory is clean", which is the opposite of what a
// failed fetch means. Mirrors IntentsMissingMarker in being a fixed string the
// tests and any downstream reader can match exactly.
const FactoryExhaustUnavailableBody = "_(exhaust source unavailable — the factory's open flaky-test and audit-advisory issues could not be listed this tick; absence of items below is NOT evidence the factory is clean)_"

// FactoryExhaustHeading is the brief section label. Exported so tests and any
// brief consumer match the same string.
const FactoryExhaustHeading = "Factory exhaust (open self-maintenance demand)"

// renderFactoryExhaust formats the exhaust items as one reference per line,
// grouped implicitly by the ordering the source returned (newest first). The
// preamble tells the council what it is allowed to do with them: these are
// evidence, and proposing against them is a choice the council makes under the
// same guards as any other proposal.
func renderFactoryExhaust(items []FactoryExhaustItem, window time.Duration, limit int) string {
	var b strings.Builder
	// State the cap alongside the count: at the cap the list is truncated, and
	// the council should read it as "at least this much" rather than "all".
	fmt.Fprintf(&b, "Open issues this factory filed about itself in the last %s — %d shown, newest first, capped at %d. These are machine-filed maintenance demand that no human has triaged; propose fixes for them when they outrank the roadmap intents above, and ignore them when they do not.\n\n",
		window.Round(time.Hour), len(items), limit)
	for _, it := range items {
		fmt.Fprintf(&b, "- `%s` kind=`%s` — %s (%s)\n", it.Ref(), it.Kind, it.Title, it.WebURL)
	}
	return b.String()
}

// SortFactoryExhaust orders items newest-first on UpdatedAt (falling back to
// CreatedAt), then by descending iid so the ordering is total and the rendered
// section is byte-stable for a given snapshot. Callers bound the result AFTER
// this sort, so "newest 10" means newest across both labels rather than the
// first ten the API happened to return. Exported because the GitLab source
// merges two label queries and must rank them by the same rule the brief does.
func SortFactoryExhaust(items []FactoryExhaustItem) {
	sort.SliceStable(items, func(a, b int) bool {
		at, bt := factoryExhaustRecency(items[a]), factoryExhaustRecency(items[b])
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return items[a].IID > items[b].IID
	})
}

func factoryExhaustRecency(i FactoryExhaustItem) time.Time {
	if !i.UpdatedAt.IsZero() {
		return i.UpdatedAt
	}
	return i.CreatedAt
}
