package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/workflow/killtest"
)

func TestRunScenarioDispatch(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	evidence := pipeline.KilltestEvidence{
		Scenario: pipeline.KilltestMRAwareness, CapturedAt: now.Add(-time.Second), Deadline: now.Add(time.Minute),
		MRAwareness: &pipeline.MRAwarenessEvidence{Repo: "services/loom-core", IID: 7, SourceBranch: "feat/x", Recognitions: []pipeline.MRRecognition{
			{Repo: "services/loom-core", IID: 7, SourceBranch: "feat/x", State: "ok", Recognized: true, ObservedAt: now.Add(-2 * time.Second)},
		}},
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScenario(pipeline.KilltestMRAwareness, path, time.Minute, now); err != nil {
		t.Fatalf("runScenario() error = %v", err)
	}
	for _, scenario := range []pipeline.KilltestScenario{"unknown", pipeline.KilltestQueuedProof} {
		if err := runScenario(scenario, path, time.Minute, now); err == nil {
			t.Fatalf("runScenario(%q) unexpectedly passed", scenario)
		}
	}
	if err := os.WriteFile(path, append(data, []byte(` {}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScenario(pipeline.KilltestMRAwareness, path, time.Minute, now); err == nil {
		t.Fatal("trailing evidence unexpectedly passed")
	}
}

func TestRunScenarioStructuredFailureReports(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, input, code string
		scenario          pipeline.KilltestScenario
	}{
		{"unknown scenario", `{}`, "unknown_scenario", "unknown"},
		{"malformed JSON", `{`, "malformed_evidence", pipeline.KilltestMRAwareness},
		{"trailing JSON", `{} {}`, "trailing_json", pipeline.KilltestMRAwareness},
		{"missing identity", `{"scenario":"mr-awareness","captured_at":"2026-08-05T11:59:59Z","deadline":"2026-08-05T12:01:00Z","mr_awareness":{}}`, "missing_identity", pipeline.KilltestMRAwareness},
		{"contradictory identity", `{"scenario":"mr-awareness","captured_at":"2026-08-05T11:59:59Z","deadline":"2026-08-05T12:01:00Z","mr_awareness":{"repo":"services/loom-core","iid":7,"source_branch":"feat/x","recognitions":[{"repo":"services/other","iid":7,"source_branch":"feat/x","state":"open","recognized":true,"observed_at":"2026-08-05T11:59:58Z"}]}}`, "contradictory_identity", pipeline.KilltestMRAwareness},
		{"duplicate evidence", `{"scenario":"mr-awareness","captured_at":"2026-08-05T11:59:59Z","deadline":"2026-08-05T12:01:00Z","mr_awareness":{"repo":"services/loom-core","iid":7,"source_branch":"feat/x","recognitions":[{"repo":"services/loom-core","iid":7,"source_branch":"feat/x","state":"open","recognized":true,"observed_at":"2026-08-05T11:59:58Z"},{"repo":"services/loom-core","iid":7,"source_branch":"feat/x","state":"open","recognized":true,"observed_at":"2026-08-05T11:59:58Z"}]}}`, "duplicate_or_ambiguous_evidence", pipeline.KilltestMRAwareness},
		{"stale", `{"scenario":"mr-awareness","captured_at":"2026-08-05T11:00:00Z","deadline":"2026-08-05T12:01:00Z"}`, "stale_evidence", pipeline.KilltestMRAwareness},
		{"expired", `{"scenario":"mr-awareness","captured_at":"2026-08-05T11:59:59Z","deadline":"2026-08-05T11:59:59Z"}`, "expired_deadline", pipeline.KilltestMRAwareness},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(path, []byte(tt.input), 0o600); err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			if err := runScenarioTo(&output, tt.scenario, path, time.Minute, now); err == nil {
				t.Fatal("unexpected pass")
			}
			var report pipeline.KilltestReport
			if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
				t.Fatalf("decode report: %v; output=%q", err, output.String())
			}
			if report.Verdict != "FAIL" || report.Passed || report.ReasonCode != tt.code {
				t.Fatalf("report=%+v, want FAIL reason %q", report, tt.code)
			}
		})
	}
}

func TestRunIsExternalDependencyIncidentFailsClosed(t *testing.T) {
	for name, detail := range map[string]queuedProofRunDetail{
		"class": func() queuedProofRunDetail {
			var d queuedProofRunDetail
			d.Run.EscalationClass = string(pipeline.ClassificationExternalDependencyIncident)
			return d
		}(),
		"dependency id": func() queuedProofRunDetail {
			var d queuedProofRunDetail
			d.Run.ExternalDependencyID = "external_dependency.gitlab.service_unavailable"
			return d
		}(),
		"canonical stage classification": {Stages: []struct {
			Stage     string         `json:"Stage"`
			Outcome   *string        `json:"Outcome"`
			Artifacts map[string]any `json:"Artifacts"`
			LogTail   string         `json:"LogTail"`
		}{{LogTail: "classification=external_dependency_incident"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if !runIsExternalDependencyIncident(detail) {
				t.Fatal("external dependency incident was not rejected")
			}
		})
	}
	if runIsExternalDependencyIncident(queuedProofRunDetail{}) {
		t.Fatal("empty run detail classified as external dependency incident")
	}
}

func TestRunLiveQueuedProofReportsExternalDependencyWithoutFalseFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/backlog/item-1":
			_, _ = fmt.Fprint(w, `{"ID":"item-1","State":"queued","PlanID":"plan-1","TargetProject":"services/widgets","Labels":["pattern-loom"],"Policy":{"auto_merge":true}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/pipeline/runs/item-1/start":
			_, _ = fmt.Fprint(w, `{"run_id":"run-1","backlog_id":"item-1","state":"queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/mills/pipeline/runs/run-1":
			_, _ = fmt.Fprint(w, `{"run":{"ID":"run-1","BacklogID":"item-1","State":"escalated","EscalationClass":"external_dependency_incident","ExternalDependency":"gitlab"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "proof.json")
	driver := queuedProofDriver{operatorURL: server.URL, adminToken: "admin", gitlabURL: server.URL, gitlabToken: "gitlab", gitlabProject: "services/widgets", client: server.Client(), poll: time.Millisecond}
	if err := runLiveQueuedProof(context.Background(), driver, "item-1", "plan-1", path, time.Second); err != nil {
		t.Fatalf("external dependency returned ordinary failure: %v", err)
	}
	var report queuedProofReport
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.Verdict != queuedProofVerdictExternalDependency || report.DeclaredTarget != "services/widgets" || report.RunID != "run-1" {
		t.Fatalf("report = %+v", report)
	}
}

func TestQueuedProofStartResponseDecodesOperatorContract(t *testing.T) {
	var got queuedProofStartResponse
	if err := json.Unmarshal([]byte(`{"run_id":"run-1","backlog_id":"item-1","state":"queued","decision":"started"}`), &got); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if got.RunID != "run-1" || got.BacklogID != "item-1" || got.State != "queued" || got.Decision != "started" {
		t.Fatalf("decoded start response = %+v", got)
	}
}

func TestValidateQueuedProofBacklogFailsClosed(t *testing.T) {
	valid := queuedProofBacklog{ID: "item-1", State: "queued", PlanID: "plan-1", TargetProject: "services/widgets", Labels: []string{"pattern-loom"}}
	valid.Policy.AutoMerge = true
	if err := validateQueuedProofBacklog(valid, "item-1", "plan-1", "services/widgets"); err != nil {
		t.Fatalf("valid backlog rejected: %v", err)
	}
	unsafe := valid
	unsafe.Policy.AutoMerge = false
	if err := validateQueuedProofBacklog(unsafe, "item-1", "plan-1", "services/widgets"); err == nil {
		t.Fatal("backlog without auto-merge unexpectedly accepted")
	}
	unsafe = valid
	unsafe.ID = "item-2"
	if err := validateQueuedProofBacklog(unsafe, "item-1", "plan-1", "services/widgets"); err == nil {
		t.Fatal("contradictory backlog identity unexpectedly accepted")
	}
	if err := validateQueuedProofBacklog(valid, "item-1", "plan-1", "services/other"); err == nil {
		t.Fatal("contradictory target unexpectedly accepted")
	}
}

func TestRunEvidencePath(t *testing.T) {
	tests := []struct {
		name         string
		base         string
		index, total int
		gate         bool
		want         string
	}{
		{"attach single keeps base", "evidence.json", 1, 1, false, "evidence.json"},
		{"gate single suffixes so the summary keeps base", "evidence.json", 1, 1, true, "evidence-run-01.json"},
		{"first of three", "dir/s1c.json", 1, 3, true, "dir/s1c-run-01.json"},
		{"third of three", "dir/s1c.json", 3, 3, true, "dir/s1c-run-03.json"},
		{"extensionless", "s1c", 2, 3, true, "s1c-run-02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runEvidencePath(tt.base, tt.index, tt.total, tt.gate); got != tt.want {
				t.Fatalf("runEvidencePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyAndSealGateSummaryInvalidatesCanonicalVerificationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1c-summary.json")
	summary := gateSummary{RequiredRuns: 3, CompletedRuns: 3, Overall: true}
	if err := writeSummary(path, 3, summary); err != nil {
		t.Fatal(err)
	}
	err := verifyAndSealGateSummary(path, 3, &summary, func(string) error {
		return errors.New("forged evidence digest")
	})
	if err == nil || !strings.Contains(err.Error(), "summary invalidated") || summary.Overall {
		t.Fatalf("canonical verification failure did not invalidate in-memory summary: summary=%+v err=%v", summary, err)
	}
	var persisted gateSummary
	if _, err := readStrictRegularJSONWithSHA256(path, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Overall {
		t.Fatalf("canonical verification failure left a passing summary on disk: %+v", persisted)
	}
}

func TestEvidenceFileSHA256UsesStrictSingleReadParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	if err := writeJSON(path, runOutput{}); err != nil {
		t.Fatal(err)
	}
	digest, err := evidenceFileSHA256(path)
	if err != nil || len(digest) != 64 {
		t.Fatalf("strict evidence digest failed: digest=%q err=%v", digest, err)
	}
	if err := writeJSON(path, map[string]any{"unknown": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := evidenceFileSHA256(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("evidence digest accepted bytes that do not parse as strict run output: %v", err)
	}
}

func TestValidateOptions(t *testing.T) {
	for _, tt := range []struct {
		name, phase, runID, token string
		runs                      int
		merging                   bool
		wantErr                   bool
	}{
		{"full gate", "full", "", "token", 3, false, false},
		{"single run cannot be gate", "full", "", "token", 1, false, true},
		{"merging gate is exactly one run", "full", "", "token", 1, true, false},
		{"merging gate rejects three runs", "full", "", "token", 3, true, true},
		{"attached recovery", "full", "run-1", "token", 1, false, false},
		{"preflight needs no token", "preflight", "", "", 1, false, false},
		{"multi attach", "full", "run-1", "token", 3, false, true},
		{"zero runs", "full", "", "token", 0, false, true},
		{"missing token", "full", "", "", 3, false, true},
		{"bad phase", "observe", "", "token", 3, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectedGitOps := "0123456789abcdef0123456789abcdef01234567"
			expectedLoomCore := "89abcdef0123456789abcdef0123456789abcdef"
			if tt.name == "preflight needs no token" {
				expectedGitOps = ""
				expectedLoomCore = ""
			}
			hudURL, hudToken := "https://hud.example", "hud-token"
			gitOpsRepo := "/tmp/gitops"
			loomCoreRepo := ""
			identityMode := killtest.GitOpsIdentityModeExactRevision
			if tt.phase == "full" {
				loomCoreRepo = "/tmp/loom-core"
			}
			if tt.phase == "full" && tt.runID == "" {
				identityMode = killtest.GitOpsIdentityModeProtectedScope
			}
			if tt.name == "preflight needs no token" {
				hudURL, hudToken = "", ""
				gitOpsRepo = ""
			}
			err := validateOptions(tt.phase, tt.runs, tt.merging, tt.runID, tt.token, expectedGitOps, expectedLoomCore,
				identityMode, gitOpsRepo, loomCoreRepo, hudURL, hudToken, killtest.AgentTypeClaudeCode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateOptions() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	gitOpsRevision := "0123456789abcdef0123456789abcdef01234567"
	loomCoreRevision := "89abcdef0123456789abcdef0123456789abcdef"
	if err := validateOptions("full", 3, false, "", "token", "", loomCoreRevision, killtest.GitOpsIdentityModeExactRevision, "/tmp/gitops", "", "https://hud.example", "hud-token", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("full gate accepted without an expected GitOps revision")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, "", killtest.GitOpsIdentityModeExactRevision, "/tmp/gitops", "", "https://hud.example", "hud-token", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("full gate accepted without an expected loom-core revision")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, loomCoreRevision, killtest.GitOpsIdentityModeExactRevision, "/tmp/gitops", "", "", "", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("full gate accepted without HUD cleanup credentials")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, loomCoreRevision, killtest.GitOpsIdentityModeExactRevision, "/tmp/gitops", "", "https://hud.example", "hud-token", "gemini"); err == nil {
		t.Fatal("full gate accepted unsupported --agent-type")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, loomCoreRevision, killtest.GitOpsIdentityModeProtectedScope, "", "", "https://hud.example", "hud-token", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("protected-scope gate accepted without source repositories")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, loomCoreRevision, killtest.GitOpsIdentityModeExactRevision, "", "", "https://hud.example", "hud-token", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("full gate accepted without a GitOps repository for reviewed render specs")
	}
	if err := validateOptions("full", 3, false, "", "token", gitOpsRevision, loomCoreRevision, killtest.GitOpsIdentityModeExactRevision, "/tmp/gitops", "/tmp/loom-core", "https://hud.example", "hud-token", killtest.AgentTypeClaudeCode); err == nil || !strings.Contains(err.Error(), "requires --gitops-identity-mode protected-scope") {
		t.Fatalf("full gate accepted non-protected identity mode: %v", err)
	}
	if err := validateOptions("preflight", 1, false, "", "", "", "", "semantic-ish", "", "", "", "", killtest.AgentTypeClaudeCode); err == nil {
		t.Fatal("invalid GitOps identity mode accepted")
	}
}

func TestValidateCanaryHoldRecheck(t *testing.T) {
	initial := killtest.CanaryHoldObservation{
		PodName: "spawn-abc", PID: 42, StartTimeTicks: 4200,
		DriverPID: 41, DriverStartTimeTicks: 4100, Seconds: 90, ObservedAt: time.Now().UTC(),
	}
	current := initial
	current.ObservedAt = current.ObservedAt.Add(time.Second)
	if err := validateCanaryHoldRecheck(initial, current, true); err != nil {
		t.Fatalf("same live hold rejected: %v", err)
	}
	for _, tt := range []struct {
		name   string
		ready  bool
		mutate func(*killtest.CanaryHoldObservation)
	}{
		{name: "not running", ready: false, mutate: func(*killtest.CanaryHoldObservation) {}},
		{name: "PID changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.PID++ }},
		{name: "driver PID changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.DriverPID++ }},
		{name: "hold start time changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.StartTimeTicks++ }},
		{name: "driver start time changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.DriverStartTimeTicks++ }},
		{name: "pod changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.PodName = "spawn-other" }},
		{name: "duration changed", ready: true, mutate: func(obs *killtest.CanaryHoldObservation) { obs.Seconds++ }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := current
			tt.mutate(&got)
			if err := validateCanaryHoldRecheck(initial, got, tt.ready); err == nil {
				t.Fatal("invalid recheck accepted")
			}
		})
	}
}

func TestJoinCrashLeaseErrorsPreservesOperationAndReleaseFailures(t *testing.T) {
	operationErr := errors.New("final workload check failed")
	releaseErr := errors.New("operator route unavailable")

	joined := joinCrashLeaseErrors(operationErr, releaseErr)
	if !errors.Is(joined, operationErr) || !errors.Is(joined, releaseErr) {
		t.Fatalf("joinCrashLeaseErrors() = %v, want both causes", joined)
	}
	if joined == nil || !strings.Contains(joined.Error(), "release crash lease") {
		t.Fatalf("joinCrashLeaseErrors() omitted release diagnostic: %v", joined)
	}
	if got := joinCrashLeaseErrors(operationErr, nil); !errors.Is(got, operationErr) {
		t.Fatalf("operation-only result = %v", got)
	}
	if got := joinCrashLeaseErrors(nil, releaseErr); !errors.Is(got, releaseErr) {
		t.Fatalf("release-only result = %v", got)
	}
	if got := joinCrashLeaseErrors(nil, nil); got != nil {
		t.Fatalf("nil result = %v", got)
	}
}

func TestJoinHarnessCleanupErrorsMakesCleanupFailureFatal(t *testing.T) {
	operationErr := errors.New("gate failed")
	cleanupErr := errors.New("permission denied")
	joined := joinHarnessCleanupErrors(operationErr, cleanupErr)
	if !errors.Is(joined, operationErr) || !errors.Is(joined, cleanupErr) ||
		!strings.Contains(joined.Error(), "private frozen S1c kubeconfig") {
		t.Fatalf("joined cleanup error = %v", joined)
	}
	if got := joinHarnessCleanupErrors(nil, cleanupErr); !errors.Is(got, cleanupErr) {
		t.Fatalf("cleanup-only error was lost: %v", got)
	}
	if got := joinHarnessCleanupErrors(operationErr, nil); !errors.Is(got, operationErr) {
		t.Fatalf("operation-only error was lost: %v", got)
	}
}

func TestReleaseCrashLeaseAfterInvokesCleanupAndPreservesBothFailures(t *testing.T) {
	operationErr := errors.New("final workload check failed")
	var releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releases.Add(1)
		if r.Method != http.MethodDelete || r.URL.Path != "/api/mills/safety/crash-lease/lease-token" {
			t.Fatalf("release request = %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	defer server.Close()

	h := killtest.New(killtest.Config{OperatorURL: server.URL, AdminToken: "admin"})
	got := releaseCrashLeaseAfter(h, "lease-token", operationErr)
	if releases.Load() != 1 || !errors.Is(got, operationErr) || !strings.Contains(got.Error(), "release crash lease") ||
		!strings.Contains(got.Error(), "401") {
		t.Fatalf("releaseCrashLeaseAfter() = %v releases=%d", got, releases.Load())
	}
}

func TestSameGateIdentityIncludesTagsGenerationsAndGitOps(t *testing.T) {
	base := killtest.PreflightReport{
		OperatorImage: "operator:v1", Operator: killtest.PodIdentity{ImageID: "operator@sha256:a"},
		OperatorDeployment: killtest.DeploymentIdentity{Generation: 7},
		HudImage:           "hud:v1", Hud: killtest.PodIdentity{ImageID: "hud@sha256:b"},
		HudDeployment:  killtest.DeploymentIdentity{Generation: 8},
		PolicyChecksum: "checksum", GitOpsRevision: "main@sha1:abc",
		GitOpsIdentity: killtest.GitOpsScopeIdentity{
			Mode: killtest.GitOpsIdentityModeExactRevision, Contract: "platform-gitops", ContractVersion: 1,
			BaselineRevision: "abc", ObservedRevision: "abc",
			BaselineDigest: "abc", ObservedDigest: "abc",
		},
		LoomCoreRevision: "main@sha1:def",
		LoomCoreIdentity: killtest.GitOpsScopeIdentity{
			Mode: killtest.GitOpsIdentityModeExactRevision, Contract: "loom-core-source", ContractVersion: 1,
			BaselineRevision: "def", ObservedRevision: "def", BaselineDigest: "def", ObservedDigest: "def",
		},
		PolicyConfigMapIdentity: killtest.KubernetesObjectIdentity{
			Name: "loom-mills-policy", Namespace: "loom-mills", UID: "policy-cm-uid", ResourceVersion: "40",
		},
		SpawnConfigMapUID: "spawn-cm-uid",
		SpawnConfigMapIdentity: killtest.KubernetesObjectIdentity{
			Name: "loom-spawn-state", Namespace: "devbox", UID: "spawn-cm-uid", ResourceVersion: "50",
		},
		SpawnConfigMapUpdateAllowed: true,
		FlagEnabled:                 true, SubstrateK8sOnly: true, EffectivePolicyMatchesConfigMap: true,
	}
	base.GitOpsBootstrapRevision = base.GitOpsRevision
	base.GitOpsBootstrapIdentity = base.GitOpsIdentity
	base.GitOpsSystemRevision = base.GitOpsRevision
	base.GitOpsSystemIdentity = base.GitOpsIdentity
	if err := sameGateIdentity(base, base); err != nil {
		t.Fatalf("same identity rejected: %v", err)
	}
	spawnAdvanced := base
	spawnAdvanced.SpawnConfigMapIdentity.ResourceVersion = "51"
	if err := sameGateIdentity(base, spawnAdvanced); err != nil {
		t.Fatalf("spawn ConfigMap resourceVersion advancement rejected: %v", err)
	}
	protectedBase := base
	protectedBase.GitOpsIdentity = killtest.GitOpsScopeIdentity{
		Mode: killtest.GitOpsIdentityModeProtectedScope, Contract: "platform-gitops", ContractVersion: 1,
		BaselineRevision: "baseline",
		ObservedRevision: "first", BaselineDigest: "scope", ObservedDigest: "scope",
	}
	protectedDescendant := protectedBase
	protectedDescendant.GitOpsRevision = "main@sha1:descendant"
	protectedDescendant.GitOpsIdentity.ObservedRevision = "descendant"
	if err := sameGateIdentity(protectedBase, protectedDescendant); err != nil {
		t.Fatalf("protected-scope-equivalent descendant revision rejected: %v", err)
	}
	for name, mutate := range map[string]func(*killtest.PreflightReport){
		"operator tag":        func(r *killtest.PreflightReport) { r.OperatorImage = "operator:v2" },
		"operator generation": func(r *killtest.PreflightReport) { r.OperatorDeployment.Generation++ },
		"hud generation":      func(r *killtest.PreflightReport) { r.HudDeployment.Generation++ },
		"GitOps digest":       func(r *killtest.PreflightReport) { r.GitOpsIdentity.ObservedDigest = "def" },
		"bootstrap digest":    func(r *killtest.PreflightReport) { r.GitOpsBootstrapIdentity.ObservedDigest = "def" },
		"system digest":       func(r *killtest.PreflightReport) { r.GitOpsSystemIdentity.ObservedDigest = "def" },
		"loom-core digest":    func(r *killtest.PreflightReport) { r.LoomCoreIdentity.ObservedDigest = "changed" },
		"spawn ConfigMap UID": func(r *killtest.PreflightReport) { r.SpawnConfigMapUID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			got := base
			mutate(&got)
			if err := sameGateIdentity(base, got); err == nil {
				t.Fatal("identity drift accepted")
			}
		})
	}
}

func TestRetryableFluxConvergence(t *testing.T) {
	const revision = "main@sha1:abc"
	transientErr := errors.New(`GitOps convergence: flux apps is not converged: ready=false applied="main@sha1:abc" attempted="main@sha1:abc"`)
	base := killtest.PreflightReport{GitOpsRevision: revision, GitOpsAttempted: revision}
	if !retryableFluxConvergence(base, transientErr, revision, "") {
		t.Fatal("unchanged applied/attempted baseline revision must be retryable")
	}
	if retryableFluxConvergence(base, transientErr, "main@sha1:different", "") {
		t.Fatal("revision differing from the crash baseline must fail immediately")
	}
	protected := base
	protected.GitOpsIdentity = killtest.GitOpsScopeIdentity{
		Mode: killtest.GitOpsIdentityModeProtectedScope, BaselineDigest: "scope", ObservedDigest: "scope",
	}
	if !retryableFluxConvergence(protected, transientErr, "main@sha1:older", "") {
		t.Fatal("protected-scope-equivalent descendant revision must be retryable")
	}
	protected.GitOpsIdentity.ObservedDigest = "changed"
	if retryableFluxConvergence(protected, transientErr, "main@sha1:older", "") {
		t.Fatal("protected-scope drift must fail immediately")
	}
	drifted := base
	drifted.GitOpsAttempted = "main@sha1:different"
	if retryableFluxConvergence(drifted, transientErr, "", "") {
		t.Fatal("differing applied and attempted revisions must fail immediately")
	}
	if retryableFluxConvergence(base, errors.New("operator deployment unavailable"), revision, "") {
		t.Fatal("unrelated preflight errors must fail immediately")
	}
	loomErr := errors.New(`loom-core convergence: flux loom-hub-servers is not converged: ready=false applied="main@sha1:def" attempted="main@sha1:def"`)
	loom := killtest.PreflightReport{LoomCoreRevision: "main@sha1:def", LoomCoreAttempted: "main@sha1:def"}
	if !retryableFluxConvergence(loom, loomErr, "", "main@sha1:def") {
		t.Fatal("unchanged loom-core source convergence must be retryable")
	}
	for _, source := range []string{"bootstrap", "system"} {
		rep := killtest.PreflightReport{}
		if source == "bootstrap" {
			rep.GitOpsBootstrapRevision = revision
			rep.GitOpsBootstrapAttempted = revision
		} else {
			rep.GitOpsSystemRevision = revision
			rep.GitOpsSystemAttempted = revision
		}
		err := fmt.Errorf("GitOps %s convergence: flux %s is not converged", source, source)
		if !retryableFluxConvergence(rep, err, revision, "") {
			t.Fatalf("unchanged %s source convergence must be retryable", source)
		}
		gotSource, gotRevision := retryingFluxSource(rep, err)
		if gotSource != source || gotRevision != revision {
			t.Fatalf("retryingFluxSource(%s) = %q %q", source, gotSource, gotRevision)
		}
	}
}

func TestRetryGatePreflightEventuallySucceeds(t *testing.T) {
	const revision = "main@sha1:abc"
	calls := 0
	probe := func(context.Context) (killtest.PreflightReport, error) {
		calls++
		rep := killtest.PreflightReport{GitOpsRevision: revision, GitOpsAttempted: revision}
		if calls == 1 {
			return rep, errors.New("GitOps convergence: flux apps is not converged")
		}
		rep.GitOpsReady = true
		rep.AllPreconditions = true
		return rep, nil
	}
	got, err := retryGatePreflight(context.Background(), probe, nil, revision, "", time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("retryGatePreflight() error = %v", err)
	}
	if calls != 2 || !got.AllPreconditions {
		t.Fatalf("retryGatePreflight() calls=%d report=%+v", calls, got)
	}
}

func TestRetryGatePreflightStopsWhenTargetIsNotRunning(t *testing.T) {
	const revision = "main@sha1:abc"
	calls := 0
	probe := func(context.Context) (killtest.PreflightReport, error) {
		calls++
		return killtest.PreflightReport{GitOpsRevision: revision, GitOpsAttempted: revision},
			errors.New("GitOps convergence: flux apps is not converged")
	}
	_, err := retryGatePreflight(context.Background(), probe, func(context.Context) error {
		return errors.New("allowed workflow is done")
	}, revision, "", time.Second, time.Millisecond)
	if err == nil || err.Error() != "allowed workflow is done" {
		t.Fatalf("retryGatePreflight() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
}

func TestRetryGatePreflightBoundsBlockedProbe(t *testing.T) {
	const timeout = 20 * time.Millisecond
	probe := func(ctx context.Context) (killtest.PreflightReport, error) {
		<-ctx.Done()
		return killtest.PreflightReport{}, ctx.Err()
	}
	started := time.Now()
	_, err := retryGatePreflight(context.Background(), probe, nil, "", "", timeout, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryGatePreflight() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked probe exceeded hard bound: %s", elapsed)
	}
}
