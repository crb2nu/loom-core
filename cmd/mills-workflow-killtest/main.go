// mills-workflow-killtest drives the S1c deployed dual-crash kill-test for
// the Mills imperative workflow runtime (plan .loom/134 §3) end-to-end:
//
//	preflight → launch canary → await pending spawn → confirm one pod
//	→ CRASH A (operator) → CRASH B (mobile-hud) → await terminal
//	→ collect dedupe evidence → PASS-1/2/4/5 verdicts → evidence JSON
//
// Prerequisites (runbook: docs/runbooks/mills-workflow-s1c-killtest.md):
//   - KUBECONFIG pointing at the k3s cluster.
//   - policy.workflows.enabled=true flipped via GitOps for the canary window
//     (this binary never flips it — the flip must be an audited commit).
//   - The operator REST surface reachable, e.g.
//     kubectl -n loom-mills port-forward svc/loom-mills-operator 8090:8090
//   - LOOM_MILLS_ADMIN_TOKEN for canary launch and crash leases.
//
// Exit code 0 = all verdicts PASS; 1 = any FAIL or phase error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/workflow/killtest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "killtest: %v\n", err)
		os.Exit(1)
	}
}

type runOutput struct {
	Preflight      killtest.PreflightReport  `json:"preflight"`
	FinalPreflight *killtest.PreflightReport `json:"final_preflight,omitempty"`
	Evidence       killtest.Evidence         `json:"evidence"`
	Verdicts       killtest.Verdicts         `json:"verdicts"`
}

type gateRunSummary struct {
	Index                  int    `json:"index"`
	EvidencePath           string `json:"evidence_path"`
	EvidenceSHA256         string `json:"evidence_sha256,omitempty"`
	PreviousEvidenceSHA256 string `json:"previous_evidence_sha256,omitempty"`
	RunID                  string `json:"run_id,omitempty"`
	AgentType              string `json:"agent_type"`
	FinalState             string `json:"final_state,omitempty"`
	Overall                bool   `json:"overall"`
	Error                  string `json:"error,omitempty"`
}

type gateSummary struct {
	GateContract        string           `json:"gate_contract"`
	GateContractVersion int              `json:"gate_contract_version"`
	GateID              string           `json:"gate_id"`
	GateStartedAt       time.Time        `json:"gate_started_at"`
	RequiredRuns        int              `json:"required_runs"`
	CompletedRuns       int              `json:"completed_runs"`
	Overall             bool             `json:"overall"`
	AgentType           string           `json:"agent_type"`
	OperatorImage       string           `json:"operator_image_id,omitempty"`
	HudImage            string           `json:"hud_image_id,omitempty"`
	PolicyChecksum      string           `json:"policy_checksum,omitempty"`
	GitOpsIdentityMode  string           `json:"gitops_identity_mode,omitempty"`
	GitOpsBaseline      string           `json:"gitops_baseline_revision,omitempty"`
	GitOpsScopeDigest   string           `json:"gitops_scope_digest,omitempty"`
	LoomCoreBaseline    string           `json:"loom_core_baseline_revision,omitempty"`
	LoomCoreScopeDigest string           `json:"loom_core_scope_digest,omitempty"`
	Runs                []gateRunSummary `json:"runs"`
}

const (
	gatePreflightTimeout = 2 * time.Minute
	gatePreflightPoll    = 5 * time.Second

	// settledAuthorizationWindow bounds how long a delete boundary may wait
	// for the observer's in-flight kubectl-exec sample to complete. It must
	// stay below killtest.ProcessEvidenceMaxSampleGap so a genuinely overdue
	// observer still fails closed via the max-gap fence inside
	// AuthorizeActiveDelete rather than being retried past it.
	settledAuthorizationWindow = 2500 * time.Millisecond
	settledAuthorizationPoll   = 20 * time.Millisecond
)

// inFlightSampleSentinel is the AuthorizeActiveDelete error for an instant
// that lands inside a sampling window. Off-cluster vantages routinely see
// kubectl-exec sample RTT exceed ProcessPollInterval, which makes a
// single-instant authorization a coin flip; retrying until the sample
// completes authorizes on the freshest possible completed sample.
const inFlightSampleSentinel = "in-flight sample at the delete boundary"

func authorizeActiveDeleteSettled(o *killtest.CanaryProcessObserver) (killtest.ProcessDeleteAuthorization, time.Time, error) {
	deadline := time.Now().Add(settledAuthorizationWindow)
	for {
		at := time.Now().UTC()
		authorization, err := o.AuthorizeActiveDelete(at)
		if err == nil {
			return authorization, at, nil
		}
		if !strings.Contains(err.Error(), inFlightSampleSentinel) || time.Now().After(deadline) {
			return killtest.ProcessDeleteAuthorization{}, at, err
		}
		time.Sleep(settledAuthorizationPoll)
	}
}

func assertActiveFreshSettled(o *killtest.CanaryProcessObserver) error {
	deadline := time.Now().Add(settledAuthorizationWindow)
	for {
		err := o.AssertActiveFreshForDelete()
		if err == nil || !strings.Contains(err.Error(), inFlightSampleSentinel) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(settledAuthorizationPoll)
	}
}

// mergingMode mirrors the -merging flag for the deep run path (runOne) without
// threading one more parameter through every phase signature.
var mergingMode bool

