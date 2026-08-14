package store

// IsPipelineTerminalState reports whether state is a final pipeline outcome.
// Terminal states are one-way: a run may receive idempotent rollups for the
// same terminal state, but it must never move from one terminal outcome to
// another or back to active work.
func IsPipelineTerminalState(state PipelineState) bool {
	switch state {
	case PipelineDone, PipelineEscalated, PipelinePaused:
		return true
	default:
		return false
	}
}

// PipelineTerminalConflict reports whether a persisted run state already
// resolved to a different terminal outcome than the requested state.
func PipelineTerminalConflict(persisted, requested PipelineState) bool {
	return IsPipelineTerminalState(persisted) && persisted != requested
}
