package worker_test

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/worker"
)

// TestStageAgentValuesMatchWorkerVocabulary pins the lockstep contract the
// policy layer documents on StageAgentValuesValid: the set is duplicated in
// package mills (importing worker there would close the
// mills→worker→pipeline→mills cycle), so this external test — which may import
// both — is the guard that keeps the copies from drifting. A new AgentType in
// worker must be added to (or deliberately excluded from) the policy set.
func TestStageAgentValuesMatchWorkerVocabulary(t *testing.T) {
	for v := range mills.StageAgentValuesValid {
		if canon, err := worker.ValidateAgentType(v); err != nil {
			t.Errorf("policy agent value %q rejected by worker.ValidateAgentType: %v", v, err)
		} else if canon != v {
			t.Errorf("policy agent value %q is not canonical (worker canonicalizes to %q)", v, canon)
		}
	}
	for _, v := range []string{worker.AgentTypeClaudeCode, worker.AgentTypeCodex, worker.AgentTypeGemini} {
		if _, ok := mills.StageAgentValuesValid[v]; !ok {
			t.Errorf("worker agent type %q missing from mills.StageAgentValuesValid", v)
		}
	}
}
