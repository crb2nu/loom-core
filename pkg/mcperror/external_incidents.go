package mcperror

import (
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	ExternalIncidentKindBlobStorage   = "blob_storage"
	ExternalIncidentKindGitLabAuth    = "gitlab_auth"
	ExternalIncidentKindModelProvider = "model_provider"

	ExternalIncidentIDBlobStorageManifestWrite = telemetry.IncidentExternalIDBlobStorageManifestWrite
	ExternalIncidentIDGitLabAuthFailure        = telemetry.IncidentExternalIDGitLabAuthFailure
	// ExternalIncidentIDJudgeUngradeableEnvelope marks the Mills LLM rubric
	// judge returning a response that could not be parsed into a score
	// envelope (empty content, raw=""). This is a model-provider dependency
	// failure — the judge model ran but emitted no gradeable verdict —
	// typically a reasoning-model budget squeeze (issue #348) that survived
	// the client's own boosted-retry recovery. It must NOT be classed as a
	// code defect: respawning the (already-successful) upstream stage cannot
	// change the model's output.
	ExternalIncidentIDJudgeUngradeableEnvelope = "external_dependency.model_provider.judge_ungradeable_envelope"
)

// ExternalIncident describes a recognized external dependency incident
// signature. Values are stable so callers can store them without parsing
// operator log text.
type ExternalIncident struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Dependency string `json:"dependency"`
	Summary    string `json:"summary"`
	Evidence   string `json:"evidence,omitempty"`
}

// ClassifyExternalIncident matches known external dependency incident
// signatures in logs or error strings. Unknown input returns false.
func ClassifyExternalIncident(text string) (ExternalIncident, bool) {
	for _, line := range strings.Split(text, "\n") {
		evidence := strings.TrimSpace(line)
		if evidence == "" {
			continue
		}
		lower := strings.ToLower(evidence)

		if isBlobStorageIncident(lower) {
			return ExternalIncident{
				ID:         ExternalIncidentIDBlobStorageManifestWrite,
				Kind:       ExternalIncidentKindBlobStorage,
				Dependency: "container_registry_blob_storage",
				Summary:    "container registry blob storage rejected manifest/cache writes",
				Evidence:   evidence,
			}, true
		}

		if isGitLabAuthIncident(lower) {
			return ExternalIncident{
				ID:         ExternalIncidentIDGitLabAuthFailure,
				Kind:       ExternalIncidentKindGitLabAuth,
				Dependency: "gitlab",
				Summary:    "GitLab API authentication failed",
				Evidence:   evidence,
			}, true
		}

		if isJudgeUngradeableEnvelopeIncident(lower) {
			return ExternalIncident{
				ID:         ExternalIncidentIDJudgeUngradeableEnvelope,
				Kind:       ExternalIncidentKindModelProvider,
				Dependency: "llm_judge_provider",
				Summary:    "LLM rubric judge returned an ungradeable score envelope (empty/unparseable response)",
				Evidence:   evidence,
			}, true
		}
	}

	return ExternalIncident{}, false
}

// isJudgeUngradeableEnvelopeIncident reports whether a log/error line is the
// Mills rubric judge failing to emit a parseable score envelope — the
// model-provider failure behind issue #348 (run self-rated 0.91, all stages
// green, then post_review_gate escalated on empty judge responses, raw="").
// The needles are the distinctive judge parse-miss phrases produced by the
// gates layer (gates.LLMGate.Evaluate) and the clients layer
// (clients.RubricJudge / ErrRubricUnparseable), so a persistent empty envelope
// classifies as an external dependency incident instead of a code defect.
func isJudgeUngradeableEnvelopeIncident(line string) bool {
	for _, needle := range []string{
		"could not be parsed into a score envelope",
		"no parseable score envelope",
		"rubric judge: unparseable response",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func isBlobStorageIncident(line string) bool {
	for _, needle := range []string{
		"error writing manifest blob",
		"/manifests/buildcache",
		"registry cache export failed",
		"blob upload unknown",
		"blob upload invalid",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func isGitLabAuthIncident(line string) bool {
	if !strings.Contains(line, "gitlab") {
		return false
	}
	for _, needle := range []string{
		"authentication failed",
		"401 unauthorized",
		"status 401",
		"unauthorized",
		"incorrect api",
		"invalid token",
	} {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
