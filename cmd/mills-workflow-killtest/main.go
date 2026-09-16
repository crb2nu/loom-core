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
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/crb2nu/loom/pkg/agentcontext"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow"
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
		operatorURL        = flag.String("operator-url", "http://localhost:8090", "restart-stable operator REST base URL")
		mode               = flag.String("mode", "", "kill-test mode: queued-proof")
		adminToken         = flag.String("admin-token", os.Getenv("LOOM_MILLS_ADMIN_TOKEN"), "admin bearer token (default $LOOM_MILLS_ADMIN_TOKEN)")
		expectedGitOps     = flag.String("expected-gitops-revision", os.Getenv("S1C_EXPECTED_GITOPS_REVISION"), "reviewed remote GitOps main SHA (required for a full gate)")
		expectedLoomCore   = flag.String("expected-loom-core-revision", os.Getenv("S1C_EXPECTED_LOOM_CORE_REVISION"), "reviewed remote loom-core main SHA (required for a full gate)")
		gitOpsIdentity     = flag.String("gitops-identity-mode", envOrDefault("S1C_GITOPS_IDENTITY_MODE", killtest.GitOpsIdentityModeExactRevision), "GitOps identity contract: exact-revision | protected-scope")
		gitOpsRepo         = flag.String("gitops-repo", os.Getenv("S1C_GITOPS_REPO"), "local platform/gitops repository used for protected identity and reviewed Flux spec binding")
		loomCoreRepo       = flag.String("loom-core-repo", os.Getenv("S1C_LOOM_CORE_REPO"), "local loom-core repository used for protected identity and exact reviewed Deployment rendering")
		fluxBin            = flag.String("flux-bin", envOrDefault("S1C_FLUX_BIN", "flux"), "Flux CLI used for exact reviewed Deployment rendering")
		hudURL             = flag.String("hud-url", envOrDefault("S1C_HUD_URL", "https://hud.flexinfer.ai"), "restart-stable mobile-hud base URL used for exact spawn cleanup")
		hudAdminToken      = flag.String("hud-admin-token", os.Getenv("HUD_ADMIN_TOKEN"), "mobile-hud admin token used for exact spawn cleanup (default $HUD_ADMIN_TOKEN)")
		phase              = flag.String("phase", "full", "phase to run: preflight | full | verify")
		attachRunID        = flag.String("run-id", "", "attach to an existing running imperative run instead of launching a fresh canary")
		agentType          = flag.String("agent-type", killtest.AgentTypeClaudeCode, "canary spawn agent: claude-code | codex")
		evidence           = flag.String("evidence", "s1c-evidence.json", "path to write the evidence JSON")
		runs               = flag.Int("runs", 3, "number of consecutive full dual-crash runs (the S1c gate requires exactly 3)")
		stepTimeout        = flag.Duration("step-timeout", 5*time.Minute, "max wait for the pending spawn step")
		termTimeout        = flag.Duration("terminal-timeout", 30*time.Minute, "max wait for the run to reach a terminal state")
		crashDelay         = flag.Duration("crash-delay", 15*time.Second, "wait after the spawn is confirmed before CRASH A, and between CRASH A and CRASH B")
		merging            = flag.Bool("merging", false, "S6-full merging canary: template v3 with a journaled merge('canary') effect; PASS-3 evaluated from real GitLab evidence")
		gitlabAPIURL       = flag.String("gitlab-api-url", envOrDefault("S1C_GITLAB_API_URL", "https://gitlab.flexinfer.ai/api/v4"), "GitLab API base for PASS-3 merge verification (merging mode)")
		gitlabToken        = flag.String("gitlab-token", os.Getenv("GITLAB_TOKEN"), "GitLab token for PASS-3 merge verification (default $GITLAB_TOKEN)")
		gitlabProject      = flag.String("gitlab-project", envOrDefault("S1C_GITLAB_PROJECT", "services/loom-core"), "GitLab project the merging canary merges into")
		scenario           = flag.String("scenario", "", "deterministic scenario: queued-proof | mr-awareness")
		scenarioMaxAge     = flag.Duration("scenario-max-age", 5*time.Minute, "maximum age of scenario evidence")
		queuedProofID      = flag.String("queued-proof-backlog-id", "", "existing queued backlog item to drive through live terminal MR proof")
		queuedProofPlan    = flag.String("queued-proof-plan-id", "", "canonical Pattern Loom plan id to seed for the live queued-proof")
		queuedProofTarget  = flag.String("queued-proof-target-project", "", "declared target project for the queued proof (required for live runs)")
		queuedProofResume  = flag.Bool("queued-proof-resume", false, "resume the admitted run recorded in --evidence instead of starting another run")
		queuedProofPoll    = flag.Duration("queued-proof-poll", 5*time.Second, "live queued-proof polling interval")
		embedHealthURL     = flag.String("queued-proof-embed-health-url", os.Getenv("AGENT_CONTEXT_EMBED_HEALTH_URL"), "embedder health snapshot URL required for live queued-proof admission")
		embedHealthWindow  = flag.Duration("queued-proof-embed-health-window", 5*time.Minute, "maximum age of the embedder health snapshot and its trailing fail-closed window")
		embedHealthTimeout = flag.Duration("queued-proof-embed-health-timeout", 5*time.Second, "timeout for the live queued-proof embedder health precondition")
		queuedProofWorker  = flag.String("queued-proof-worker-command", "", "command that starts the isolated operator worker (required by --mode queued-proof)")
		queuedProofState   = flag.String("queued-proof-state-dir", "", "empty isolated state directory used across the worker restart (required by --mode queued-proof)")
		queuedProofStamp   = flag.String("queued-proof-stamp-id", "", "target-project stamp identity (default: generated; deterministic in --dry-run)")
		queuedProofWait    = flag.Duration("queued-proof-worker-timeout", 30*time.Second, "maximum wait for each queued-proof worker start or stop")
		queuedProofDryRun  = flag.Bool("dry-run", false, "emit the queued-proof plan without starting or killing a worker")
		mrWorker           = flag.String("mr-awareness-worker-command", "", "command that starts the isolated operator worker")
		mrState            = flag.String("mr-awareness-state-dir", "", "isolated state directory preserved across restart")
		mrBacklog          = flag.String("mr-awareness-backlog-id", "", "queued backlog item whose run creates the test MR")
		mrSourceBranch     = flag.String("mr-awareness-source-branch", "", "expected GitLab source branch (recorded on first execution)")
		mrWait             = flag.Duration("mr-awareness-timeout", 20*time.Minute, "maximum wait for MR creation and worker restart")
		mrPoll             = flag.Duration("mr-awareness-poll", 2*time.Second, "poll interval for durable run and GitLab evidence")
		mrRecoveryMaxAge   = flag.Duration("mr-awareness-recovery-max-age", 24*time.Hour, "maximum age of existing recovery evidence")
	)
	flag.Parse()
	if *mode != "" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		switch *mode {
		case string(pipeline.KilltestQueuedProof):
			return runQueuedAdmissionKilltest(ctx, os.Stdout, queuedAdmissionOptions{
				OperatorURL: *operatorURL, AdminToken: *adminToken, WorkerCommand: *queuedProofWorker,
				StateDir: *queuedProofState, EvidencePath: *evidence, Timeout: *queuedProofWait, DryRun: *queuedProofDryRun,
				TargetProject: *queuedProofTarget, StampID: *queuedProofStamp,
			})
		case string(pipeline.KilltestMRAwareness):
			return runMRAwarenessKilltest(ctx, os.Stdout, mrAwarenessOptions{
				OperatorURL: *operatorURL, AdminToken: *adminToken, WorkerCommand: *mrWorker,
				StateDir: *mrState, EvidencePath: *evidence, BacklogID: *mrBacklog,
				SourceBranch: *mrSourceBranch, GitLabURL: *gitlabAPIURL, GitLabToken: *gitlabToken,
				GitLabProject: *gitlabProject, Timeout: *mrWait, Poll: *mrPoll, RecoveryMaxAge: *mrRecoveryMaxAge,
			})
		default:
			return emitQueuedAdmissionFailure(os.Stdout, *evidence, "invalid_mode", fmt.Errorf("invalid --mode %q (want queued-proof or mr-awareness)", *mode))
		}
	}
	if *scenario != "" {
		if *scenario == string(pipeline.KilltestQueuedProof) && (*queuedProofID != "" || *queuedProofPlan != "") {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if err := runQueuedProofWithEmbedPrecondition(ctx, &http.Client{Timeout: *embedHealthTimeout}, *embedHealthURL, *embedHealthWindow, time.Now().UTC(), func() error {
				return runLiveQueuedProof(ctx, queuedProofDriver{operatorURL: *operatorURL, adminToken: *adminToken, gitlabURL: *gitlabAPIURL, gitlabToken: *gitlabToken, gitlabProject: *queuedProofTarget, client: &http.Client{Timeout: 30 * time.Second}, poll: *queuedProofPoll, resume: *queuedProofResume}, *queuedProofID, *queuedProofPlan, *evidence, *termTimeout)
			}); err != nil {
				return err
			}
			return verifyLiveQueuedProof(*evidence, *queuedProofTarget)
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

func requireHealthyEmbedder(ctx context.Context, client *http.Client, endpoint string, trailingWindow time.Duration, now time.Time) error {
	if _, err := agentcontext.ProbeEmbedHealth(ctx, client, endpoint, now, trailingWindow); err != nil {
		return fmt.Errorf("embedder_unhealthy: %w", err)
	}
	return nil
}

func runQueuedProofWithEmbedPrecondition(ctx context.Context, client *http.Client, endpoint string, trailingWindow time.Duration, now time.Time, runQueuedProof func() error) error {
	if err := requireHealthyEmbedder(ctx, client, endpoint, trailingWindow, now); err != nil {
		return err
	}
	return runQueuedProof()
}

// verifyLiveQueuedProof is the final fail-closed boundary for the live path.
// The driver persists external-dependency diagnostics for operators, but a
// diagnostic is not proof that the queued workflow completed and auto-merged.
func verifyLiveQueuedProof(evidencePath, declaredTarget string) error {
	f, err := os.Open(evidencePath)
	if err != nil {
		return fmt.Errorf("open live queued-proof evidence: %w", err)
	}
	defer f.Close()

	var report queuedProofReport
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return fmt.Errorf("decode live queued-proof evidence: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("decode live queued-proof evidence: trailing JSON data")
	}
	if report.Verdict != queuedProofVerdictPass {
		return fmt.Errorf("live queued-proof verdict is %q, want %q", report.Verdict, queuedProofVerdictPass)
	}
	if strings.TrimSpace(declaredTarget) == "" || report.DeclaredTarget != declaredTarget || report.MR.Project != declaredTarget {
		return fmt.Errorf("live queued-proof target identity is contradictory: declared=%q report=%q MR=%q", declaredTarget, report.DeclaredTarget, report.MR.Project)
	}
	if err := workflow.AssertQueuedProof(report.QueuedProofEvidence); err != nil {
		return fmt.Errorf("verify live queued-proof evidence: %w", err)
	}
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

type queuedAdmissionOptions struct {
	OperatorURL, AdminToken, WorkerCommand, StateDir, EvidencePath string
	TargetProject, StampID                                         string
	Timeout                                                        time.Duration
	DryRun                                                         bool
}

type queuedAdmissionSnapshot struct {
	ID            string `json:"ID"`
	State         string `json:"State"`
	ClaimVersion  int64  `json:"ClaimVersion"`
	TargetProject string `json:"TargetProject"`
}

type queuedAdmissionRun struct {
	ID        string `json:"ID"`
	BacklogID string `json:"BacklogID"`
}

type queuedAdmissionVerdict struct {
	Mode                  string                          `json:"mode"`
	Verdict               string                          `json:"verdict"`
	Passed                bool                            `json:"passed"`
	ReasonCode            string                          `json:"reason_code,omitempty"`
	Detail                string                          `json:"detail,omitempty"`
	Stamp                 string                          `json:"stamp,omitempty"`
	TargetID              string                          `json:"target_id,omitempty"`
	TargetAdmissions      int                             `json:"target_admissions"`
	CollateralTransitions []string                        `json:"collateral_transitions"`
	Before                []queuedAdmissionSnapshot       `json:"before,omitempty"`
	After                 []queuedAdmissionSnapshot       `json:"after,omitempty"`
	WorkerRestarts        int                             `json:"worker_restarts"`
	DryRun                bool                            `json:"dry_run,omitempty"`
	Proof                 workflow.QueuedRequeueProof     `json:"proof"`
	TargetProof           workflow.QueuedTargetStampProof `json:"target_proof"`
}

// runQueuedAdmissionKilltest owns an isolated worker process. It deliberately
// sends admission asynchronously and terminates the worker at that boundary;
// the assertion is valid whether the transaction commits immediately before
// or after termination because restart recovery must still expose one run.
func runQueuedAdmissionKilltest(ctx context.Context, out io.Writer, o queuedAdmissionOptions) error {
	fail := func(code string, err error) error { return emitQueuedAdmissionFailure(out, o.EvidencePath, code, err) }
	o.TargetProject = strings.TrimSpace(o.TargetProject)
	o.StampID = strings.TrimSpace(o.StampID)
	if o.DryRun {
		if o.TargetProject == "" {
			// No declared target: plan the requeue proof only (offline, deterministic).
			proof, err := workflow.PlanQueuedRequeueProof("queued-proof-target")
			if err != nil {
				return fail("dry_run_failed", err)
			}
			return writeQueuedAdmissionVerdict(out, o.EvidencePath, queuedAdmissionVerdict{Mode: "queued-proof", Verdict: "PLANNED", DryRun: true, Stamp: "dry-run", TargetID: proof.ItemID, Proof: proof}, nil)
		}
		if o.StampID == "" {
			o.StampID = "queued-proof-dry-run"
		}
		proof := workflow.QueuedTargetStampProof{StampID: o.StampID, TargetProject: o.TargetProject, LandedTargetProject: o.TargetProject, QueueStates: []string{"queued", "admitted"}, Admissions: 1, CollisionDetected: true}
		if err := workflow.AssertQueuedTargetStampProof(proof); err != nil {
			return fail("malformed_evidence", err)
		}
		return writeQueuedAdmissionVerdict(out, o.EvidencePath, queuedAdmissionVerdict{Mode: "queued-proof", Verdict: "PASS", Passed: true, DryRun: true, Stamp: o.StampID, TargetID: o.StampID + "-target", TargetAdmissions: 1, TargetProof: proof}, nil)
	}
	if o.TargetProject == "" {
		return fail("invalid_configuration", errors.New("--mode queued-proof requires --queued-proof-target-project"))
	}
	if strings.TrimSpace(o.WorkerCommand) == "" || strings.TrimSpace(o.StateDir) == "" || strings.TrimSpace(o.AdminToken) == "" {
		return fail("invalid_configuration", errors.New("--mode queued-proof requires --queued-proof-worker-command, --queued-proof-state-dir, and --admin-token"))
	}
	if o.Timeout <= 0 {
		return fail("invalid_configuration", errors.New("--queued-proof-worker-timeout must be positive"))
	}
	stateDir, err := filepath.Abs(o.StateDir)
	if err != nil {
		return fail("invalid_configuration", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("invalid_configuration", fmt.Errorf("read state directory: %w", err))
	}
	if err == nil && len(entries) != 0 {
		return fail("unsafe_state_directory", fmt.Errorf("queued-proof state directory %s is not empty", stateDir))
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fail("invalid_configuration", fmt.Errorf("create state directory: %w", err))
	}

	startWorker := func() (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", o.WorkerCommand)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "LOOM_MILLS_DB_PATH="+filepath.Join(stateDir, "mills.db"))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if err := waitOperator(ctx, o.OperatorURL, o.Timeout); err != nil {
			_ = stopQueuedWorker(cmd, o.Timeout)
			return nil, err
		}
		return cmd, nil
	}
	worker, err := startWorker()
	if err != nil {
		return fail("worker_start_failed", err)
	}
	defer func() { _ = stopQueuedWorker(worker, o.Timeout) }()

	stamp := o.StampID
	if stamp == "" {
		stamp = fmt.Sprintf("queued-proof-%d", time.Now().UTC().UnixNano())
	}
	stampStore, err := store.Open(ctx, store.Options{Path: filepath.Join(stateDir, "mills.db")})
	if err != nil {
		return fail("stamp_store_failed", err)
	}
	stampRecord := &store.Stamp{ID: stamp, TargetProject: o.TargetProject}
	if err := stampStore.Stamps.Put(ctx, stampRecord); err != nil {
		_ = stampStore.Close()
		return fail("stamp_collision", err)
	}
	collisionErr := stampStore.Stamps.Put(ctx, &store.Stamp{ID: stamp, TargetProject: o.TargetProject})
	persistedStamp, getStampErr := stampStore.Stamps.Get(ctx, o.TargetProject, stamp)
	if err := stampStore.Close(); err != nil {
		return fail("stamp_store_failed", err)
	}
	if collisionErr == nil {
		return fail("stamp_collision_missing", errors.New("duplicate target-project stamp was accepted"))
	}
	if getStampErr != nil {
		return fail("stamp_evidence_unavailable", fmt.Errorf("read original stamp after collision: %w", getStampErr))
	}
	if persistedStamp.ID != stamp || persistedStamp.TargetProject != o.TargetProject {
		return fail("stamp_collision_overwrite", fmt.Errorf("duplicate write changed stamp tuple: got (%q, %q), want (%q, %q)", persistedStamp.TargetProject, persistedStamp.ID, o.TargetProject, stamp))
	}
	ids := []string{stamp + "-target", stamp + "-control-a", stamp + "-control-b"}
	client := &http.Client{Timeout: o.Timeout}
	for _, id := range ids {
		seed := map[string]any{"ID": id, "Title": "queued-proof stamped item " + id, "State": "queued", "Labels": []string{"queued-proof-killtest", stamp}, "CreatedBy": "mills-workflow-killtest", "TargetProject": o.TargetProject}
		if err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodPost, "/api/mills/backlog", seed, nil, http.StatusCreated); err != nil {
			return fail("seed_failed", err)
		}
	}
	before, err := queuedAdmissionItems(ctx, client, o, ids)
	if err != nil {
		return fail("snapshot_failed", err)
	}
	for _, item := range before {
		if item.State != "queued" || item.ClaimVersion != 0 || item.TargetProject != o.TargetProject {
			return fail("malformed_evidence", fmt.Errorf("seeded item %s was not pristine queued evidence: state=%q claim_version=%d", item.ID, item.State, item.ClaimVersion))
		}
	}

	admissionWritten := make(chan struct{}, 1)
	releaseAdmission := make(chan struct{})
	admissionDone := make(chan error, 1)
	go func() {
		requestCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
			admissionWritten <- struct{}{}
			<-releaseAdmission
		}})
		admissionDone <- queuedAdmissionRequest(requestCtx, client, o.OperatorURL, o.AdminToken, http.MethodPost, "/api/mills/pipeline/runs/"+ids[0]+"/start", nil, nil, http.StatusCreated)
	}()
	select {
	case <-admissionWritten:
	case err := <-admissionDone:
		close(releaseAdmission)
		return fail("admission_not_dispatched", fmt.Errorf("admission request settled before dispatch boundary: %w", err))
	case <-time.After(o.Timeout):
		close(releaseAdmission)
		return fail("admission_timeout", errors.New("admission request was not written before timeout"))
	}
	if err := killQueuedWorker(worker, o.Timeout); err != nil {
		close(releaseAdmission)
		return fail("worker_kill_failed", err)
	}
	worker = nil
	close(releaseAdmission)
	// The request may fail with EOF because the kill landed before its response;
	// durable evidence after restart, not the transport result, decides the test.
	select {
	case <-admissionDone:
	case <-time.After(o.Timeout):
		return fail("admission_timeout", errors.New("admission request did not settle after worker termination"))
	}
	worker, err = startWorker()
	if err != nil {
		return fail("worker_restart_failed", err)
	}
	// Replay the same identity to exercise the restart dedupe boundary. A 409
	// is expected when the first transaction committed; 201 is permitted only
	// when it did not, and the run-count assertion below remains authoritative.
	if err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodPost, "/api/mills/pipeline/runs/"+ids[0]+"/start", nil, nil, http.StatusCreated, http.StatusConflict); err != nil {
		return fail("replay_failed", err)
	}

	after, err := queuedAdmissionItems(ctx, client, o, ids)
	if err != nil {
		return fail("snapshot_failed", err)
	}
	var runs []queuedAdmissionRun
	if err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodGet, "/api/mills/pipeline/runs?backlog_id="+ids[0], nil, &runs, http.StatusOK); err != nil {
		return fail("evidence_unavailable", err)
	}
	for _, run := range runs {
		if strings.TrimSpace(run.ID) == "" || run.BacklogID != ids[0] {
			return fail("malformed_evidence", fmt.Errorf("contradictory run evidence: id=%q backlog_id=%q", run.ID, run.BacklogID))
		}
	}
	collateral := changedCollateral(before, after, ids[0])
	events := []workflow.QueuedProofEvent{{ItemID: ids[0], Kind: workflow.QueuedProofSeeded}, {ItemID: ids[0], Kind: workflow.QueuedProofInterrupted}, {ItemID: ids[0], Kind: workflow.QueuedProofRequeued}}
	for _, run := range runs {
		events = append(events, workflow.QueuedProofEvent{ItemID: ids[0], Kind: workflow.QueuedProofExecuted, AttemptID: run.ID})
	}
	proof, proofErr := workflow.ProveQueuedRequeue(ids[0], events)
	landedTarget := ""
	for _, item := range after {
		if item.ID == ids[0] {
			landedTarget = item.TargetProject
		}
	}
	targetProof := workflow.QueuedTargetStampProof{StampID: stamp, TargetProject: o.TargetProject, LandedTargetProject: landedTarget, QueueStates: []string{"queued", "admitted"}, Admissions: len(runs), CollisionDetected: collisionErr != nil}
	report := queuedAdmissionVerdict{Mode: "queued-proof", Verdict: "PASS", Passed: true, Stamp: stamp, TargetID: ids[0], TargetAdmissions: len(runs), CollateralTransitions: collateral, Before: before, After: after, WorkerRestarts: 1, Proof: proof, TargetProof: targetProof}
	if proofErr != nil || len(runs) != 1 || len(collateral) != 0 {
		report.Verdict, report.Passed, report.ReasonCode = "FAIL", false, "invariant_violation"
		report.Detail = fmt.Sprintf("queued requeue proof=%v, target runs=%d (want 1), collateral transitions=%d (want 0)", proofErr, len(runs), len(collateral))
		return writeQueuedAdmissionVerdict(out, o.EvidencePath, report, errors.New(report.Detail))
	}
	if err := workflow.AssertQueuedTargetStampProof(targetProof); err != nil {
		report.Verdict, report.Passed, report.ReasonCode, report.Detail = "FAIL", false, "target_proof_failed", err.Error()
		return writeQueuedAdmissionVerdict(out, o.EvidencePath, report, err)
	}
	return writeQueuedAdmissionVerdict(out, o.EvidencePath, report, nil)
}

