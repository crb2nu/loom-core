package gates

// knownGateNames is the closed vocabulary accepted by pipeline
// configuration validation. Keep it aligned with the deterministic gates
// registered by Default; LLM gates are wired separately at runtime.
var knownGateNames = map[string]struct{}{
	"branch_pushed":    {},
	"commit_format":    {},
	"diff_size":        {},
	"docs_guardrail":   {},
	"fabricated_slice": {},
	"nonempty_diff":    {},
	"path_policy":      {},
	"pr_self_review":   {},
	"scope":            {},
	"secret_scan":      {},
	"spec_conformance": {},
}

// IsKnownGate reports whether a configured gate name belongs to the supported
// pipeline vocabulary.
func IsKnownGate(name string) bool {
	_, ok := knownGateNames[name]
	return ok
}
