// Package overseer contains the Mills supervisory agents — the loops that
// operate the mill itself rather than the work flowing through it: the
// backlog groomer (dedup / staleness / zombie hygiene), the deployment-health
// sentinel (dependency probes + admission suppression), and the mill foreman
// (KPI anomaly rules). The guarded-auto-act harness + audit recorder the
// agents ride live in pkg/mills/guard (extracted when the council mutator
// became the second consumer); the aliases below keep this package's
// established API surface stable for the operator wiring and tests.
package overseer

import "github.com/crb2nu/loom/pkg/mills/guard"

type (
	// Agent is one supervisory loop body (see guard.Agent).
	Agent = guard.Agent
	// TickResult summarises one agent tick (see guard.TickResult).
	TickResult = guard.TickResult
	// AgentStatus is the harness-owned status snapshot (see guard.AgentStatus).
	AgentStatus = guard.AgentStatus
	// Harness runs one Agent on a policy-driven interval (see guard.Harness).
	Harness = guard.Harness
	// ActionRecorder writes the guarded audit trail (see guard.ActionRecorder).
	ActionRecorder = guard.ActionRecorder
)

// boolGauge maps a lease-held bool onto the 0/1 suppression gauge (used by
// the sentinel and foreman suppression call sites).
func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
