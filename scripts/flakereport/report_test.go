package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseRerunReport(t *testing.T) {
	// Line shapes taken verbatim from gotestsum v1.13.0's
	// --rerun-fails-report output.
	const input = `github.com/crb2nu/loom/internal/spawn.TestStopSpawnLateStartCleanupFailureRetainsRetryablePod: 2 runs, 1 failures
github.com/crb2nu/loom/pkg/mills/pipeline.TestAlwaysBroken: 3 runs, 3 failures

not a rerun line at all
github.com/crb2nu/loom/pkg/x.TestThrice: 3 runs, 2 failures
`
	got, err := ParseRerunReport(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseRerunReport: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d occurrences, want 3: %+v", len(got), got)
	}

	first := got[0]
	if first.Package != "github.com/crb2nu/loom/internal/spawn" {
		t.Errorf("package = %q", first.Package)
	}
	if first.Test != "TestStopSpawnLateStartCleanupFailureRetainsRetryablePod" {
		t.Errorf("test = %q", first.Test)
	}
	if first.Runs != 2 || first.Failures != 1 {
		t.Errorf("runs/failures = %d/%d, want 2/1", first.Runs, first.Failures)
	}
	if !first.Flaked() || first.HardFailed() {
		t.Errorf("1-of-2 failures must classify as flake, not hard failure")
	}

	// The whole point of the honest-rerun contract: failures == runs is a
	// real break that reruns must not launder into a flake issue.
	if got[1].Flaked() {
		t.Errorf("3 runs / 3 failures must NOT be a flake")
	}
	if !got[1].HardFailed() {
		t.Errorf("3 runs / 3 failures must be a hard failure")
	}

	// Failed twice, passed on the third attempt: still a flake.
	if !got[2].Flaked() || got[2].HardFailed() {
		t.Errorf("3 runs / 2 failures must classify as flake")
	}
}

func TestParseRerunReportEmpty(t *testing.T) {
	got, err := ParseRerunReport(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseRerunReport: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d occurrences from empty input", len(got))
	}
}

// A package path full of dots must not be mistaken for the test-name
// boundary — the split is on the LAST dot.
func TestParseRerunReportDottedImportPath(t *testing.T) {
	got, err := ParseRerunReport(strings.NewReader(
		"gitlab.flexinfer.ai/libs/fi-accel/go/fiaccel.TestVector: 2 runs, 1 failures\n"))
	if err != nil {
		t.Fatalf("ParseRerunReport: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d occurrences, want 1", len(got))
	}
	if got[0].Package != "gitlab.flexinfer.ai/libs/fi-accel/go/fiaccel" {
		t.Errorf("package = %q", got[0].Package)
	}
	if got[0].Test != "TestVector" {
		t.Errorf("test = %q", got[0].Test)
	}
}

func TestIssueTitleKeyedOnTestName(t *testing.T) {
	o := Occurrence{Package: "github.com/crb2nu/loom/internal/spawn", Test: "TestFoo"}
	if got := IssueTitle(o); got != "flake: TestFoo" {
		t.Errorf("IssueTitle = %q, want %q", got, "flake: TestFoo")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	marker := Marker("pkg/x.TestFoo", 7, now)

	state, ok := ParseMarker("some body\n\n" + marker + "\n")
	if !ok {
		t.Fatalf("ParseMarker did not find %q", marker)
	}
	if state.ID != "pkg/x.TestFoo" {
		t.Errorf("ID = %q", state.ID)
	}
	if state.Hits != 7 {
		t.Errorf("Hits = %d, want 7", state.Hits)
	}
	if state.Last != "2026-08-03T12:00:00Z" {
		t.Errorf("Last = %q", state.Last)
	}
}

func TestParseMarkerAbsent(t *testing.T) {
	if _, ok := ParseMarker("a hand-written issue with no marker"); ok {
		t.Fatal("ParseMarker reported a marker in a body that has none")
	}
}

// A recurrence bumps the counter without disturbing anything a human wrote in
// the issue body.
func TestReplaceMarkerPreservesHumanEdits(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	body := "Investigation notes: suspect the reconciler tick.\n\n" +
		Marker("pkg/x.TestFoo", 1, now) + "\n\nMore notes below.\n"

	updated := ReplaceMarker(body, Marker("pkg/x.TestFoo", 2, now))

	if !strings.Contains(updated, "Investigation notes: suspect the reconciler tick.") {
		t.Error("human notes above the marker were lost")
	}
	if !strings.Contains(updated, "More notes below.") {
		t.Error("human notes below the marker were lost")
	}
	state, ok := ParseMarker(updated)
	if !ok || state.Hits != 2 {
		t.Errorf("hits not bumped: %+v ok=%v", state, ok)
	}
	if strings.Contains(updated, "hits=1") {
		t.Error("stale marker left behind")
	}
}

func TestReplaceMarkerAppendsWhenMissing(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	updated := ReplaceMarker("hand-filed issue", Marker("pkg/x.TestFoo", 1, now))
	state, ok := ParseMarker(updated)
	if !ok {
		t.Fatal("marker not appended to a marker-less body")
	}
	if state.Hits != 1 {
		t.Errorf("Hits = %d, want 1", state.Hits)
	}
	if !strings.HasPrefix(updated, "hand-filed issue") {
		t.Error("original body not preserved")
	}
}

func TestNewIssueBodyCarriesEvidenceAndMarker(t *testing.T) {
	o := Occurrence{Package: "pkg/spawn", Test: "TestRace", Runs: 2, Failures: 1}
	ctx := Context{
		PipelineURL: "https://gl/pipelines/1",
		JobURL:      "https://gl/jobs/2",
		Ref:         "main",
		CommitSHA:   "abc1234",
		Now:         time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	body := NewIssueBody(o, ctx)

	for _, want := range []string{
		"pkg/spawn",
		"TestRace",
		"pkg/spawn.TestRace: 2 runs, 1 failures",
		"https://gl/pipelines/1",
		"https://gl/jobs/2",
		"abc1234",
		"go test -race -count=20 -run '^TestRace$' pkg/spawn",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q", want)
		}
	}
	state, ok := ParseMarker(body)
	if !ok {
		t.Fatal("new issue body has no dedup marker")
	}
	if state.Hits != 1 {
		t.Errorf("new issue Hits = %d, want 1", state.Hits)
	}
}

func TestRecurrenceCommentNamesOccurrenceNumber(t *testing.T) {
	o := Occurrence{Package: "pkg/spawn", Test: "TestRace", Runs: 2, Failures: 1}
	ctx := Context{PipelineURL: "https://gl/pipelines/9", Now: time.Now().UTC()}
	note := RecurrenceComment(o, ctx, 4)
	if !strings.Contains(note, "occurrence #4") {
		t.Errorf("comment missing occurrence number: %q", note)
	}
	if !strings.Contains(note, "https://gl/pipelines/9") {
		t.Errorf("comment missing pipeline link: %q", note)
	}
}
