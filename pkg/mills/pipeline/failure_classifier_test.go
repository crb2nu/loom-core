package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
)

func TestExternalIncidentPatternIDsLinkedToOperatorRunbook(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		patternID string
	}{
		{
			name:      "blob storage manifest write",
			message:   "registry cache export failed: error writing manifest blob: blob upload unknown",
			patternID: "external_dependency.blob_storage.manifest_write",
		},
		{
			name:      "gitlab auth failure",
			message:   "gitlab: status 401: unauthorized: authentication failed",
			patternID: "external_dependency.gitlab.auth_failure",
		},
		{
			name:      "gitlab rate limit",
			message:   "gitlab ci_watch: status 429: too many requests",
			patternID: "external_dependency.gitlab.rate_limit",
		},
		{
			name:      "gitlab service unavailable",
			message:   "ci_watch: gitlab: GET /projects/47/pipelines: status 503: Service Unavailable",
			patternID: "external_dependency.gitlab.service_unavailable",
		},
		{
			name:      "judge ungradeable envelope",
			message:   `rubric judge: no parseable score envelope in response; raw=""`,
			patternID: "external_dependency.model_provider.judge_ungradeable_envelope",
		},
		{
			name:      "clickhouse merge task",
			message:   "ClickHouse background worker: merge task failed: cannot select parts",
			patternID: "external_dependency.clickhouse.merge_task",
		},
		{
			name:      "longhorn no available disk",
			message:   "Longhorn volume scheduling failed: no available disk",
			patternID: "external_dependency.longhorn.no_available_disk",
		},
		{
			name:      "litellm missing api key",
			message:   "LiteLLM authentication error: missing API key for provider",
			patternID: "external_dependency.litellm.missing_api_key",
		},
		{
			// The live 2026-08-06 shape: OpenRouter's 402 copy surfacing
			// through the litellm chain at the research stage (#471/#476/#477).
			name:      "openrouter credits exhausted",
			message:   `flexinfer chat: status 402: litellm.APIError: OpenrouterException - {"error":{"message":"This request requires more credits, or fewer max_tokens. You requested up to 4096 tokens"}}`,
			patternID: "external_dependency.openrouter.credits_exhausted",
		},
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate failure_classifier_test.go")
	}
	runbookPath := filepath.Join(filepath.Dir(filename), "..", "..", "..", "docs", "operator-runbook-external-dependency-incidents.md")
	runbook, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read operator runbook: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailureRecord(errors.New(tc.message))
			if got.ExternalDependencyID != tc.patternID {
				t.Fatalf("ExternalDependencyID = %q, want %q", got.ExternalDependencyID, tc.patternID)
			}
			heading := "### `" + tc.patternID + "`"
			if count := strings.Count(string(runbook), heading); count != 1 {
				t.Fatalf("runbook heading %q occurs %d times, want exactly once", heading, count)
			}
		})
	}
}

// Prose that merely mentions credits — without OpenRouter's own 402 wording —
// must stay unclassified: the signature is deliberately narrow to the
// provider's error copy so budget discussions in agent output can never read
// as an outage.
func TestObservedIncident_CreditsProseDoesNotMatch(t *testing.T) {
	for _, msg := range []string{
		"openrouter pricing: the run consumed more credits than expected",
		"litellm: consider adding credits to the account for larger windows",
		"status 402: payment required",
	} {
		if got := ClassifyFailureRecord(errors.New(msg)); got.ExternalDependencyID != "" {
			t.Fatalf("%q classified as %q, want unclassified", msg, got.ExternalDependencyID)
		}
	}
}

func TestFailureClassClosedSet(t *testing.T) {
	got := AllFailureClasses()
	want := []FailureClass{
		FailureTransient,
		FailureTransientQuota,
		FailureInfrastructure,
		FailureCode,
		FailureConfiguration,
	}
	if len(got) != len(want) {
		t.Fatalf("AllFailureClasses len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllFailureClasses[%d] = %q, want %q", i, got[i], want[i])
		}
		if !got[i].Valid() {
			t.Fatalf("%q should be valid", got[i])
		}
	}
	if FailureClass("made_up").Valid() {
		t.Fatal("unexpected class should be invalid")
	}
	got[0] = FailureCode
	if AllFailureClasses()[0] != FailureTransient {
		t.Fatal("AllFailureClasses returned mutable backing slice")
	}
}

