package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Occurrence is one test's rerun outcome, parsed from a line of gotestsum's
// --rerun-fails-report file.
//
// gotestsum writes one line per test it reran, in the form:
//
//	gitlab.example/pkg/path.TestName: 2 runs, 1 failures
//
// The distinction that matters is Failures vs Runs. A test that failed on its
// first attempt and passed on a later one reports fewer failures than runs —
// that is a flake, and the job stays green. A test that reports failures ==
// runs failed every attempt: a hard red that reruns did not and must not
// rescue.
type Occurrence struct {
	Package  string
	Test     string
	Runs     int
	Failures int
}

// ID is the stable identity of a rerun test: its fully-qualified
// package.TestName. Used as the issue dedup key.
func (o Occurrence) ID() string { return o.Package + "." + o.Test }

// Flaked reports whether the test failed at least once but ultimately passed.
func (o Occurrence) Flaked() bool { return o.Failures > 0 && o.Failures < o.Runs }

// HardFailed reports whether the test failed every attempt.
func (o Occurrence) HardFailed() bool { return o.Runs > 0 && o.Failures >= o.Runs }

// Line renders the occurrence back into gotestsum's report syntax, for
// embedding verbatim in an issue body.
func (o Occurrence) Line() string {
	return fmt.Sprintf("%s: %d runs, %d failures", o.ID(), o.Runs, o.Failures)
}

var rerunLineRE = regexp.MustCompile(`^(\S+): (\d+) runs, (\d+) failures\s*$`)

// ParseRerunReport reads a gotestsum --rerun-fails-report file. Unparseable
// lines are skipped rather than fatal: this runs in after_script, where
// crashing on an unexpected gotestsum output tweak would lose the flake signal
// entirely for no benefit.
func ParseRerunReport(r io.Reader) ([]Occurrence, error) {
	var out []Occurrence
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m := rerunLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Root Go test names are identifiers and contain no dots, while import
		// paths routinely do (gitlab.flexinfer.ai/...). Splitting on the LAST
		// dot is therefore the correct package/test boundary.
		dot := strings.LastIndex(m[1], ".")
		if dot <= 0 || dot == len(m[1])-1 {
			continue
		}
		runs, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		failures, err := strconv.Atoi(m[3])
		if err != nil {
			continue
		}
		out = append(out, Occurrence{
			Package:  m[1][:dot],
			Test:     m[1][dot+1:],
			Runs:     runs,
			Failures: failures,
		})
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read rerun report: %w", err)
	}
	return out, nil
}

// IssueTitle is the deduplicating title for a flaked test. Deliberately keyed
// on the bare test name (not the import path) so the same test moving between
// packages keeps one issue.
func IssueTitle(o Occurrence) string { return "flake: " + o.Test }

// FlakeLabel marks every issue this tool files. The weekly digest lists open
// issues carrying it.
const FlakeLabel = "flaky-test"

// dedupMarker is an invisible, machine-readable line kept in every flake
// issue's description. It carries the recurrence count so the weekly digest
// can rank issues from a single list call instead of walking each issue's
// notes. Mirrors the mills escalator's dedup-marker pattern
// (pkg/mills/pipeline.EscalationDedupMarker).
var markerRE = regexp.MustCompile(`(?m)^<!-- loom-flake-dedup: (\S+) hits=(\d+) last=(\S+) -->$`)

// Marker renders the dedup marker line.
func Marker(id string, hits int, last time.Time) string {
	return fmt.Sprintf("<!-- loom-flake-dedup: %s hits=%d last=%s -->",
		id, hits, last.UTC().Format(time.RFC3339))
}

// MarkerState is the decoded dedup marker.
type MarkerState struct {
	ID   string
	Hits int
	Last string
}

// ParseMarker extracts the dedup marker from an issue description.
func ParseMarker(description string) (MarkerState, bool) {
	m := markerRE.FindStringSubmatch(description)
	if m == nil {
		return MarkerState{}, false
	}
	hits, err := strconv.Atoi(m[2])
	if err != nil {
		return MarkerState{}, false
	}
	return MarkerState{ID: m[1], Hits: hits, Last: m[3]}, true
}

