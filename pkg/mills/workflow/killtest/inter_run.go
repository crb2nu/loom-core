package killtest

import "fmt"

// ValidateInterRunPodContinuity proves that no unplanned singleton restart
// occurred between consecutive full-gate artifacts. Each run already proves
// its planned replacements through its final preflight; the next initial
// preflight must therefore observe those exact Kubernetes pod identities.
func ValidateInterRunPodContinuity(previousFinal, nextInitial PreflightReport) error {
	if !sameControllerPodIncarnation(previousFinal.Operator, nextInitial.Operator) {
		return fmt.Errorf("operator changed between runs: previous_final=%+v next_initial=%+v",
			previousFinal.Operator, nextInitial.Operator)
	}
	if !sameControllerPodIncarnation(previousFinal.Hud, nextInitial.Hud) {
		return fmt.Errorf("mobile-hud changed between runs: previous_final=%+v next_initial=%+v",
			previousFinal.Hud, nextInitial.Hud)
	}
	return nil
}
