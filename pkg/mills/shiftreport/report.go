package shiftreport

import (
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/overseer"
)

// RenderS2Soak renders the overseer dry-run evidence used for the S2
// promotion decision. now is supplied by the caller to keep output stable.
func RenderS2Soak(soak overseer.SoakMetrics, now time.Time) string {
	start := soak.SoakStart()
	days := 0
	if !start.IsZero() && now.After(start) {
		days = int(now.UTC().Sub(start) / (24 * time.Hour))
	}

	rate := "n/a"
	if agreementRate, ok := soak.AgreementRate(); ok {
		rate = fmt.Sprintf("%.1f%%", agreementRate*100)
	}

	startText := "n/a"
	if !start.IsZero() {
		startText = start.Format(time.RFC3339)
	}

	var b strings.Builder
	fmt.Fprintln(&b, "## S2 overseer dry-run soak")
	fmt.Fprintf(&b, "| start (UTC) | decisions | agree rate | days soaked |\n")
	fmt.Fprintf(&b, "|---|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| %s | %d | %s | %d |\n", startText, soak.DecisionCount(), rate, days)
	return b.String()
}