func waitOperator(ctx context.Context, base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		// The operator exposes health on its optional metrics listener, not the
		// REST listener supplied here. Any HTTP response proves the controlled
		// REST listener is accepting requests; later authenticated calls verify
		// the API itself.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("timed out waiting for operator REST listener")
}

func stopQueuedWorker(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err
			}
		}
		return nil
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return nil
	}
}

func killQueuedWorker(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return err
		}
		return nil
	case <-time.After(timeout):
		return errors.New("timed out waiting for killed worker to exit")
	}
}

func queuedAdmissionRequest(ctx context.Context, client *http.Client, base, token, method, path string, body, out any, allowed ...int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	accepted := false
	for _, status := range allowed {
		accepted = accepted || resp.StatusCode == status
	}
	if !accepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

func queuedAdmissionItems(ctx context.Context, client *http.Client, o queuedAdmissionOptions, ids []string) ([]queuedAdmissionSnapshot, error) {
	items := make([]queuedAdmissionSnapshot, 0, len(ids))
	for _, id := range ids {
		var item queuedAdmissionSnapshot
		if err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodGet, "/api/mills/backlog/"+id, nil, &item, http.StatusOK); err != nil {
			return nil, err
		}
		if item.ID != id {
			return nil, fmt.Errorf("contradictory backlog identity: got %q want %q", item.ID, id)
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func changedCollateral(before, after []queuedAdmissionSnapshot, target string) []string {
	b := make(map[string]queuedAdmissionSnapshot, len(before))
	for _, item := range before {
		b[item.ID] = item
	}
	var changed []string
	for _, item := range after {
		if item.ID == target {
			continue
		}
		prior, ok := b[item.ID]
		if !ok || prior.State != item.State || prior.ClaimVersion != item.ClaimVersion || prior.TargetProject != item.TargetProject {
			changed = append(changed, item.ID)
		}
	}
	sort.Strings(changed)
	return changed
}

func emitQueuedAdmissionFailure(out io.Writer, path, code string, err error) error {
	report := queuedAdmissionVerdict{Mode: "queued-proof", Verdict: "FAIL", ReasonCode: code}
	if err != nil {
		report.Detail = err.Error()
	}
	return writeQueuedAdmissionVerdict(out, path, report, err)
}

func writeQueuedAdmissionVerdict(out io.Writer, path string, report queuedAdmissionVerdict, cause error) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			return fmt.Errorf("write queued-proof evidence: %w", err)
		}
	}
	if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
		return err
	}
	return cause
}

