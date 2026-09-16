package gates

import (
	"context"
	"fmt"
	"strings"
)

// FailedCheck is the portion of a devbox check needed by tests_verdict.
type FailedCheck struct {
	Name     string
	ExitCode int
	Output   string
}

// TestsVerdict is the structured result emitted by the tests stage.
type TestsVerdict struct {
	Passed       bool
	FailedChecks []FailedCheck
}

// TestsVerdictGate rewinds a genuine lint/test verdict to implementation.
type TestsVerdictGate struct{}

func (*TestsVerdictGate) Name() string { return "tests_verdict" }

func (*TestsVerdictGate) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if in.TestsVerdict == nil || in.TestsVerdict.Passed {
		return Outcome{Pass: true, JudgedBy: "go"}, nil
	}
	reasons := make([]string, 0, len(in.TestsVerdict.FailedChecks))
	for _, check := range in.TestsVerdict.FailedChecks {
		reasons = append(reasons, fmt.Sprintf("%s[exit=%d]: %s", check.Name, check.ExitCode, boundedCheckOutput(check.Output)))
	}
	return Outcome{Pass: false, Reasons: reasons, JudgedBy: "go"}, nil
}

func boundedCheckOutput(output string) string {
	lines := strings.Split(output, "\n")
	if len(lines) > 12 {
		lines = lines[:12]
	}
	out := strings.Join(lines, "\n")
	const maxBytes = 1536
	if len(out) > maxBytes {
		out = out[:maxBytes]
	}
	return out
}
