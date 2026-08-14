package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DigestLabel marks the rolling digest issue. Deliberately NOT FlakeLabel: a
// digest issue carrying `flaky-test` would list itself. Equally deliberately
// not `flaky-test-digest` — that has `flaky-test` as a SUBSTRING, which is
// safe against GitLab's exact label matching but silently wrong for any
// grep-shaped tooling that reads the label set later.
const DigestLabel = "flake-digest"

// DigestTitle is the stable title of the single rolling digest issue. One
// long-lived issue beats a new issue per week: the link stays valid in
// bookmarks and groomer/backlog triage, and the comment history is the trend.
const DigestTitle = "flake digest: loom-core test:unit"

// DigestRow is one open flake issue as it appears in the digest table.
type DigestRow struct {
	Title  string
	IID    int
	WebURL string
	TestID string
	Hits   int
	Last   string
}

// BuildDigestRows turns open `flaky-test` issues into ranked digest rows.
// Issues without a dedup marker (hand-filed, or filed before this tooling)
// still appear, with an unknown hit count, so triage sees the whole label.
func BuildDigestRows(issues []Issue) []DigestRow {
	rows := make([]DigestRow, 0, len(issues))
	for _, issue := range issues {
		row := DigestRow{
			Title:  issue.Title,
			IID:    issue.IID,
			WebURL: issue.WebURL,
		}
		if state, ok := ParseMarker(issue.Description); ok {
			row.TestID = state.ID
			row.Hits = state.Hits
			row.Last = state.Last
		}
		rows = append(rows, row)
	}
	// Loudest first: most recurrences, then most recently seen, then a stable
	// tiebreak so the digest body does not churn between identical weeks.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Hits != rows[j].Hits {
			return rows[i].Hits > rows[j].Hits
		}
		if rows[i].Last != rows[j].Last {
			return rows[i].Last > rows[j].Last
		}
		return rows[i].IID < rows[j].IID
	})
	return rows
}

// RenderDigest composes the digest body from ranked rows.
func RenderDigest(rows []DigestRow, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weekly flake digest — generated %s.\n\n", now.UTC().Format(time.RFC3339))

	if len(rows) == 0 {
		b.WriteString("**No open `flaky-test` issues.** ")
		b.WriteString("Every test that failed in `test:unit` this period failed deterministically ")
		b.WriteString("(and redded its pipeline) or passed first try.\n\n")
		b.WriteString(digestFooter())
		return b.String()
	}

	total := 0
	for _, r := range rows {
		total += r.Hits
	}
	fmt.Fprintf(&b, "**%d open flake(s), %d recorded rerun-pass occurrence(s).**\n\n", len(rows), total)

	b.WriteString("| Hits | Test | Issue | Last seen |\n")
	b.WriteString("|-----:|------|-------|-----------|\n")
	for _, r := range rows {
		hits := "?"
		if r.Hits > 0 {
			hits = fmt.Sprintf("%d", r.Hits)
		}
		test := r.TestID
		if test == "" {
			test = r.Title
		}
		last := r.Last
		if last == "" {
			last = "unknown"
		}
		fmt.Fprintf(&b, "| %s | `%s` | #%d | %s |\n", hits, test, r.IID, last)
	}

	b.WriteString("\n### Triage guidance\n\n")
	b.WriteString("Rank by hits. A test at the top of this table has repeatedly failed-then-passed ")
	b.WriteString("in CI, which means it is a live race or timing bug that has so far only been ")
	b.WriteString("absorbed by the rerun — not fixed. Pull the top entry into the backlog.\n\n")
	b.WriteString(digestFooter())
	return b.String()
}

func digestFooter() string {
	return "<!-- loom-flake-digest -->\n"
}

// RunDigest lists open flake issues and upserts the rolling digest issue.
func RunDigest(ctx context.Context, gl *GitLab, now time.Time, log func(string, ...any)) error {
	issues, err := gl.ListOpenByLabel(ctx, FlakeLabel)
	if err != nil {
		return fmt.Errorf("list %q issues: %w", FlakeLabel, err)
	}
	rows := BuildDigestRows(issues)
	body := RenderDigest(rows, now)

	log("flake digest: %d open %q issue(s)", len(rows), FlakeLabel)
	for _, r := range rows {
		test := r.TestID
		if test == "" {
			test = r.Title
		}
		log("  hits=%-4d %s (#%d)", r.Hits, test, r.IID)
	}

	existing, err := gl.FindOpenByTitle(ctx, DigestLabel, DigestTitle)
	if err != nil {
		return fmt.Errorf("find digest issue: %w", err)
	}
	if existing == nil {
		created, err := gl.CreateIssue(ctx, DigestTitle, body, []string{DigestLabel})
		if err != nil {
			return fmt.Errorf("create digest issue: %w", err)
		}
		log("filed digest issue #%d %s", created.IID, created.WebURL)
		return nil
	}

	// Description carries the CURRENT table; the comment stream carries the
	// week-over-week history.
	if err := gl.UpdateDescription(ctx, existing.IID, body); err != nil {
		return fmt.Errorf("update digest issue #%d: %w", existing.IID, err)
	}
	if err := gl.Comment(ctx, existing.IID, body); err != nil {
		return fmt.Errorf("comment digest issue #%d: %w", existing.IID, err)
	}
	log("updated digest issue #%d %s", existing.IID, existing.WebURL)
	return nil
}