type mrAwarenessOptions struct {
	OperatorURL, AdminToken, WorkerCommand, StateDir, EvidencePath string
	BacklogID, SourceBranch, GitLabURL, GitLabToken, GitLabProject string
	Timeout, Poll, RecoveryMaxAge                                  time.Duration
}

type mrAwarenessRun struct {
	ID           string `json:"ID"`
	BacklogID    string `json:"BacklogID"`
	State        string `json:"State"`
	CurrentStage string `json:"CurrentStage"`
	MRIID        *int64 `json:"MRIID"`
}

type mrAwarenessSummary struct {
	Mode              string    `json:"mode"`
	Verdict           string    `json:"verdict"`
	Passed            bool      `json:"passed"`
	ReasonCode        string    `json:"reason_code,omitempty"`
	Detail            string    `json:"detail,omitempty"`
	BacklogID         string    `json:"backlog_id,omitempty"`
	RunID             string    `json:"run_id,omitempty"`
	MRProject         string    `json:"mr_project,omitempty"`
	MRIID             int64     `json:"mr_iid,omitempty"`
	MRURL             string    `json:"mr_url,omitempty"`
	SourceBranch      string    `json:"source_branch,omitempty"`
	MRCount           int       `json:"mr_count"`
	StageBeforeKill   string    `json:"stage_before_kill,omitempty"`
	StageAfterRestart string    `json:"stage_after_restart,omitempty"`
	WorkerRestarts    int       `json:"worker_restarts"`
	Recovered         bool      `json:"recovered"`
	CapturedAt        time.Time `json:"captured_at"`
}

