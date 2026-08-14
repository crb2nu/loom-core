package gates

import (
	"context"
	"strings"
	"testing"
)

func TestNonEmptyDiff_Evaluate(t *testing.T) {
	cases := []struct {
		name     string
		in       StageInput
		wantPass bool
	}{
		{
			name:     "no files and empty diff fails",
			in:       StageInput{},
			wantPass: false,
		},
		{
			name:     "no files and nil diff fails",
			in:       StageInput{FilesChanged: nil, DiffPatch: nil},
			wantPass: false,
		},
		{
			name:     "files changed passes",
			in:       StageInput{FilesChanged: []string{"foo.go"}},
			wantPass: true,
		},
		{
			name:     "diff only passes",
			in:       StageInput{DiffPatch: []byte("diff --git a/x b/x\n+y\n")},
			wantPass: true,
		},
		{
			name: "both populated passes",
			in: StageInput{
				FilesChanged: []string{"foo.go"},
				DiffPatch:    []byte("diff --git a/foo.go b/foo.go\n+x\n"),
			},
			wantPass: true,
		},
	}

	g := &NonEmptyDiff{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.Evaluate(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if out.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (reasons=%v)", out.Pass, tc.wantPass, out.Reasons)
			}
			if !tc.wantPass && len(out.Reasons) == 0 {
				t.Error("a failing gate must populate Reasons for the audit row")
			}
			if out.JudgedBy != "go" {
				t.Errorf("JudgedBy = %q, want \"go\"", out.JudgedBy)
			}
		})
	}
}

func TestNonEmptyDiff_Name(t *testing.T) {
	if got := (&NonEmptyDiff{}).Name(); got != "nonempty_diff" {
		t.Errorf("Name() = %q, want \"nonempty_diff\"", got)
	}
}

// TestNonEmptyDiff_NamesTheCaptureWhenItDidNotRun pins the issue-#224
// triage contract: an empty gate input caused by a dead cumulative git
// capture must not be reported as "the agent did no work". The verdict
// stays fail (an unobservable branch cannot become an MR) but the reason
// carries the capture status and its cause, so the escalation issue says
// why the retry budget burned.
func TestNonEmptyDiff_NamesTheCaptureWhenItDidNotRun(t *testing.T) {
	out, err := (&NonEmptyDiff{}).Evaluate(context.Background(), StageInput{
		GitCaptureStatus: "fetch_failed",
		GitCaptureReason: `branch ref "feat/x" fetch failed (exit 128): fatal: couldn't find remote ref`,
	})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if out.Pass {
		t.Fatal("an unobservable branch must still fail the gate")
	}
	reason := strings.Join(out.Reasons, " ")
	for _, want := range []string{"fetch_failed", "exit 128", "invisible"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q should contain %q", reason, want)
		}
	}
	if strings.Contains(reason, "agent did no work") {
		t.Errorf("capture failure must not be diagnosed as a true no-op: %q", reason)
	}
}

// TestNonEmptyDiff_StaysQuietWhenCaptureRan: a capture that ran and found
// nothing means the base message is already accurate, so no suffix — and
// a legacy stage row that records no capture status must read exactly as
// it did before.
func TestNonEmptyDiff_StaysQuietWhenCaptureRan(t *testing.T) {
	for _, status := range []string{"", "captured", "captured_empty"} {
		out, err := (&NonEmptyDiff{}).Evaluate(context.Background(), StageInput{GitCaptureStatus: status})
		if err != nil {
			t.Fatalf("Evaluate(%q) returned error: %v", status, err)
		}
		if got := strings.Join(out.Reasons, " "); strings.Contains(got, "cumulative git capture did not run") {
			t.Errorf("status %q should not add a capture suffix; got %q", status, got)
		} else if !strings.Contains(got, "agent did no work") {
			t.Errorf("status %q should identify a true no-op; got %q", status, got)
		}
	}
}