func run() (runErr error) {
	var (
		operatorURL       = flag.String("operator-url", "http://localhost:8090", "restart-stable operator REST base URL")
		adminToken        = flag.String("admin-token", os.Getenv("LOOM_MILLS_ADMIN_TOKEN"), "admin bearer token (default $LOOM_MILLS_ADMIN_TOKEN)")
		expectedGitOps    = flag.String("expected-gitops-revision", os.Getenv("S1C_EXPECTED_GITOPS_REVISION"), "reviewed remote GitOps main SHA (required for a full gate)")
		expectedLoomCore  = flag.String("expected-loom-core-revision", os.Getenv("S1C_EXPECTED_LOOM_CORE_REVISION"), "reviewed remote loom-core main SHA (required for a full gate)")
		gitOpsIdentity    = flag.String("gitops-identity-mode", envOrDefault("S1C_GITOPS_IDENTITY_MODE", killtest.GitOpsIdentityModeExactRevision), "GitOps identity contract: exact-revision | protected-scope")
		gitOpsRepo        = flag.String("gitops-repo", os.Getenv("S1C_GITOPS_REPO"), "local platform/gitops repository used for protected identity and reviewed Flux spec binding")
		loomCoreRepo      = flag.String("loom-core-repo", os.Getenv("S1C_LOOM_CORE_REPO"), "local loom-core repository used for protected identity and exact reviewed Deployment rendering")
		fluxBin           = flag.String("flux-bin", envOrDefault("S1C_FLUX_BIN", "flux"), "Flux CLI used for exact reviewed Deployment rendering")
		hudURL            = flag.String("hud-url", envOrDefault("S1C_HUD_URL", "https://hud.flexinfer.ai"), "restart-stable mobile-hud base URL used for exact spawn cleanup")
		hudAdminToken     = flag.String("hud-admin-token", os.Getenv("HUD_ADMIN_TOKEN"), "mobile-hud admin token used for exact spawn cleanup (default $HUD_ADMIN_TOKEN)")
		phase             = flag.String("phase", "full", "phase to run: preflight | full | verify")
		attachRunID       = flag.String("run-id", "", "attach to an existing running imperative run instead of launching a fresh canary")
		agentType         = flag.String("agent-type", killtest.AgentTypeClaudeCode, "canary spawn agent: claude-code | codex")
		evidence          = flag.String("evidence", "s1c-evidence.json", "path to write the evidence JSON")
		runs              = flag.Int("runs", 3, "number of consecutive full dual-crash runs (the S1c gate requires exactly 3)")
		stepTimeout       = flag.Duration("step-timeout", 5*time.Minute, "max wait for the pending spawn step")
		termTimeout       = flag.Duration("terminal-timeout", 30*time.Minute, "max wait for the run to reach a terminal state")
		crashDelay        = flag.Duration("crash-delay", 15*time.Second, "wait after the spawn is confirmed before CRASH A, and between CRASH A and CRASH B")
		merging           = flag.Bool("merging", false, "S6-full merging canary: template v3 with a journaled merge('canary') effect; PASS-3 evaluated from real GitLab evidence")
		gitlabAPIURL      = flag.String("gitlab-api-url", envOrDefault("S1C_GITLAB_API_URL", "https://gitlab.flexinfer.ai/api/v4"), "GitLab API base for PASS-3 merge verification (merging mode)")
		gitlabToken       = flag.String("gitlab-token", os.Getenv("GITLAB_TOKEN"), "GitLab token for PASS-3 merge verification (default $GITLAB_TOKEN)")
		gitlabProject     = flag.String("gitlab-project", envOrDefault("S1C_GITLAB_PROJECT", "services/loom-core"), "GitLab project the merging canary merges into")
		scenario          = flag.String("scenario", "", "deterministic scenario: queued-proof | mr-awareness")
		scenarioMaxAge    = flag.Duration("scenario-max-age", 5*time.Minute, "maximum age of scenario evidence")
		queuedProofID     = flag.String("queued-proof-backlog-id", "", "existing queued backlog item to drive through live terminal MR proof")
		queuedProofPlan   = flag.String("queued-proof-plan-id", "", "canonical Pattern Loom plan id to seed for the live queued-proof")
		queuedProofTarget = flag.String("queued-proof-target-project", "", "declared target project for the queued proof (required for live runs)")
		queuedProofResume = flag.Bool("queued-proof-resume", false, "resume the admitted run recorded in --evidence instead of starting another run")
		queuedProofPoll   = flag.Duration("queued-proof-poll", 5*time.Second, "live queued-proof polling interval")
	)
	flag.Parse()
	if *scenario != "" {
		if *scenario == string(pipeline.KilltestQueuedProof) && (*queuedProofID != "" || *queuedProofPlan != "") {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runLiveQueuedProof(ctx, queuedProofDriver{operatorURL: *operatorURL, adminToken: *adminToken, gitlabURL: *gitlabAPIURL, gitlabToken: *gitlabToken, gitlabProject: *queuedProofTarget, client: &http.Client{Timeout: 30 * time.Second}, poll: *queuedProofPoll, resume: *queuedProofResume}, *queuedProofID, *queuedProofPlan, *evidence, *termTimeout)
		}
		return runScenario(pipeline.KilltestScenario(*scenario), *evidence, *scenarioMaxAge, time.Now().UTC())
	}
	if err := validateOptions(*phase, *runs, *merging, *attachRunID, *adminToken,
		*expectedGitOps, *expectedLoomCore, *gitOpsIdentity, *gitOpsRepo, *loomCoreRepo,
		*hudURL, *hudAdminToken, *agentType); err != nil {
		return err
	}
	if *phase == "verify" {
		if err := verifyGateEvidence(*evidence); err != nil {
			return fmt.Errorf("verify S1c evidence: %w", err)
		}
		fmt.Printf("S1c gate evidence VERIFIED — all declared runs distinct and clean: %s\n", *evidence)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h := killtest.New(killtest.Config{
		OperatorURL:              *operatorURL,
		AdminToken:               *adminToken,
		ExpectedGitOpsRevision:   *expectedGitOps,
		ExpectedLoomCoreRevision: *expectedLoomCore,
		GitOpsIdentityMode:       *gitOpsIdentity,
		GitOpsRepoPath:           *gitOpsRepo,
		LoomCoreRepoPath:         *loomCoreRepo,
		FluxBin:                  *fluxBin,
		HudURL:                   *hudURL,
		HudAdminToken:            *hudAdminToken,
		StepTimeout:              *stepTimeout,
		TerminalTimeout:          *termTimeout,
		RequireAuthorityBinding:  true,
		Merging:                  *merging,
		GitLabAPIURL:             *gitlabAPIURL,
		GitLabToken:              *gitlabToken,
		GitLabProject:            *gitlabProject,
	})
	if *merging && strings.TrimSpace(*gitlabToken) == "" {
		return errors.New("merging mode requires -gitlab-token (or $GITLAB_TOKEN) for PASS-3 verification")
	}
	mergingMode = *merging
	closeAttempted := false
	closeHarness := func() error {
		closeAttempted = true
		return joinHarnessCleanupErrors(nil, h.Close())
	}
	defer func() {
		if !closeAttempted {
			runErr = joinHarnessCleanupErrors(runErr, h.Close())
		}
	}()

	if *phase == "preflight" {
		rep, err := preflightForGate(ctx, h, *attachRunID, "", "")
		fmt.Printf("== preflight ==\n%s\n", mustJSON(rep))
		if err != nil {
			return fmt.Errorf("preflight: %w", err)
		}
		if *attachRunID != "" {
			if err := h.ValidateCanaryRun(ctx, *attachRunID, *agentType); err != nil {
				return fmt.Errorf("preflight attached canary identity: %w", err)
			}
		}
		return nil
	}
	gateMode := *attachRunID == ""
	if err := validateEvidenceDestination(*evidence); err != nil {
		return fmt.Errorf("evidence destination: %w", err)
	}

	gateID := ""
	gateStartedAt := time.Time{}
	if gateMode {
		var err error
		gateID, err = killtest.NewCanaryGateID()
		if err != nil {
			return fmt.Errorf("allocate S1c gate identity: %w", err)
		}
		gateStartedAt = time.Now().UTC()
	}
	summary := gateSummary{
		GateContract: killtest.GateBindingContract, GateContractVersion: killtest.GateBindingContractVersion,
		GateID: gateID, GateStartedAt: gateStartedAt,
		RequiredRuns: *runs, AgentType: *agentType,
	}
	// Invalidate any older passing summary at this path before the first
	// preflight. A crash or cancellation during run 1 must leave an incomplete
	// current gate, never a stale prior PASS that still verifies.
	if gateMode {
		if err := writeSummary(*evidence, *runs, summary); err != nil {
			return fmt.Errorf("write initial S1c gate checkpoint: %w", err)
		}
	}
	previousEvidenceSHA256 := ""
	var gateIdentity *killtest.PreflightReport
	var previousFinalPreflight *killtest.PreflightReport
	for i := 1; i <= *runs; i++ {
		baselineRevision, loomCoreBaselineRevision := "", ""
		if gateIdentity != nil {
			baselineRevision = gateIdentity.GitOpsRevision
			loomCoreBaselineRevision = gateIdentity.LoomCoreRevision
		}
		rep, err := preflightForGate(ctx, h, *attachRunID, baselineRevision, loomCoreBaselineRevision)
		fmt.Printf("== preflight run %d/%d ==\n%s\n", i, *runs, mustJSON(rep))
		path := runEvidencePath(*evidence, i, *runs, *attachRunID == "")
		if err := validateEvidenceDestination(path); err != nil {
			return fmt.Errorf("evidence destination: %w", err)
		}
		entry := gateRunSummary{
			Index: i, EvidencePath: path, AgentType: *agentType,
			PreviousEvidenceSHA256: previousEvidenceSHA256,
		}
		if err != nil {
			entry.Error = err.Error()
			summary.Runs = append(summary.Runs, entry)
			if writeErr := writeSummary(*evidence, *runs, summary); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("run %d preflight: %w", i, err)
		}
		if gateIdentity == nil {
			copy := rep
			gateIdentity = &copy
			summary.OperatorImage = rep.Operator.ImageID
			summary.HudImage = rep.Hud.ImageID
			summary.PolicyChecksum = rep.PolicyChecksum
			summary.GitOpsIdentityMode = rep.GitOpsIdentity.Mode
			summary.GitOpsBaseline = rep.GitOpsIdentity.BaselineRevision
			summary.GitOpsScopeDigest = rep.GitOpsIdentity.ObservedDigest
			summary.LoomCoreBaseline = rep.LoomCoreIdentity.BaselineRevision
			summary.LoomCoreScopeDigest = rep.LoomCoreIdentity.ObservedDigest
		} else if err := sameGateIdentity(*gateIdentity, rep); err != nil {
			entry.Error = err.Error()
			summary.Runs = append(summary.Runs, entry)
			if writeErr := writeSummary(*evidence, *runs, summary); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("run %d identity drift: %w", i, err)
		}
		if gateMode && previousFinalPreflight != nil {
			if err := killtest.ValidateInterRunPodContinuity(*previousFinalPreflight, rep); err != nil {
				entry.Error = err.Error()
				summary.Runs = append(summary.Runs, entry)
				if writeErr := writeSummary(*evidence, *runs, summary); writeErr != nil {
					return writeErr
				}
				return fmt.Errorf("run %d inter-run pod continuity: %w", i, err)
			}
		}

		binding := killtest.GateBinding{}
		if gateMode {
			binding = killtest.GateBinding{
				Contract: killtest.GateBindingContract, ContractVersion: killtest.GateBindingContractVersion,
				GateID: gateID, RunIndex: i, RequiredRuns: *runs, GateStartedAt: gateStartedAt,
				PreviousEvidenceSHA256: previousEvidenceSHA256,
			}
		}
		out, runErr := runOne(ctx, h, rep, *attachRunID, binding, *agentType, path, *stepTimeout, *crashDelay)
		if runErr != nil && out.Evidence.RunID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
			cleanupErr := h.CleanupRun(cleanupCtx, out.Evidence.RunID, out.Evidence.SpawnID, "S1c harness failure: "+runErr.Error())
			cancel()
			if cleanupErr != nil {
				runErr = fmt.Errorf("%w; automatic /fail cleanup also failed: %v", runErr, cleanupErr)
			} else {
				fmt.Fprintf(os.Stderr, "failed canary %s and its exact spawn are verified terminal\n", out.Evidence.RunID)
			}
		}
		writeErr := writeJSON(path, out)
		if writeErr != nil {
			if runErr == nil {
				runErr = fmt.Errorf("write evidence: %w", writeErr)
			} else {
				runErr = fmt.Errorf("%v; write evidence: %w", runErr, writeErr)
			}
		}
		if writeErr == nil {
			digest, digestErr := evidenceFileSHA256(path)
			if digestErr != nil {
				if runErr == nil {
					runErr = fmt.Errorf("hash final evidence: %w", digestErr)
				} else {
					runErr = fmt.Errorf("%v; hash final evidence: %w", runErr, digestErr)
				}
			} else {
				entry.EvidenceSHA256 = digest
				previousEvidenceSHA256 = digest
			}
		}
		entry.RunID = out.Evidence.RunID
		entry.FinalState = out.Evidence.Final.Run.State
		entry.Overall = out.Verdicts.Overall
		if runErr != nil {
			entry.Error = runErr.Error()
		}
		summary.Runs = append(summary.Runs, entry)
		if out.Verdicts.Overall && runErr == nil {
			summary.CompletedRuns++
		}
		if err := writeSummary(*evidence, *runs, summary); err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("run %d/%d: %w", i, *runs, runErr)
		}
		if gateMode {
			finalCopy := out.Evidence.FinalPreflight
			previousFinalPreflight = &finalCopy
			fmt.Printf("S1c kill-test run PASSED — evidence: %s\n", path)
		} else {
			fmt.Printf("S1c recovery run evidence complete — cleanup pending: %s\n", path)
		}
	}

	summary.Overall = summary.CompletedRuns == summary.RequiredRuns
	if err := writeSummary(*evidence, *runs, summary); err != nil {
		return err
	}
	if !summary.Overall {
		return fmt.Errorf("S1c gate FAILED — see %s", *evidence)
	}
	if !gateMode {
		if err := closeHarness(); err != nil {
			return err
		}
		fmt.Printf("S1c recovery run PASSED (non-gating) — evidence: %s\n", *evidence)
		return nil
	}
	if err := verifyAndSealGateSummary(*evidence, *runs, &summary, verifyGateEvidence); err != nil {
		return err
	}
	if err := closeHarness(); err != nil {
		return err
	}
	fmt.Printf("S1c gate PASSED — %d consecutive runs; summary: %s\n", *runs, *evidence)
	return nil
}

