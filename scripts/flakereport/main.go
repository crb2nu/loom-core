// Command flakereport turns gotestsum rerun evidence into first-class,
// deduplicated flake telemetry.
//
// Background: on 2026-08-02 one timing-dependent test intermittently redded
// services/loom-core `main` and blocked every image build for ~10 hours. The
// fix for the CLASS is gotestsum's `--rerun-fails` in `test:unit` — but a
// rerun that silently swallows a race is worse than the outage it prevents.
// So every rerun-pass is recorded here instead of discarded.
//
// Modes:
//
//	report   Parse a gotestsum --rerun-fails-report file. Print a loud
//	         FLAKY-TEST-DETECTED line per test that failed then passed, and
//	         file (or update) one deduplicated `flaky-test` GitLab issue per
//	         such test. Tests that failed EVERY attempt are reported as hard
//	         failures and never get a flake issue — the job is already red.
//
//	digest   List open `flaky-test` issues with their recurrence counts and
//	         upsert the rolling weekly digest issue. Run from a scheduled
//	         pipeline; feeds groomer/backlog triage.
//
// This command is invoked from `test:unit`'s after_script, which means two
// hard rules. (1) It must never turn a green pipeline red: GitLab API
// problems, a missing token, and malformed input all warn and exit 0. The
// job's verdict belongs to the tests, not to issue bookkeeping. (2) It must
// still print the flake evidence when the API is unreachable, so the job log
// remains a complete record on its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const apiTimeout = 2 * time.Minute

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "report":
		os.Exit(runReportCmd(os.Args[2:]))
	case "digest":
		os.Exit(runDigestCmd(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "flakereport: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: flakereport <report|digest> [flags]

  report --rerun-report FILE   record rerun-passes as flaky-test issues
  digest                       refresh the rolling weekly flake digest issue

Environment (all supplied by GitLab CI):
  GITLAB_TOKEN / FLAKE_ISSUE_TOKEN   project access token with api scope
  CI_API_V4_URL, CI_PROJECT_ID       GitLab API target
  CI_PIPELINE_URL, CI_JOB_URL        provenance stamped into issues
  CI_COMMIT_REF_NAME, CI_COMMIT_SHORT_SHA
`)
}

// logf writes progress to stdout so it lands in the job log.
func logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WARNING: "+format+"\n", args...)
}

// client builds a GitLab client from the CI environment, or returns nil with
// a human-readable reason when the environment is not configured for issue
// filing. A nil client is not an error: the caller still prints evidence.
func client() (*GitLab, string) {
	token := firstNonEmpty(os.Getenv("FLAKE_ISSUE_TOKEN"), os.Getenv("GITLAB_TOKEN"))
	if token == "" {
		return nil, "no FLAKE_ISSUE_TOKEN/GITLAB_TOKEN in the environment " +
			"(CI_JOB_TOKEN cannot create issues); flake evidence printed but not filed"
	}
	api := os.Getenv("CI_API_V4_URL")
	if api == "" {
		return nil, "CI_API_V4_URL is unset; flake evidence printed but not filed"
	}
	project := firstNonEmpty(os.Getenv("CI_PROJECT_ID"), os.Getenv("CI_PROJECT_PATH"))
	if project == "" {
		return nil, "CI_PROJECT_ID/CI_PROJECT_PATH are unset; flake evidence printed but not filed"
	}
	return NewGitLab(api, project, token), ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func ciContext(now time.Time) Context {
	return Context{
		PipelineURL: os.Getenv("CI_PIPELINE_URL"),
		JobURL:      os.Getenv("CI_JOB_URL"),
		Ref:         os.Getenv("CI_COMMIT_REF_NAME"),
		CommitSHA:   os.Getenv("CI_COMMIT_SHORT_SHA"),
		Now:         now,
	}
}

func runReportCmd(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	path := fs.String("rerun-report", "rerun-report.txt", "gotestsum --rerun-fails-report file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	f, err := os.Open(*path)
	if err != nil {
		if os.IsNotExist(err) {
			logf("No rerun report at %s — no tests were rerun.", *path)
			return 0
		}
		warnf("open %s: %v", *path, err)
		return 0
	}
	defer func() { _ = f.Close() }()

	occurrences, err := ParseRerunReport(f)
	if err != nil {
		warnf("parse %s: %v", *path, err)
	}
	if len(occurrences) == 0 {
		logf("No tests were rerun — nothing to report.")
		return 0
	}

	var flaked, hard []Occurrence
	for _, o := range occurrences {
		switch {
		case o.Flaked():
			flaked = append(flaked, o)
		case o.HardFailed():
			hard = append(hard, o)
		}
	}

	// The loud lines come first and unconditionally: even if every API call
	// below fails, the job log is a complete record of what flaked.
	for _, o := range flaked {
		logf("FLAKY-TEST-DETECTED: %s (%d/%d attempts failed, then passed) [%s]",
			o.Test, o.Failures, o.Runs, o.Package)
	}
	for _, o := range hard {
		logf("HARD-FAILURE: %s failed all %d attempts [%s] — not a flake, the job is red",
			o.Test, o.Runs, o.Package)
	}

	if len(flaked) == 0 {
		logf("No rerun-passes: every rerun test failed every attempt.")
		return 0
	}

	gl, reason := client()
	if gl == nil {
		warnf("%s", reason)
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	now := time.Now().UTC()

	for _, o := range flaked {
		if err := fileFlake(ctx, gl, o, ciContext(now)); err != nil {
			// One test's filing failure must not skip the rest, and must not
			// fail the job.
			warnf("filing flake issue for %s: %v", o.ID(), err)
		}
	}
	return 0
}

// fileFlake creates or updates the deduplicated issue for one flaked test.
func fileFlake(ctx context.Context, gl *GitLab, o Occurrence, cctx Context) error {
	title := IssueTitle(o)
	existing, err := gl.FindOpenByTitle(ctx, FlakeLabel, title)
	if err != nil {
		// Fail OPEN into a fresh issue rather than dropping the signal, the
		// same way the mills escalator treats a dedup lookup error.
		warnf("dedup lookup for %q failed (%v); filing a fresh issue", title, err)
		existing = nil
	}

	if existing == nil {
		created, err := gl.CreateIssue(ctx, title, NewIssueBody(o, cctx), []string{FlakeLabel})
		if err != nil {
			return fmt.Errorf("create issue: %w", err)
		}
		logf("  filed %s -> #%d %s", title, created.IID, created.WebURL)
		return nil
	}

	hits := 1
	if state, ok := ParseMarker(existing.Description); ok {
		hits = state.Hits + 1
	}
	// Rewrite only the marker line so human edits to the body survive.
	updated := ReplaceMarker(existing.Description, Marker(o.ID(), hits, cctx.Now))
	if err := gl.UpdateDescription(ctx, existing.IID, updated); err != nil {
		warnf("bumping hit count on #%d: %v", existing.IID, err)
	}
	if err := gl.Comment(ctx, existing.IID, RecurrenceComment(o, cctx, hits)); err != nil {
		return fmt.Errorf("comment on #%d: %w", existing.IID, err)
	}
	logf("  updated %s -> #%d (occurrence #%d) %s", title, existing.IID, hits, existing.WebURL)
	return nil
}

func runDigestCmd(args []string) int {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	gl, reason := client()
	if gl == nil {
		warnf("%s", reason)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := RunDigest(ctx, gl, time.Now().UTC(), logf); err != nil {
		warnf("flake digest: %v", err)
	}
	return 0
}
