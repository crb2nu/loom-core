package shiftreport

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/overseer"
)

func TestRenderS2Soak(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		soak overseer.SoakMetrics
		want string
	}{
		{
			name: "populated soak renders UTC rate and floored days",
			soak: overseer.SoakMetrics{
				StartedAt:       time.Date(2026, 9, 2, 20, 0, 0, 0, time.FixedZone("offset", 2*60*60)),
				DryRunDecisions: 40,
				Divergences:     1,
			},
			want: "## S2 overseer dry-run soak\n" +
				"| start (UTC) | decisions | agree rate | days soaked |\n" +
				"|---|---:|---:|---:|\n" +
				"| 2026-09-02T18:00:00Z | 40 | 97.5% | 7 |\n",
		},
		{
			name: "zero decisions has no agreement rate",
			soak: overseer.SoakMetrics{StartedAt: now.Add(-12 * time.Hour)},
			want: "## S2 overseer dry-run soak\n" +
				"| start (UTC) | decisions | agree rate | days soaked |\n" +
				"|---|---:|---:|---:|\n" +
				"| 2026-09-09T06:00:00Z | 0 | n/a | 0 |\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderS2Soak(tc.soak, now)
			if got != tc.want {
				t.Fatalf("RenderS2Soak() =\n%q\nwant\n%q", got, tc.want)
			}
			if again := RenderS2Soak(tc.soak, now); again != got {
				t.Fatalf("render is not deterministic:\nfirst %q\nagain %q", got, again)
			}
		})
	}
}