func runScenario(scenario pipeline.KilltestScenario, evidencePath string, maxAge time.Duration, now time.Time) error {
	return runScenarioTo(os.Stdout, scenario, evidencePath, maxAge, now)
}

func runScenarioTo(out io.Writer, scenario pipeline.KilltestScenario, evidencePath string, maxAge time.Duration, now time.Time) error {
	if scenario != pipeline.KilltestQueuedProof && scenario != pipeline.KilltestMRAwareness {
		return writeScenarioFailure(out, scenario, "unknown_scenario", fmt.Errorf("invalid --scenario %q (want queued-proof or mr-awareness)", scenario))
	}
	f, err := os.Open(evidencePath)
	if err != nil {
		return writeScenarioFailure(out, scenario, "evidence_unavailable", fmt.Errorf("open scenario evidence: %w", err))
	}
	defer f.Close()
	var evidence pipeline.KilltestEvidence
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return writeScenarioFailure(out, scenario, "malformed_evidence", fmt.Errorf("decode scenario evidence: %w", err))
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return writeScenarioFailure(out, scenario, "trailing_json", errors.New("decode scenario evidence: trailing JSON data"))
	}
	report, err := pipeline.AssertKilltestScenario(scenario, evidence, now, maxAge)
	if err != nil {
		return writeScenarioFailure(out, scenario, pipeline.KilltestFailureCode(err), fmt.Errorf("scenario %s FAILED: %w", scenario, err))
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode scenario report: %w", err)
	}
	fmt.Fprintf(out, "%s\n", encoded)
	return nil
}

func writeScenarioFailure(out io.Writer, scenario pipeline.KilltestScenario, code string, cause error) error {
	report := pipeline.FailKilltestReport(scenario, code, cause)
	if err := json.NewEncoder(out).Encode(report); err != nil {
		return fmt.Errorf("encode scenario failure report: %v (original failure: %w)", err, cause)
	}
	return cause
}

func verifyAndSealGateSummary(
	path string,
	runs int,
	summary *gateSummary,
	verify func(string) error,
) error {
	if summary == nil || verify == nil {
		return errors.New("completed gate verification requires a summary and verifier")
	}
	if err := verify(path); err != nil {
		summary.Overall = false
		if writeErr := writeSummary(path, runs, *summary); writeErr != nil {
			return fmt.Errorf("canonical S1c evidence verification failed: %w; invalidate summary: %v", err, writeErr)
		}
		return fmt.Errorf("canonical S1c evidence verification failed; summary invalidated: %w", err)
	}
	return nil
}