// ReplaceMarker swaps the marker line in a description, leaving every other
// byte untouched so human edits to the issue body survive a recurrence. When
// no marker is present (a hand-filed issue that later got the label), the
// marker is appended.
func ReplaceMarker(description, marker string) string {
	if markerRE.MatchString(description) {
		return markerRE.ReplaceAllLiteralString(description, marker)
	}
	if description != "" && !strings.HasSuffix(description, "\n") {
		description += "\n"
	}
	return description + "\n" + marker + "\n"
}

// Context is the CI provenance stamped into issue bodies and comments.
type Context struct {
	PipelineURL string
	JobURL      string
	Ref         string
	CommitSHA   string
	Now         time.Time
}

// NewIssueBody composes the description for a first-time flake issue.
func NewIssueBody(o Occurrence, ctx Context) string {
	var b strings.Builder
	b.WriteString("Filed automatically by the `test:unit` flake quarantine.\n\n")
	fmt.Fprintf(&b, "**Test**: `%s`\n", o.Test)
	fmt.Fprintf(&b, "**Package**: `%s`\n", o.Package)
	fmt.Fprintf(&b, "**First seen**: %s\n\n", ctx.Now.UTC().Format(time.RFC3339))

	b.WriteString("### What happened\n\n")
	b.WriteString("This test **failed on its first attempt and passed on a rerun** in CI. ")
	b.WriteString("The pipeline was allowed to stay green so one timing-dependent test ")
	b.WriteString("cannot block every image build the way ")
	b.WriteString("`TestStopSpawnLateStartCleanupFailureRetainsRetryablePod` did on 2026-08-02 ")
	b.WriteString("(~10h of blocked deploys). The rerun is **not** an all-clear: a test that ")
	b.WriteString("only sometimes passes is a real defect, usually a race or an unsynchronised ")
	b.WriteString("timing assumption, and this repo supervises spawns under exactly those conditions.\n\n")

	b.WriteString("### Evidence\n\n")
	b.WriteString(occurrenceEvidence(o, ctx))

	b.WriteString("\n### How to close this\n\n")
	b.WriteString("Close this issue only from a merge request that **fixes** the flake, ")
	b.WriteString("referencing this issue. Do not close it because the test has been quiet: ")
	b.WriteString("the recurrence counter in this issue is the signal, and a silent flake ")
	b.WriteString("re-opens the same 10-hour outage the next time it lands on `main`.\n\n")
	b.WriteString("If the test is genuinely unfixable in place, delete or rewrite it — ")
	b.WriteString("do not add it to an ignore list.\n\n")
	b.WriteString("Reproduce locally with:\n\n")
	fmt.Fprintf(&b, "```bash\ngo test -race -count=20 -run '^%s$' %s\n```\n\n", o.Test, o.Package)

	b.WriteString(Marker(o.ID(), 1, ctx.Now))
	b.WriteString("\n")
	return b.String()
}

// RecurrenceComment composes the note appended to an existing flake issue.
func RecurrenceComment(o Occurrence, ctx Context, hits int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Flaked again at %s (occurrence #%d).\n\n",
		ctx.Now.UTC().Format(time.RFC3339), hits)
	b.WriteString(occurrenceEvidence(o, ctx))
	return b.String()
}

func occurrenceEvidence(o Occurrence, ctx Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- Rerun report: `%s`\n", o.Line())
	if ctx.Ref != "" {
		fmt.Fprintf(&b, "- Branch: `%s`\n", ctx.Ref)
	}
	if ctx.CommitSHA != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", ctx.CommitSHA)
	}
	if ctx.PipelineURL != "" {
		fmt.Fprintf(&b, "- Pipeline: %s\n", ctx.PipelineURL)
	}
	if ctx.JobURL != "" {
		fmt.Fprintf(&b, "- Job: %s\n", ctx.JobURL)
	}
	return b.String()
}
