package shiftreport

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/overseer"
)

// The shift report is itself a valid soak-complete artifact: depositing it
// verbatim must yield the promotion mode its own soak_complete projection
// implies, because both sides evaluate overseer.SoakProgress.CompleteAt.
func TestShiftReportIsSoakCompleteArtifact(t *testing.T) {
	generatedAt := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	started := generatedAt.Add(-overseer.S2SoakMinimumDuration)
	tests := []struct {
		name     string
		progress *overseer.SoakProgress
		want     overseer.PromotionMode
	}{
		{name: "no soak telemetry", want: overseer.PromotionModeDryRun},
		{name: "short soak", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: overseer.S2SoakMinimumSeconds - 1}, want: overseer.PromotionModeDryRun},
		{name: "diverged soak", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: overseer.S2SoakMinimumSeconds, Divergences: 1}, want: overseer.PromotionModeDryRun},
		{name: "complete soak", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: overseer.S2SoakMinimumSeconds}, want: overseer.PromotionModeActive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := Compose(Input{GeneratedAt: generatedAt, SoakProgress: tc.progress})
			b, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "shift-report.json")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			got := overseer.PromotionGate{ArtifactPath: path, Logger: slog.New(slog.DiscardHandler)}.Decide()
			if got.Mode != tc.want {
				t.Fatalf("mode = %q (%s), want %q", got.Mode, got.Reason, tc.want)
			}
			if (got.Mode == overseer.PromotionModeActive) != report.SoakComplete {
				t.Fatalf("mode = %q disagrees with the report's soak_complete = %v", got.Mode, report.SoakComplete)
			}
		})
	}
}