func TestFailureClassRetrySemantics(t *testing.T) {
	cases := []struct {
		class     FailureClass
		retryable bool
		freeRetry bool
		terminal  bool
	}{
		{FailureTransient, true, true, false},
		{FailureTransientQuota, true, true, false},
		{FailureInfrastructure, true, false, false},
		{FailureCode, true, false, false},
		{FailureConfiguration, false, false, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.class), func(t *testing.T) {
			if got := tc.class.Retryable(); got != tc.retryable {
				t.Errorf("Retryable = %v, want %v", got, tc.retryable)
			}
			if got := tc.class.FreeRetry(); got != tc.freeRetry {
				t.Errorf("FreeRetry = %v, want %v", got, tc.freeRetry)
			}
			if got := tc.class.Terminal(); got != tc.terminal {
				t.Errorf("Terminal = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestClassifyFailureMapsExistingClassifier(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, ""},
		{"transient", errors.New("websocket: close 1006"), FailureTransient},
		{"quota", errors.New("status 429: too many requests"), FailureTransientQuota},
		{"infra", errors.New("image build failed: create buildah pod: pods x already exists"), FailureInfrastructure},
		{"config", errors.New("merge: status 405: method not allowed"), FailureConfiguration},
		{"external incident", errors.New("gitlab: status 401: unauthorized: authentication failed"), FailureConfiguration},
		{"code default", errors.New("go test FAIL: TestFoo"), FailureCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(tc.err); got != tc.want {
				t.Fatalf("ClassifyFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailureClassFromStringFailsClosed(t *testing.T) {
	if got := FailureClassFromString("transient"); got != FailureTransient {
		t.Fatalf("FailureClassFromString(transient) = %q", got)
	}
	if got := FailureClassFromString("bogus"); got != FailureCode {
		t.Fatalf("FailureClassFromString(bogus) = %q, want code", got)
	}
	if got := FailureClassFromString(""); got != FailureCode {
		t.Fatalf("FailureClassFromString(empty) = %q, want code", got)
	}
}

func TestClassifyFailureRecord(t *testing.T) {
	got := ClassifyFailureRecord(errors.New("pod not found during reconciliation"))
	if got.Classifier != FailureClassifierName {
		t.Fatalf("classifier = %q, want %q", got.Classifier, FailureClassifierName)
	}
	if got.Class != FailureTransient || !got.Retryable || !got.FreeRetry || got.Terminal {
		t.Fatalf("transient record wrong: %+v", got)
	}

	got = ClassifyFailureRecord(errors.New("status 405: method not allowed"))
	if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
		t.Fatalf("configuration record wrong: %+v", got)
	}
}

func TestClassifyFailureRecordExternalIncident(t *testing.T) {
	cases := []struct {
		name               string
		err                error
		externalDependency string
		externalID         string
	}{
		{
			name:               "gitlab auth",
			err:                errors.New("gitlab: status 401: unauthorized: authentication failed"),
			externalDependency: "gitlab",
			externalID:         "external_dependency.gitlab.auth_failure",
		},
		{
			name:               "blob storage",
			err:                errors.New("registry cache export failed: error writing manifest blob: blob upload unknown"),
			externalDependency: "container_registry_blob_storage",
			externalID:         "external_dependency.blob_storage.manifest_write",
		},
		{
			// The EXACT persistent-empty judge failure from GitLab issue #348:
			// the post_review_gate judge returned an empty score envelope
			// (raw="") that survived recovery. It must classify as a terminal
			// external model-provider dependency, NOT as a retryable code defect,
			// so the pipeline stops burning code-class retries on finished work.
			name:               "judge ungradeable envelope (issue #348)",
			err:                errors.New(`judge response could not be parsed into a score envelope: rubric judge: parse: no parseable score envelope in response: rubric judge: unparseable response; raw=""`),
			externalDependency: "llm_judge_provider",
			externalID:         "external_dependency.model_provider.judge_ungradeable_envelope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailureRecord(tc.err)
			if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
				t.Fatalf("classification = %+v, want terminal configuration", got)
			}
			if got.ExternalDependency != tc.externalDependency || got.ExternalDependencyID != tc.externalID {
				t.Fatalf("external metadata = id %q dependency %q, want id %q dependency %q",
					got.ExternalDependencyID, got.ExternalDependency, tc.externalID, tc.externalDependency)
			}
		})
	}
}

func TestClassifyFailureRecordObservedExternalIncidentSignatures(t *testing.T) {
	cases := []struct {
		name       string
		message    string
		dependency string
		externalID string
	}{
		{
			name:       "clickhouse merge task",
			message:    "ClickHouse background worker: merge task failed: DB::Exception: cannot select parts",
			dependency: "clickhouse",
			externalID: "external_dependency.clickhouse.merge_task",
		},
		{
			name:       "longhorn no available disk",
			message:    `Longhorn volume pvc-123 scheduling failed: no available disk on node worker-2`,
			dependency: "longhorn",
			externalID: "external_dependency.longhorn.no_available_disk",
		},
		{
			name:       "litellm missing api key",
			message:    "LiteLLM authentication error: missing API key for provider openrouter",
			dependency: "litellm",
			externalID: "external_dependency.litellm.missing_api_key",
		},
		{
			name:       "gitlab agent unauthenticated",
			message:    "starting GitLab agent\nGitLab agent is unauthenticated; run glab auth login",
			dependency: "gitlab",
			externalID: "external_dependency.gitlab.auth_failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailureRecord(errors.New(tc.message))
			if got.Class != FailureConfiguration || got.Retryable || got.FreeRetry || !got.Terminal {
				t.Fatalf("classification = %+v, want terminal non-free-retry configuration", got)
			}
			if got.ExternalDependency != tc.dependency || got.ExternalDependencyID != tc.externalID {
				t.Fatalf("external metadata = id %q dependency %q, want id %q dependency %q",
					got.ExternalDependencyID, got.ExternalDependency, tc.externalID, tc.dependency)
			}
		})
	}
}

func TestClassifyFailureRecordObservedExternalIncidentNearMisses(t *testing.T) {
	for _, message := range []string{
		"merge task failed while compacting the local cache",
		"ClickHouse merge task completed successfully",
		"no available disk on node worker-2",
		"Longhorn volume is healthy and has an available disk",
		"missing API key for provider openrouter",
		"LiteLLM request failed with status 500",
		"agent is unauthenticated",
		"GitLab agent connected successfully",
	} {
		t.Run(message, func(t *testing.T) {
			got := ClassifyFailureRecord(errors.New(message))
			if got.ExternalDependencyID != "" || got.ExternalDependency != "" {
				t.Fatalf("near miss classified as external incident: %+v", got)
			}
		})
	}
}

func TestFailureClassificationJSONTags(t *testing.T) {
	raw, err := json.Marshal(ClassifyFailureRecord(errors.New("status 429: too many requests")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"classifier":"mills-failure-classifier","class":"transient_quota","retryable":true,"free_retry":true,"terminal":false}`
	if string(raw) != want {
		t.Fatalf("classification JSON = %s, want %s", raw, want)
	}
}

func TestFailureClassificationJSONTagsExternalIncident(t *testing.T) {
	raw, err := json.Marshal(ClassifyFailureRecord(errors.New("gitlab: status 401: unauthorized: authentication failed")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"classifier":"mills-failure-classifier","class":"configuration","retryable":false,"free_retry":false,"terminal":true,"external_dependency_id":"external_dependency.gitlab.auth_failure","external_dependency":"gitlab"}`
	if string(raw) != want {
		t.Fatalf("classification JSON = %s, want %s", raw, want)
	}
}

func TestFailureClassificationFeedsCouncilEscalationPolicy(t *testing.T) {
	class := ClassifyFailure(errors.New("status 429: too many requests"))
	got := council.DecideEscalation(council.EscalationPolicy{
		MaxAttempts:            3,
		TransientRetryCap:      2,
		EscalationAutoRetryCap: 1,
	}, council.EscalationContext{
		FailureClass:       council.EscalationFailureClass(class),
		Attempts:           5,
		PriorAutoRetryRuns: 0,
	})
	if got.Action != council.EscalationActionAutoRetryRun {
		t.Fatalf("Action = %q, want auto_retry_run: %+v", got.Action, got)
	}
	if got.Class != council.EscalationFailureTransientQuota {
		t.Fatalf("Class = %q, want transient_quota", got.Class)
	}
}