func validateOptions(
	phase string,
	runs int,
	merging bool,
	runID, adminToken, expectedGitOps, expectedLoomCore, gitOpsIdentityMode,
	gitOpsRepo, loomCoreRepo, hudURL, hudAdminToken, agentType string,
) error {
	if phase != "preflight" && phase != "full" && phase != "verify" {
		return fmt.Errorf("invalid phase %q", phase)
	}
	if phase == "verify" {
		return nil
	}
	if err := killtest.ValidateAgentType(agentType); err != nil {
		return err
	}
	if gitOpsIdentityMode != killtest.GitOpsIdentityModeExactRevision &&
		gitOpsIdentityMode != killtest.GitOpsIdentityModeProtectedScope {
		return fmt.Errorf("invalid --gitops-identity-mode %q", gitOpsIdentityMode)
	}
	if gitOpsIdentityMode == killtest.GitOpsIdentityModeProtectedScope &&
		(strings.TrimSpace(gitOpsRepo) == "" || strings.TrimSpace(loomCoreRepo) == "") {
		return fmt.Errorf("--gitops-repo/S1C_GITOPS_REPO and --loom-core-repo/S1C_LOOM_CORE_REPO are required for protected-scope identity")
	}
	if phase == "full" && strings.TrimSpace(gitOpsRepo) == "" {
		return fmt.Errorf("--gitops-repo/S1C_GITOPS_REPO is required to bind live Flux specs to reviewed manifests")
	}
	if phase == "full" && strings.TrimSpace(loomCoreRepo) == "" {
		return fmt.Errorf("--loom-core-repo/S1C_LOOM_CORE_REPO is required to render the reviewed mobile-hud Deployment")
	}
	if phase == "full" && runID == "" && !merging && runs != 3 {
		return fmt.Errorf("the S1c full gate requires exactly --runs 3")
	}
	if phase == "full" && runID == "" && merging && runs != 1 {
		// Each merging run merges its canary MR into main, moving the
		// expected-revision identity baseline out from under the next run.
		return fmt.Errorf("the S6-full merging gate requires exactly --runs 1")
	}
	if phase == "full" && runID == "" && gitOpsIdentityMode != killtest.GitOpsIdentityModeProtectedScope {
		return fmt.Errorf("the S1c full gate requires --gitops-identity-mode %s", killtest.GitOpsIdentityModeProtectedScope)
	}
	if runID != "" && runs != 1 {
		return fmt.Errorf("--run-id is recovery-only and requires --runs 1")
	}
	if phase == "preflight" && (runs < 1 || runs > 10) {
		return fmt.Errorf("runs must be between 1 and 10")
	}
	if phase == "full" && adminToken == "" {
		return fmt.Errorf("LOOM_MILLS_ADMIN_TOKEN or --admin-token is required")
	}
	if phase == "full" && strings.TrimSpace(expectedGitOps) == "" {
		return fmt.Errorf("S1C_EXPECTED_GITOPS_REVISION or --expected-gitops-revision is required")
	}
	if phase == "full" && strings.TrimSpace(expectedLoomCore) == "" {
		return fmt.Errorf("S1C_EXPECTED_LOOM_CORE_REVISION or --expected-loom-core-revision is required")
	}
	if phase == "full" && (strings.TrimSpace(hudURL) == "" || strings.TrimSpace(hudAdminToken) == "") {
		return fmt.Errorf("--hud-url and HUD_ADMIN_TOKEN or --hud-admin-token are required for fail-safe spawn cleanup")
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func runOne(ctx context.Context, h *killtest.Harness, rep killtest.PreflightReport, attachRunID string, gateBinding killtest.GateBinding, agentType, evidencePath string, stepTimeout, crashDelay time.Duration) (runOutput, error) {
	runID := attachRunID
	ev := killtest.Evidence{GateBinding: gateBinding, RunID: runID, AgentType: agentType, InitialPreflight: rep}
	checkpoint := func() error {
		return writeJSON(evidencePath, runOutput{Preflight: rep, Evidence: ev, Verdicts: killtest.Evaluate(ev)})
	}
	var observer *killtest.SpawnPodObserver
	observerStopped := false
	defer func() {
		if observer != nil && !observerStopped {
			_ = observer.Stop(nil)
		}
	}()
	if runID == "" {
		requestedRunID, err := killtest.CanaryRunIDForGate(gateBinding.GateID, gateBinding.RunIndex)
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("allocate canary identity: %w", err)
		}
		runID = requestedRunID
		ev.RunID = runID
		if err := checkpoint(); err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write pre-launch checkpoint: %w", err)
		}
		observer, err = h.StartSpawnNamespaceObservation(ctx)
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("start pre-launch namespace spawn pod observation: %w", err)
		}
		observer.RecordStart(&ev)
		if err := checkpoint(); err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write pre-launch watch checkpoint: %w", err)
		}
		launchedRunID, err := h.LaunchCanaryWithRequestObserver(ctx, requestedRunID, agentType, func(requestedAt time.Time) {
			ev.CanaryLaunchRequestedAt = requestedAt
		})
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("launch: %w", err)
		}
		runID = launchedRunID
	} else {
		if err := h.ValidateCanaryRun(ctx, runID, agentType); err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("attach canary identity: %w", err)
		}
		fmt.Printf("attaching to existing run %s\n", runID)
	}
	ev.RunID = runID
	if err := checkpoint(); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write launch checkpoint: %w", err)
	}

	st, err := h.AwaitPendingSpawn(ctx, runID)
	if err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("await spawn: %w", err)
	}

	// A healthy in-flight dispatch has no spawn_id in the journal yet —
	// derive the deterministic identity from run_id + step_key + call_hash.
	identity, err := killtest.DeriveSpawnIdentity(runID, st)
	if err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("derive spawn identity: %w", err)
	}
	spawnID, podName := identity.SpawnID, identity.PodName
	fmt.Printf("spawn identity: spawn_id=%s pod=%s (derived and verified)\n", spawnID, podName)

	ev.AgentStepKey = st.StepKey
	ev.SpawnID = spawnID
	ev.SpawnPodName = podName
	ev.ExpectedIdempotencyKey = identity.IdempotencyKey
	ev.BaselineSpawnRecordIDs = append([]string(nil), rep.SpawnRecordIDs...)
	// An attached run's own durable record is allowed; it is not baseline.
	for i, id := range ev.BaselineSpawnRecordIDs {
		if id == spawnID {
			ev.BaselineSpawnRecordIDs = append(ev.BaselineSpawnRecordIDs[:i], ev.BaselineSpawnRecordIDs[i+1:]...)
			break
		}
	}
	if attachRunID == "" {
		if err := observer.BindSpawnIdentity(spawnID); err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("bind pre-launch spawn pod observation: %w", err)
		}
	} else {
		observer, err = h.StartSpawnPodObservation(ctx, spawnID)
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("start recovery spawn pod observation: %w", err)
		}
		observer.RecordStart(&ev)
	}

	// §3.3 step 3: confirm exactly one pod EXISTS before crashing anything —
	// the meaningful crash window is while the spawn pod runs. The pod may
	// lag the journal row by minutes (spawn runtime image build), so poll.
	var concurrent, ready int
	var spawnStatus string
	var names []string
	var hold killtest.CanaryHoldObservation
	podDeadline := time.Now().Add(stepTimeout)
	for {
		if err := observer.AssertHealthy(); err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("continuous spawn pod observation: %w", err)
		}
		var err error
		concurrent, ready, names, err = h.SpawnPodStatus(ctx, spawnID)
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("count spawn pods: %w", err)
		}
		if concurrent > 1 {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("pre-crash duplicate: %d active pods for %s", concurrent, spawnID)
		}
		spawnStatus, err = h.SpawnRecordStatus(ctx, spawnID)
		if err != nil {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("read durable spawn status: %w", err)
		}
		if concurrent == 1 && ready == 1 && spawnStatus == "running" {
			var holdReady bool
			hold, holdReady, err = h.ProbeCanaryHold(ctx, spawnID)
			if err != nil {
				return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("prove canary hold process: %w", err)
			}
			if holdReady {
				break
			}
		}
		if time.Now().After(podDeadline) {
			return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("exact canary hold in spawn pod %s was not proven within %s (active=%d ready=%d durable_status=%s)",
				podName, stepTimeout, concurrent, ready, spawnStatus)
		}
		fmt.Printf("waiting for exact agent execution window %s (active=%d ready=%d durable_status=%s hold_pid=%d driver_pid=%d)...\n",
			podName, concurrent, ready, spawnStatus, hold.PID, hold.DriverPID)
		poll := 10 * time.Second
		if concurrent == 1 && ready == 1 && spawnStatus == "running" {
			poll = time.Second
		}
		wait(ctx, poll)
		if ctx.Err() != nil {
			return runOutput{Preflight: rep, Evidence: ev}, ctx.Err()
		}
	}
	fmt.Printf("== pre-crash == spawn pods: %d active, %d Running+Ready, durable_status=%s, hold_pid=%d, driver_pid=%d, names=%v\n",
		concurrent, ready, spawnStatus, hold.PID, hold.DriverPID, names)
	ev.MaxConcurrentSpawnPods = concurrent
	ev.TotalSpawnPodNames = names
	ev.CanaryHoldInitial = hold
	if err := h.CaptureSpawnState(ctx, &ev); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("capture pre-crash spawn state: %w", err)
	}
	if err := checkpoint(); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write pre-crash checkpoint: %w", err)
	}

	wait(ctx, crashDelay)

	// CRASH A: operator.
	crashARep, err := preflightForGate(ctx, h, runID, rep.GitOpsRevision, rep.LoomCoreRevision)
	if err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A immediate preflight: %w", err)
	}
	if err := sameGateIdentity(rep, crashARep); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A identity gate: %w", err)
	}
	if err := killtest.ValidateGateIdentityContinuity(rep, crashARep); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A serialized identity gate: %w", err)
	}
	if err := killtest.ValidateCrashAPodContinuity(rep, crashARep); err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A pod continuity gate: %w", err)
	}
	sourceFenceA, err := h.PrepareSourceIdentityFence(ctx, crashARep)
	if err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A prepare coherent Flux source identity fence: %w", err)
	}
	leaseA, err := h.AcquireCrashLease(ctx, runID, spawnID)
	if err != nil {
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A lease: %w", err)
	}
	ev.CrashASafety.ImmediatePreflight = crashARep
	ev.CrashASafety.LeaseAcquired = leaseA.Evidence()
	ev.CrashABefore = crashARep.Operator
	ev.CrashASafety.DeleteIntentRecordedAt = time.Now().UTC()
	if err := checkpoint(); err != nil {
		err = releaseCrashLeaseAfter(h, leaseA.Token, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("persist CRASH A delete intent: %w", err)
	}
	var processObserver *killtest.CanaryProcessObserver
	replacement, renewedLeaseA, crashErr := h.CrashPodWithLeaseEvidenceAndHooks(ctx, "loom-mills", "app.kubernetes.io/name=loom-mills-operator", "loom-mills-operator", crashARep.Operator, crashARep.OperatorDeployment, leaseA, func(checkCtx context.Context) error {
		if err := observer.AssertHealthy(); err != nil {
			return fmt.Errorf("continuous spawn pod observation before CRASH A: %w", err)
		}
		targetSafety, err := h.AssertSafeToCrash(checkCtx, runID, spawnID)
		if err != nil {
			return fmt.Errorf("prove exact crash target safety: %w", err)
		}
		ev.CrashASafety.Target = targetSafety
		holdA, holdReady, err := h.ProbeCanaryHold(checkCtx, spawnID)
		if err != nil {
			return fmt.Errorf("probe exact canary hold: %w", err)
		}
		if err := validateCanaryHoldRecheck(hold, holdA, holdReady); err != nil {
			return err
		}
		ev.CanaryHoldBeforeCrashA = holdA
		observationStart := time.Now().UTC()
		processObserver, err = h.StartPausedCanaryProcessObservation(ctx, spawnID, hold, observationStart)
		if err != nil {
			return fmt.Errorf("start pre-CRASH A process observer: %w", err)
		}
		ev.ProcessObservationStartedAt = observationStart
		fluxProvenance, err := h.FinalizeSourceIdentityFence(checkCtx, sourceFenceA)
		if err != nil {
			return fmt.Errorf("final coherent Flux source snapshot immediately before delete: %w", err)
		}
		ev.CrashAFluxProvenance = fluxProvenance
		if err := processObserver.AssertFreshForDelete(); err != nil {
			return fmt.Errorf("final process observation freshness immediately before CRASH A: %w", err)
		}
		if err := processObserver.Activate(); err != nil {
			return fmt.Errorf("activate crash-window process observer after CRASH A source fence: %w", err)
		}
		return nil
	}, func() error {
		if processObserver == nil {
			return errors.New("verify crash-window process observer: observer was not prepared")
		}
		policyBoundary, err := h.CollectPolicyDeleteBoundaryEvidence(ctx, crashARep)
		if err != nil {
			return fmt.Errorf("refresh policy at CRASH A delete boundary: %w", err)
		}
		ev.CrashASafety.PolicyDeleteBoundary = policyBoundary
		authorization, deleteAt, err := authorizeActiveDeleteSettled(processObserver)
		if err != nil {
			return fmt.Errorf("authorize CRASH A from active process observation: %w", err)
		}
		if err := killtest.ValidateDeleteBoundaryFreshness(deleteAt, ev.CrashASafety.Target, ev.CrashAFluxProvenance); err != nil {
			return fmt.Errorf("CRASH A delete-boundary freshness: %w", err)
		}
		if err := killtest.ValidatePolicyDeleteBoundaryFreshness(
			deleteAt, crashARep, ev.CrashASafety.PolicyDeleteBoundary,
		); err != nil {
			return fmt.Errorf("CRASH A policy delete-boundary freshness: %w", err)
		}
		ev.CrashAAt = deleteAt
		ev.CrashASafety.DeleteRequestedAt = deleteAt
		ev.CrashAProcessAuthorization = authorization
		if err := observer.AssertHealthy(); err != nil {
			return fmt.Errorf("final continuous spawn pod observation before CRASH A DELETE: %w", err)
		}
		return nil
	}, func(acceptedAt time.Time, renewed killtest.CrashLease) error {
		ev.CrashASafety.LeaseRenewed = renewed.Evidence()
		ev.CrashASafety.DeleteAcceptedAt = acceptedAt
		processErr := processObserver.Record(&ev)
		return errors.Join(processErr, checkpoint())
	})
	ev.CrashASafety.LeaseRenewed = renewedLeaseA.Evidence()
	if err := releaseCrashLeaseAfter(h, leaseA.Token, crashErr); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A: %w", err)
	}
	ev.CrashAReplacement = replacement
	if processObserver == nil {
		return runOutput{Preflight: rep, Evidence: ev}, errors.New("CRASH A completed without a process observer")
	}
	if err := processObserver.Record(&ev); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH A process observation: %w", err)
	}
	if err := checkpoint(); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write CRASH A checkpoint: %w", err)
	}
	stopObservedFailure := func(cause error) (runOutput, error) {
		cause = stopProcessObserverAfter(processObserver, &ev, cause)
		return runOutput{Preflight: rep, Evidence: ev}, cause
	}

	wait(ctx, crashDelay)

	// CRASH B: mobile-hud, interleaved before the operator's resume completes
	// (the operator's first tick fires on boot; the 60s scheduler cadence plus
	// the spawn's minutes-long runtime keep the resume in flight).
	crashBRep, err := preflightForGate(ctx, h, runID, rep.GitOpsRevision, rep.LoomCoreRevision)
	if err != nil {
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("CRASH B immediate preflight: %w", observedErr)
	}
	if err := sameGateIdentity(rep, crashBRep); err != nil {
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("CRASH B identity gate: %w", observedErr)
	}
	if err := killtest.ValidateGateIdentityContinuity(rep, crashBRep); err != nil {
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("CRASH B serialized identity gate: %w", observedErr)
	}
	if err := killtest.ValidateCrashBPodContinuity(crashARep, crashBRep, ev.CrashAReplacement); err != nil {
		out, observedErr := stopObservedFailure(fmt.Errorf("CRASH B pod continuity gate: %w", err))
		return out, observedErr
	}
	sourceFenceB, err := h.PrepareSourceIdentityFence(ctx, crashBRep)
	if err != nil {
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("CRASH B prepare coherent Flux source identity fence: %w", observedErr)
	}
	leaseB, err := h.AcquireCrashLease(ctx, runID, spawnID)
	if err != nil {
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("CRASH B lease: %w", observedErr)
	}
	ev.CrashBSafety.ImmediatePreflight = crashBRep
	ev.CrashBSafety.LeaseAcquired = leaseB.Evidence()
	ev.CrashBBefore = crashBRep.Hud
	ev.CrashBSafety.DeleteIntentRecordedAt = time.Now().UTC()
	if err := checkpoint(); err != nil {
		err = releaseCrashLeaseAfter(h, leaseB.Token, err)
		out, observedErr := stopObservedFailure(err)
		return out, fmt.Errorf("persist CRASH B delete intent: %w", observedErr)
	}
	replacement, renewedLeaseB, crashErr := h.CrashPodWithLeaseEvidenceAndHooks(ctx, "loom-hub", "app=mobile-hud", "mobile-hud", crashBRep.Hud, crashBRep.HudDeployment, leaseB, func(checkCtx context.Context) error {
		if err := observer.AssertHealthy(); err != nil {
			return fmt.Errorf("continuous spawn pod observation before CRASH B: %w", err)
		}
		targetSafety, err := h.AssertSafeToCrash(checkCtx, runID, spawnID)
		if err != nil {
			return fmt.Errorf("prove exact crash target safety: %w", err)
		}
		ev.CrashBSafety.Target = targetSafety
		holdB, holdReady, err := h.ProbeCanaryHold(checkCtx, spawnID)
		if err != nil {
			return fmt.Errorf("probe exact canary hold: %w", err)
		}
		if err := validateCanaryHoldRecheck(hold, holdB, holdReady); err != nil {
			return err
		}
		ev.CanaryHoldBeforeCrashB = holdB
		fluxProvenance, err := h.FinalizeSourceIdentityFence(checkCtx, sourceFenceB)
		if err != nil {
			return fmt.Errorf("final coherent Flux source snapshot immediately before delete: %w", err)
		}
		ev.CrashBFluxProvenance = fluxProvenance
		if err := assertActiveFreshSettled(processObserver); err != nil {
			return fmt.Errorf("final process observation freshness immediately before CRASH B: %w", err)
		}
		return nil
	}, func() error {
		if processObserver == nil {
			return errors.New("verify crash-window process observer: observer was not prepared")
		}
		policyBoundary, err := h.CollectPolicyDeleteBoundaryEvidence(ctx, crashBRep)
		if err != nil {
			return fmt.Errorf("refresh policy at CRASH B delete boundary: %w", err)
		}
		ev.CrashBSafety.PolicyDeleteBoundary = policyBoundary
		authorization, deleteAt, err := authorizeActiveDeleteSettled(processObserver)
		if err != nil {
			return fmt.Errorf("authorize CRASH B from completed process sample: %w", err)
		}
		if err := killtest.ValidateDeleteBoundaryFreshness(deleteAt, ev.CrashBSafety.Target, ev.CrashBFluxProvenance); err != nil {
			return fmt.Errorf("CRASH B delete-boundary freshness: %w", err)
		}
		if err := killtest.ValidatePolicyDeleteBoundaryFreshness(
			deleteAt, crashBRep, ev.CrashBSafety.PolicyDeleteBoundary,
		); err != nil {
			return fmt.Errorf("CRASH B policy delete-boundary freshness: %w", err)
		}
		ev.CrashBAt = deleteAt
		ev.CrashBSafety.DeleteRequestedAt = deleteAt
		ev.CrashBProcessAuthorization = authorization
		if err := observer.AssertHealthy(); err != nil {
			return fmt.Errorf("final continuous spawn pod observation before CRASH B DELETE: %w", err)
		}
		return nil
	}, func(acceptedAt time.Time, renewed killtest.CrashLease) error {
		ev.CrashBSafety.LeaseRenewed = renewed.Evidence()
		ev.CrashBSafety.DeleteAcceptedAt = acceptedAt
		processErr := processObserver.Record(&ev)
		return errors.Join(processErr, checkpoint())
	})
	ev.CrashBSafety.LeaseRenewed = renewedLeaseB.Evidence()
	if err := releaseCrashLeaseAfter(h, leaseB.Token, crashErr); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH B: %w", err)
	}
	ev.CrashBReplacement = replacement
	if err := processObserver.Record(&ev); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("CRASH B process observation: %w", err)
	}
	if err := checkpoint(); err != nil {
		err = stopProcessObserverAfter(processObserver, &ev, err)
		return runOutput{Preflight: rep, Evidence: ev}, fmt.Errorf("write CRASH B checkpoint: %w", err)
	}

	terminalErr := h.AwaitTerminalWithProcessObserver(ctx, runID, spawnID, &ev, processObserver)
	if terminalErr != nil {
		// Keep going: a timeout still produces evidence + verdicts (which
		// will FAIL honestly).
		fmt.Fprintf(os.Stderr, "await terminal: %v (evaluating anyway)\n", terminalErr)
	}
	if mergingMode {
		ev.MergingCanary = true
		if err := h.CollectCanaryMergeEvidence(ctx, runID, &ev); err != nil {
			// Fail-closed: absent merge evidence makes PASS-3 FAIL honestly.
			fmt.Fprintf(os.Stderr, "collect canary merge evidence: %v\n", err)
		}
	}
	dedupe, dedupeErr := h.CollectDedupeEvidence(ctx, spawnID, ev.CrashAAt, ev.CrashAReplacement, ev.CrashBAt, ev.CrashBReplacement)
	if dedupeErr != nil {
		fmt.Fprintf(os.Stderr, "collect dedupe evidence: %v\n", dedupeErr)
	} else {
		ev.DedupeLog = dedupe
		ev.DedupeEvidence = dedupe.Line
	}
	finalObservationErr := h.CaptureSpawnState(ctx, &ev)
	if finalObservationErr != nil {
		appendMessage := "final all-record spawn observation: " + finalObservationErr.Error()
		ev.ObservationErrors = append(ev.ObservationErrors, appendMessage)
		fmt.Fprintln(os.Stderr, appendMessage)
	}
	finalPreflight, finalIdentityErr := awaitFinalGateIdentity(ctx, h, rep, 2*time.Minute)
	ev.FinalPreflight = finalPreflight
	if finalIdentityErr != nil {
		fmt.Fprintf(os.Stderr, "final gate identity: %v\n", finalIdentityErr)
	}
	if afterErr := h.CaptureSpawnState(ctx, &ev); afterErr != nil {
		message := "post-preflight all-record spawn observation: " + afterErr.Error()
		ev.ObservationErrors = append(ev.ObservationErrors, message)
		fmt.Fprintln(os.Stderr, message)
		finalObservationErr = errors.Join(finalObservationErr, afterErr)
	}
	watchErr := observer.Stop(&ev)
	observerStopped = true
	if watchErr != nil {
		fmt.Fprintf(os.Stderr, "continuous spawn pod observation: %v (proof marked incomplete)\n", watchErr)
	}

	v := killtest.Evaluate(ev)
	out := runOutput{Preflight: rep, FinalPreflight: &finalPreflight, Evidence: ev, Verdicts: v}
	fmt.Printf("== verdicts ==\n%s\n", mustJSON(v))
	if terminalErr != nil {
		return out, terminalErr
	}
	if watchErr != nil {
		return out, watchErr
	}
	if finalObservationErr != nil {
		return out, finalObservationErr
	}
	if finalIdentityErr != nil {
		return out, finalIdentityErr
	}
	if dedupeErr != nil {
		return out, dedupeErr
	}
	if !v.Overall {
		return out, fmt.Errorf("S1c kill-test FAILED — see %s", evidencePath)
	}
	return out, nil
}