type gitLabMRIdentity struct {
	IID          int64  `json:"iid"`
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
}

// runMRAwarenessKilltest kills only after the MR identity is durable in the
// canonical run row. Its summary is also a recovery token: rerunning with the
// same state and summary adopts that run instead of admitting another one.
func runMRAwarenessKilltest(ctx context.Context, out io.Writer, o mrAwarenessOptions) error {
	report := mrAwarenessSummary{Mode: string(pipeline.KilltestMRAwareness), Verdict: "FAIL", BacklogID: o.BacklogID, MRProject: o.GitLabProject, SourceBranch: o.SourceBranch, CapturedAt: time.Now().UTC()}
	fail := func(code string, err error) error {
		report.ReasonCode = code
		if err != nil {
			report.Detail = err.Error()
		}
		return writeMRAwarenessSummary(out, o.EvidencePath, report, err)
	}
	if strings.TrimSpace(o.WorkerCommand) == "" || strings.TrimSpace(o.StateDir) == "" || strings.TrimSpace(o.BacklogID) == "" ||
		strings.TrimSpace(o.SourceBranch) == "" || strings.TrimSpace(o.AdminToken) == "" || strings.TrimSpace(o.GitLabToken) == "" || strings.TrimSpace(o.GitLabProject) == "" || o.Timeout <= 0 || o.Poll <= 0 || o.RecoveryMaxAge <= 0 {
		return fail("invalid_configuration", errors.New("mr-awareness mode requires worker command, state dir, backlog id, source branch, admin token, GitLab token/project, and positive timeout/poll"))
	}
	stateDir, err := filepath.Abs(o.StateDir)
	if err != nil {
		return fail("invalid_configuration", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fail("invalid_configuration", err)
	}

	// A valid prior summary is authoritative recovery evidence. Malformed or
	// contradictory evidence fails closed; it is never silently replaced.
	if data, readErr := os.ReadFile(o.EvidencePath); readErr == nil {
		prior, code, recoveryErr := validateMRAwarenessRecovery(data, o, time.Now().UTC())
		if prior.Mode != "" {
			report = prior
		}
		report.Verdict, report.Passed, report.ReasonCode, report.Detail = "FAIL", false, "", ""
		if recoveryErr != nil {
			return fail(code, recoveryErr)
		}
		report.Recovered = true
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fail("evidence_unavailable", readErr)
	}

	startWorker := func() (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", o.WorkerCommand)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "LOOM_MILLS_DB_PATH="+filepath.Join(stateDir, "mills.db"))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if err := waitOperator(ctx, o.OperatorURL, o.Timeout); err != nil {
			_ = stopQueuedWorker(cmd, o.Timeout)
			return nil, err
		}
		return cmd, nil
	}
	worker, err := startWorker()
	if err != nil {
		return fail("worker_start_failed", err)
	}
	defer func() { _ = stopQueuedWorker(worker, o.Timeout) }()
	client := &http.Client{Timeout: minDuration(o.Timeout, 30*time.Second)}
	if report.RunID == "" {
		var started queuedProofStartResponse
		if err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodPost, "/api/mills/pipeline/runs/"+urlPathEscape(o.BacklogID)+"/start", nil, &started, http.StatusCreated); err != nil {
			return fail("start_failed", err)
		}
		if started.RunID == "" || started.BacklogID != o.BacklogID {
			return fail("mismatched_identity", errors.New("start returned contradictory run identity"))
		}
		report.RunID = started.RunID
		if err := writeMRAwarenessSummary(io.Discard, o.EvidencePath, report, nil); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(o.Timeout)
	before, err := awaitMRAwarenessRun(ctx, client, o, report.RunID, deadline, true)
	if err != nil {
		return fail("mr_not_created", err)
	}
	if err := recoveredMRIdentityError(report, *before.MRIID); err != nil {
		return fail("mismatched_identity", err)
	}
	report.StageBeforeKill, report.MRIID = before.CurrentStage, *before.MRIID
	if !stageAtOrBeyondMR(before.CurrentStage) {
		return fail("stage_regression", fmt.Errorf("MR identity appeared at stage %q before mr", before.CurrentStage))
	}
	if err := writeMRAwarenessSummary(io.Discard, o.EvidencePath, report, nil); err != nil {
		return err
	}
	if err := killQueuedWorker(worker, o.Timeout); err != nil {
		return fail("worker_kill_failed", err)
	}
	worker = nil
	worker, err = startWorker()
	if err != nil {
		return fail("worker_restart_failed", err)
	}
	report.WorkerRestarts++
	after, err := awaitMRAwarenessRun(ctx, client, o, report.RunID, deadline, false)
	if err != nil {
		return fail("restart_observation_failed", err)
	}
	report.StageAfterRestart = after.CurrentStage
	if after.BacklogID != report.BacklogID || after.MRIID == nil || *after.MRIID != report.MRIID {
		return fail("mismatched_identity", errors.New("run or MR identity changed after restart"))
	}
	if !stageAtOrBeyondMR(after.CurrentStage) {
		return fail("stage_regression", fmt.Errorf("resumed at stage %q before mr", after.CurrentStage))
	}
	mrs, err := listBranchMRs(ctx, client, o, report.SourceBranch)
	if err != nil {
		return fail("gitlab_evidence_unavailable", err)
	}
	report.MRCount = len(mrs)
	if len(mrs) != 1 {
		return fail("mr_count_violation", fmt.Errorf("source branch %q has %d merge requests; want exactly 1", report.SourceBranch, len(mrs)))
	}
	if mrs[0].IID != report.MRIID || mrs[0].SourceBranch != report.SourceBranch {
		return fail("mismatched_identity", errors.New("GitLab MR identity contradicts persisted run"))
	}
	report.MRURL, report.Verdict, report.Passed, report.ReasonCode, report.Detail = mrs[0].WebURL, "PASS", true, "", ""
	return writeMRAwarenessSummary(out, o.EvidencePath, report, nil)
}

