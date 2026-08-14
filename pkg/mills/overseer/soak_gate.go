package overseer

// Evaluate evaluates a complete telemetry snapshot without performing or
// recording promotion.
func (SoakGate) Evaluate(telemetry *SoakGateTelemetry) SoakGateVerdict {
	return EvaluateSoakGate(telemetry)
}

// EvaluateSoakGate atomically evaluates the Mill Staff S2 dry-run soak. It
// fails closed when evidence is absent, invalid, or does not meet every bar.
func EvaluateSoakGate(telemetry *SoakGateTelemetry) SoakGateVerdict {
	verdict := SoakGateVerdict{FailureReasons: []string{}}
	if telemetry == nil {
		verdict.FailureReasons = append(verdict.FailureReasons, "telemetry is missing")
		return verdict
	}

	if telemetry.Window < 0 || telemetry.Regressions < 0 || telemetry.ReviewedDecisions < 0 || telemetry.Disagreements < 0 {
		verdict.FailureReasons = append(verdict.FailureReasons, "telemetry contains negative values")
	}
	if telemetry.Disagreements > telemetry.ReviewedDecisions {
		verdict.FailureReasons = append(verdict.FailureReasons, "disagreements exceed reviewed decisions")
	}
	if telemetry.Window < SoakGateMinimumDuration {
		verdict.FailureReasons = append(verdict.FailureReasons, "soak window is shorter than 168 hours")
	}
	if telemetry.Regressions > 0 {
		verdict.FailureReasons = append(verdict.FailureReasons, "regressions must equal zero")
	}
	if telemetry.ReviewedDecisions == 0 {
		verdict.FailureReasons = append(verdict.FailureReasons, "reviewed decisions must be greater than zero")
	} else if telemetry.Disagreements >= 0 {
		verdict.DecisionDisagreementRate = float64(telemetry.Disagreements) / float64(telemetry.ReviewedDecisions)
		// d/reviewed < 1/20 without multiplication overflow. Exactly 5% fails.
		if telemetry.Disagreements > (telemetry.ReviewedDecisions-1)/20 {
			verdict.FailureReasons = append(verdict.FailureReasons, "decision disagreement rate must be strictly below 5 percent")
		}
	}

	verdict.Pass = len(verdict.FailureReasons) == 0
	if verdict.Pass {
		verdict.MetricPass = 1
	}
	return verdict
}
