package mcperror

import "testing"

func TestClassifyExternalIncident_BlobStorage(t *testing.T) {
	t.Parallel()

	cases := []string{
		`failed to solve: error writing manifest blob: failed commit on ref "manifest-sha256:abc"`,
		`unexpected status from PUT https://registry.example/v2/services/loom-core/manifests/buildcache: 500 Internal Server Error`,
		`Registry cache export failed; retrying without cache flags.`,
		`failed commit on ref: blob upload unknown to registry`,
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, ok := ClassifyExternalIncident(input)
			if !ok {
				t.Fatal("ClassifyExternalIncident returned ok=false")
			}
			if got.ID != ExternalIncidentIDBlobStorageManifestWrite {
				t.Fatalf("ID = %q, want %q", got.ID, ExternalIncidentIDBlobStorageManifestWrite)
			}
			if got.Kind != ExternalIncidentKindBlobStorage {
				t.Fatalf("Kind = %q, want %q", got.Kind, ExternalIncidentKindBlobStorage)
			}
			if got.Dependency != "container_registry_blob_storage" {
				t.Fatalf("Dependency = %q, want container_registry_blob_storage", got.Dependency)
			}
			if got.Evidence == "" {
				t.Fatal("Evidence is empty")
			}
		})
	}
}

func TestClassifyExternalIncident_GitLabAuth(t *testing.T) {
	t.Parallel()

	cases := []string{
		`GitLab: authentication failed - check your API token`,
		`gitlab: GET /projects/47/pipelines: status 401: {"message":"401 Unauthorized"}`,
		`MR stage failed with GitLab auth errors: unexpected status 401 Unauthorized: Incorrect API key`,
		`gitlab verify_token failed: invalid token`,
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, ok := ClassifyExternalIncident(input)
			if !ok {
				t.Fatal("ClassifyExternalIncident returned ok=false")
			}
			if got.ID != ExternalIncidentIDGitLabAuthFailure {
				t.Fatalf("ID = %q, want %q", got.ID, ExternalIncidentIDGitLabAuthFailure)
			}
			if got.Kind != ExternalIncidentKindGitLabAuth {
				t.Fatalf("Kind = %q, want %q", got.Kind, ExternalIncidentKindGitLabAuth)
			}
			if got.Dependency != "gitlab" {
				t.Fatalf("Dependency = %q, want gitlab", got.Dependency)
			}
			if got.Evidence == "" {
				t.Fatal("Evidence is empty")
			}
		})
	}
}

// issue348JudgeReason is the EXACT failing-gate reason text from GitLab issue
// #348 (services/loom-core): a post_review_gate escalation where every LLM
// judge returned an empty score envelope (raw="") on finished work. The
// classifier must recognize it as a model-provider dependency incident so the
// pipeline stops burning code-class retries on it.
const issue348JudgeReason = `judge response could not be parsed into a score envelope: rubric judge: parse: no parseable score envelope in response: rubric judge: unparseable response; raw=""`

func TestClassifyExternalIncident_JudgeUngradeableEnvelope(t *testing.T) {
	t.Parallel()

	cases := []string{
		issue348JudgeReason,
		// The gates-layer summary form (spec_conformance + pr_self_review both
		// empty), as summarizeGateFailures renders it into an escalation reason.
		`spec_conformance: judge response could not be parsed into a score envelope: rubric judge: parse: no parseable score envelope in response: rubric judge: unparseable response; raw=""; pr_self_review: judge response could not be parsed into a score envelope: rubric judge: parse: no parseable score envelope in response: rubric judge: unparseable response; raw=""`,
		// The bare clients-layer sentinel message.
		`rubric judge: unparseable response`,
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, ok := ClassifyExternalIncident(input)
			if !ok {
				t.Fatal("ClassifyExternalIncident returned ok=false")
			}
			if got.ID != ExternalIncidentIDJudgeUngradeableEnvelope {
				t.Fatalf("ID = %q, want %q", got.ID, ExternalIncidentIDJudgeUngradeableEnvelope)
			}
			if got.Kind != ExternalIncidentKindModelProvider {
				t.Fatalf("Kind = %q, want %q", got.Kind, ExternalIncidentKindModelProvider)
			}
			if got.Dependency != "llm_judge_provider" {
				t.Fatalf("Dependency = %q, want llm_judge_provider", got.Dependency)
			}
			if got.Evidence == "" {
				t.Fatal("Evidence is empty")
			}
		})
	}
}

func TestClassifyExternalIncident_UnknownAndEmpty(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"   \n\t",
		"GitLab returned status 503: service unavailable",
		"Harbor returned 401 Unauthorized while pulling a runner image",
		"go test ./...\n--- FAIL: TestWidget",
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got, ok := ClassifyExternalIncident(input); ok {
				t.Fatalf("ClassifyExternalIncident(%q) = %+v, true; want false", input, got)
			}
		})
	}
}

func TestClassifyExternalIncident_UsesFirstMatchedLineAsEvidence(t *testing.T) {
	t.Parallel()

	input := "\n\nstarting job\n  gitlab: POST /projects/47/merge_requests: status 401: unauthorized\nsecond line"
	got, ok := ClassifyExternalIncident(input)
	if !ok {
		t.Fatal("ClassifyExternalIncident returned ok=false")
	}
	if got.Evidence != "gitlab: POST /projects/47/merge_requests: status 401: unauthorized" {
		t.Fatalf("Evidence = %q, want first trimmed matching line", got.Evidence)
	}
}