func recoveredMRIdentityError(report mrAwarenessSummary, observed int64) error {
	if report.Recovered && report.MRIID > 0 && observed != report.MRIID {
		return fmt.Errorf("recovered MR IID %d contradicts persisted run MR IID %d", report.MRIID, observed)
	}
	return nil
}

func validateMRAwarenessRecovery(data []byte, o mrAwarenessOptions, now time.Time) (mrAwarenessSummary, string, error) {
	var prior mrAwarenessSummary
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&prior); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		return prior, "malformed_evidence", errors.New("existing MR-awareness recovery evidence is malformed")
	}
	if prior.CapturedAt.IsZero() || now.Sub(prior.CapturedAt) > o.RecoveryMaxAge || prior.CapturedAt.After(now.Add(time.Minute)) {
		return prior, "stale_evidence", errors.New("existing MR-awareness recovery evidence is stale or future-dated")
	}
	if prior.Mode != string(pipeline.KilltestMRAwareness) || prior.BacklogID != o.BacklogID || prior.RunID == "" || prior.MRIID < 0 || prior.MRProject != o.GitLabProject ||
		(o.SourceBranch != "" && prior.SourceBranch != o.SourceBranch) {
		return prior, "mismatched_identity", errors.New("existing MR-awareness recovery evidence contradicts requested identity")
	}
	return prior, "", nil
}

