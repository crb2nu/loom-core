package gates

import (
	"context"
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
