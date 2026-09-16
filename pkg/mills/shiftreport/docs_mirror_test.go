package shiftreport

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/finishing"
)

func TestDocsMirrorNarrativeAndMarkdown(t *testing.T) {
	last := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, golden string
		drift        finishing.DocsMirrorDrift
	}{
		{"fresh", "testdata/docs_mirror_fresh.golden.md", finishing.DocsMirrorDrift{State: finishing.StateFresh, MirrorCommitAt: &last}},
		{"drifted", "testdata/docs_mirror_drifted.golden.md", finishing.DocsMirrorDrift{State: finishing.StateDrifted, Missing: 12, Stale: 19, MirrorRef: "main", MirrorCommitAt: &last}},
		{"unknown", "testdata/docs_mirror_unknown.golden.md", finishing.DocsMirrorDrift{State: finishing.StateUnknown, Reason: "git unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Compose(Input{GeneratedAt: last, WindowSeconds: 86400, DocsMirror: &tc.drift})
			assertGolden(t, tc.golden, r.Markdown)
		})
	}
}