func awaitMRAwarenessRun(ctx context.Context, client *http.Client, o mrAwarenessOptions, runID string, deadline time.Time, requireMR bool) (mrAwarenessRun, error) {
	for time.Now().Before(deadline) {
		var detail struct {
			Run mrAwarenessRun `json:"run"`
		}
		err := queuedAdmissionRequest(ctx, client, o.OperatorURL, o.AdminToken, http.MethodGet, "/api/mills/pipeline/runs/"+urlPathEscape(runID), nil, &detail, http.StatusOK)
		if err == nil && detail.Run.ID == runID && detail.Run.BacklogID == o.BacklogID && (!requireMR || detail.Run.MRIID != nil && *detail.Run.MRIID > 0) {
			return detail.Run, nil
		}
		select {
		case <-ctx.Done():
			return mrAwarenessRun{}, ctx.Err()
		case <-time.After(o.Poll):
		}
	}
	return mrAwarenessRun{}, errors.New("timed out waiting for durable run/MR evidence")
}

func stageAtOrBeyondMR(stage string) bool {
	mrIndex := -1
	for i, candidate := range pipeline.DefaultStages {
		if candidate.ID == "mr" {
			mrIndex = i
		}
		if candidate.ID == stage {
			return mrIndex >= 0 && i >= mrIndex
		}
	}
	// A terminal run clears CurrentStage after completing the entire DAG.
	return stage == ""
}

func listBranchMRs(ctx context.Context, client *http.Client, o mrAwarenessOptions, branch string) ([]gitLabMRIdentity, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, errors.New("source branch is missing from recovery evidence")
	}
	path := strings.TrimRight(o.GitLabURL, "/") + "/projects/" + urlPathEscape(o.GitLabProject) + "/merge_requests?scope=all&source_branch=" + urlQueryEscape(branch) + "&per_page=100"
	var mrs []gitLabMRIdentity
	if err := queuedAdmissionRequest(ctx, client, path, o.GitLabToken, http.MethodGet, "", nil, &mrs, http.StatusOK); err != nil {
		return nil, err
	}
	return mrs, nil
}

func writeMRAwarenessSummary(out io.Writer, path string, report mrAwarenessSummary, cause error) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			return fmt.Errorf("write MR-awareness summary: %w", err)
		}
	}
	if out != nil {
		if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
			return err
		}
	}
	return cause
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
func urlPathEscape(s string) string  { return url.PathEscape(s) }
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

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