func awaitFinalGateIdentity(
	ctx context.Context,
	h *killtest.Harness,
	want killtest.PreflightReport,
	timeout time.Duration,
) (killtest.PreflightReport, error) {
	deadline := time.Now().Add(timeout)
	var last killtest.PreflightReport
	var lastErr error
	for {
		got, err := h.Preflight(ctx)
		last = got
		if err == nil {
			if identityErr := sameGateIdentity(want, got); identityErr != nil {
				return got, fmt.Errorf("final immutable identity drift: %w", identityErr)
			}
			if got.AllPreconditions {
				return got, nil
			}
			lastErr = errors.New("final fleet did not drain to zero-work preconditions")
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("final identity/quiescence was not proven within %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func preflightForGate(
	ctx context.Context,
	h *killtest.Harness,
	allowedRunID string,
	baselineRevision, loomCoreBaselineRevision string,
) (killtest.PreflightReport, error) {
	probe := func(probeCtx context.Context) (killtest.PreflightReport, error) {
		return h.Preflight(probeCtx, allowedRunID)
	}
	var verifyTargetRunning func(context.Context) error
	if allowedRunID != "" {
		verifyTargetRunning = func(probeCtx context.Context) error {
			detail, err := h.GetRun(probeCtx, allowedRunID)
			if err != nil {
				return fmt.Errorf("verify allowed workflow run %s while Flux is reconciling: %w", allowedRunID, err)
			}
			if detail.Run.State != "running" {
				return fmt.Errorf("allowed workflow run %s state %q while Flux is reconciling, want running", allowedRunID, detail.Run.State)
			}
			return nil
		}
	}
	return retryGatePreflight(ctx, probe, verifyTargetRunning, baselineRevision, loomCoreBaselineRevision,
		gatePreflightTimeout, gatePreflightPoll)
}

func retryGatePreflight(
	ctx context.Context,
	probe func(context.Context) (killtest.PreflightReport, error),
	verifyTargetRunning func(context.Context) error,
	baselineRevision, loomCoreBaselineRevision string,
	timeout time.Duration,
	pollInterval time.Duration,
) (killtest.PreflightReport, error) {
	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last killtest.PreflightReport
	for {
		rep, err := probe(retryCtx)
		last = rep
		if retryCtx.Err() != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, fmt.Errorf("preflight did not complete within %s: %w", timeout, retryCtx.Err())
		}
		if err == nil {
			if !rep.AllPreconditions {
				return rep, errors.New("preflight preconditions not met (identity, policy, Loki, or active work)")
			}
			return rep, nil
		}
		if !retryableFluxConvergence(rep, err, baselineRevision, loomCoreBaselineRevision) {
			return rep, err
		}
		if verifyTargetRunning != nil {
			if targetErr := verifyTargetRunning(retryCtx); targetErr != nil {
				if retryCtx.Err() != nil {
					if ctx.Err() != nil {
						return last, ctx.Err()
					}
					return last, fmt.Errorf("preflight did not complete within %s: %w", timeout, retryCtx.Err())
				}
				return rep, targetErr
			}
		}
		source, revision := retryingFluxSource(rep, err)
		fmt.Fprintf(os.Stderr, "Flux %s is scanning accepted identity at revision %s; waiting for Ready before continuing\n", source, revision)
		timer := time.NewTimer(pollInterval)
		select {
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, fmt.Errorf("flux %s did not become Ready within %s at accepted revision %q: %w",
				source, timeout, revision, retryCtx.Err())
		case <-timer.C:
		}
	}
}

func retryableFluxConvergence(
	rep killtest.PreflightReport,
	err error,
	baselineRevision, loomCoreBaselineRevision string,
) bool {
	if err == nil {
		return false
	}
	switch {
	case strings.Contains(err.Error(), "flux apps is not converged"):
		return retryableFluxSource(rep.GitOpsRevision, rep.GitOpsAttempted,
			rep.GitOpsIdentity, baselineRevision)
	case strings.Contains(err.Error(), "flux bootstrap is not converged"):
		return retryableFluxSource(rep.GitOpsBootstrapRevision, rep.GitOpsBootstrapAttempted,
			rep.GitOpsBootstrapIdentity, baselineRevision)
	case strings.Contains(err.Error(), "flux system is not converged"):
		return retryableFluxSource(rep.GitOpsSystemRevision, rep.GitOpsSystemAttempted,
			rep.GitOpsSystemIdentity, baselineRevision)
	case strings.Contains(err.Error(), "flux loom-hub-servers is not converged"):
		return retryableFluxSource(rep.LoomCoreRevision, rep.LoomCoreAttempted,
			rep.LoomCoreIdentity, loomCoreBaselineRevision)
	default:
		return false
	}
}

func retryableFluxSource(
	revision, attempted string,
	identity killtest.GitOpsScopeIdentity,
	baselineRevision string,
) bool {
	if revision == "" || revision != attempted {
		return false
	}
	if baselineRevision == "" || revision == baselineRevision {
		return true
	}
	return identity.Mode == killtest.GitOpsIdentityModeProtectedScope &&
		identity.BaselineDigest != "" && identity.BaselineDigest == identity.ObservedDigest
}

func retryingFluxSource(rep killtest.PreflightReport, err error) (string, string) {
	if err != nil && strings.Contains(err.Error(), "flux loom-hub-servers is not converged") {
		return "loom-hub-servers", rep.LoomCoreRevision
	}
	if err != nil && strings.Contains(err.Error(), "flux bootstrap is not converged") {
		return "bootstrap", rep.GitOpsBootstrapRevision
	}
	if err != nil && strings.Contains(err.Error(), "flux system is not converged") {
		return "system", rep.GitOpsSystemRevision
	}
	return "apps", rep.GitOpsRevision
}

func runEvidencePath(base string, index, total int, gate bool) string {
	// Recovery attach (-run-id) writes one evidence file and no summary, so
	// the base path is the run's. A gate ALWAYS writes the summary at the base
	// path, so gate run evidence must carry the -run-NN suffix even for the
	// one-run merging contract — otherwise the run output and the summary
	// clobber each other.
	if !gate && total == 1 {
		return base
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	extension := filepath.Ext(base)
	return fmt.Sprintf("%s-run-%02d%s", stem, index, extension)
}

func releaseCrashLease(h *killtest.Harness, token string) error {
	releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.ReleaseCrashLease(releaseCtx, token)
}

func releaseCrashLeaseAfter(h *killtest.Harness, token string, operationErr error) error {
	return joinCrashLeaseErrors(operationErr, releaseCrashLease(h, token))
}

func joinCrashLeaseErrors(operationErr, releaseErr error) error {
	if releaseErr != nil {
		releaseErr = fmt.Errorf("release crash lease: %w", releaseErr)
	}
	return errors.Join(operationErr, releaseErr)
}

func joinHarnessCleanupErrors(operationErr, cleanupErr error) error {
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("remove private frozen S1c kubeconfig: %w", cleanupErr)
	}
	return errors.Join(operationErr, cleanupErr)
}

func stopProcessObserverAfter(observer *killtest.CanaryProcessObserver, ev *killtest.Evidence, operationErr error) error {
	if observer == nil {
		return operationErr
	}
	return errors.Join(operationErr, observer.StopAndRecord(ev))
}

func validateCanaryHoldRecheck(initial, current killtest.CanaryHoldObservation, ready bool) error {
	if !ready {
		return errors.New("exact canary hold process is no longer running")
	}
	if initial.PID <= 1 || initial.StartTimeTicks == 0 || initial.DriverPID <= 1 ||
		initial.DriverStartTimeTicks == 0 || initial.Seconds <= 0 || initial.PodName == "" {
		return fmt.Errorf("initial canary hold proof is incomplete: %+v", initial)
	}
	if current.PID != initial.PID || current.StartTimeTicks != initial.StartTimeTicks ||
		current.DriverPID != initial.DriverPID || current.DriverStartTimeTicks != initial.DriverStartTimeTicks ||
		current.Seconds != initial.Seconds || current.PodName != initial.PodName {
		return fmt.Errorf("exact canary hold identity changed: %+v -> %+v", initial, current)
	}
	return nil
}

func sameGateIdentity(want, got killtest.PreflightReport) error {
	if err := killtest.ValidateConfigMapGateIdentity(want, got); err != nil {
		return fmt.Errorf("ConfigMap gate identity changed: %w", err)
	}
	if want.FluxSourcesEnd.Contract != "" || got.FluxSourcesEnd.Contract != "" {
		if err := killtest.ValidateGateIdentityContinuity(want, got); err != nil {
			return fmt.Errorf("serialized gate identity changed: %w", err)
		}
	}
	if err := sameGateSourceIdentity("platform GitOps", want.GitOpsRevision, got.GitOpsRevision,
		want.GitOpsIdentity, got.GitOpsIdentity); err != nil {
		return err
	}
	if err := sameGateSourceIdentity("platform GitOps bootstrap", want.GitOpsBootstrapRevision, got.GitOpsBootstrapRevision,
		want.GitOpsBootstrapIdentity, got.GitOpsBootstrapIdentity); err != nil {
		return err
	}
	if err := sameGateSourceIdentity("platform GitOps system", want.GitOpsSystemRevision, got.GitOpsSystemRevision,
		want.GitOpsSystemIdentity, got.GitOpsSystemIdentity); err != nil {
		return err
	}
	if err := sameGateSourceIdentity("loom-core", want.LoomCoreRevision, got.LoomCoreRevision,
		want.LoomCoreIdentity, got.LoomCoreIdentity); err != nil {
		return err
	}
	if got.OperatorImage != want.OperatorImage || got.Operator.ImageID != want.Operator.ImageID ||
		got.OperatorDeployment.Generation != want.OperatorDeployment.Generation ||
		got.HudImage != want.HudImage || got.Hud.ImageID != want.Hud.ImageID ||
		got.HudDeployment.Generation != want.HudDeployment.Generation ||
		got.PolicyChecksum != want.PolicyChecksum ||
		got.SpawnConfigMapUID != want.SpawnConfigMapUID || !got.SpawnConfigMapUpdateAllowed ||
		got.ConfigMapPolicyEnabled != want.ConfigMapPolicyEnabled || got.FlagEnabled != want.FlagEnabled ||
		got.SubstrateK8sOnly != want.SubstrateK8sOnly || !got.EffectivePolicyMatchesConfigMap {
		return fmt.Errorf("immutable workload identity changed: operator tag/digest/generation %q/%q/%d -> %q/%q/%d, hud %q/%q/%d -> %q/%q/%d, policy %q -> %q, spawn ConfigMap UID %q -> %q",
			want.OperatorImage, want.Operator.ImageID, want.OperatorDeployment.Generation,
			got.OperatorImage, got.Operator.ImageID, got.OperatorDeployment.Generation,
			want.HudImage, want.Hud.ImageID, want.HudDeployment.Generation,
			got.HudImage, got.Hud.ImageID, got.HudDeployment.Generation,
			want.PolicyChecksum, got.PolicyChecksum,
			want.SpawnConfigMapUID, got.SpawnConfigMapUID)
	}
	return nil
}

func sameGateSourceIdentity(
	name, wantRevision, gotRevision string,
	want, got killtest.GitOpsScopeIdentity,
) error {
	matches := want.Mode != "" && want.Contract != "" && want.ContractVersion > 0 &&
		want.BaselineRevision != "" && want.BaselineDigest != "" && want.ObservedDigest != "" &&
		got.Mode == want.Mode && got.Contract == want.Contract &&
		got.ContractVersion == want.ContractVersion &&
		got.BaselineRevision == want.BaselineRevision &&
		got.BaselineDigest == want.BaselineDigest && got.ObservedDigest == want.ObservedDigest
	if !matches {
		return fmt.Errorf("immutable %s source identity changed: contract/mode/baseline/digest %q-v%d/%q/%q/%q -> %q-v%d/%q/%q/%q (observed revisions %q -> %q)",
			name,
			want.Contract, want.ContractVersion, want.Mode, want.BaselineRevision, want.ObservedDigest,
			got.Contract, got.ContractVersion, got.Mode, got.BaselineRevision, got.ObservedDigest,
			wantRevision, gotRevision)
	}
	return nil
}

func writeJSON(path string, value any) error {
	blob, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mills-s1c-evidence-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Persist the rename as well as the file contents. This makes delete intent
	// and accepted-delete receipts survive a harness process crash cleanly.
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func evidenceFileSHA256(path string) (string, error) {
	var output runOutput
	return readStrictRegularJSONWithSHA256(path, &output)
}

func validateEvidenceDestination(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("evidence path is empty")
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("parent %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("parent %s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".mills-s1c-write-probe-*")
	if err != nil {
		return fmt.Errorf("parent %s is not writable: %w", dir, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func writeSummary(path string, total int, summary gateSummary) error {
	if total == 1 {
		return nil
	}
	return writeJSON(path, summary)
}

func wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
