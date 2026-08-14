// Package killtest is the S1c deployed dual-crash kill-test harness for the
// Mills imperative workflow runtime (plan .loom/134 §3). It drives the EXACT
// documented procedure against the real deployed pods:
//
//	preflight → launch canary → await pending spawn → CRASH A (operator)
//	→ CRASH B (mobile-hud) → await terminal → collect evidence → verdicts
//
// The harness shells out to kubectl (KUBECONFIG passthrough) for the pod
// surface and talks to the operator's workflow and crash-lease REST endpoints
// for the durable step log and destructive-action fence — the same journal the
// runtime replays, so the evidence is the replay input, not a side
// reconstruction.
//
// Verdict evaluation (PASS-1/2/4/5 — PASS-3 is deferred to S6-full's merging
// canary) is pure and unit-tested; only the collection phases need a cluster.
//
// Safety: the harness never flips policy.workflows.enabled itself — the flag
// flip is a deliberate GitOps step in the runbook
// (docs/runbooks/mills-workflow-s1c-killtest.md) so the canary window is an
// audited commit, not a side effect of running a binary.
package killtest

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ProcessEvidenceMaxSampleGap is the versioned S1c proof allowance. Runtime
// configuration may tighten this bound but cannot weaken the verdict contract.
const ProcessEvidenceMaxSampleGap = 3 * time.Second

// Config carries every cluster/endpoint coordinate the harness needs. Zero
// values are filled by Defaults(). The selectors default to the values pinned
// live on 2026-07-08 (plan §4 B3 marked them [live-unverified]; they are now
// verified — see the runbook's preflight transcript).
type Config struct {
	// KubectlBin is the kubectl binary (default "kubectl"). KUBECONFIG comes
	// from the process environment, matching the runbook invocation.
	KubectlBin string
	// FluxBin is the Flux CLI binary (default "flux") used to reconstruct the
	// exact reviewed Deployment renders without consulting the cluster.
	FluxBin string

	// OperatorURL is the operator REST base, e.g. "http://localhost:8090"
	// via `kubectl -n loom-mills port-forward svc/loom-mills-operator 8090:8090`.
	OperatorURL string
	// AdminToken authorizes canary launch plus crash-lease acquire/renew/release
	// (LOOM_MILLS_ADMIN_TOKEN, k8s secret loom-mills-admin).
	AdminToken string

	// Merging selects the S6-full merging canary (template v3): the run gains
	// a single journaled merge('canary') effect after the gate and the gate
	// asserts PASS-3 (no double-merge) with real GitLab evidence.
	Merging bool
	// GitLabAPIURL/GitLabToken/GitLabProject authorize the PASS-3 exactly-once
	// merge verification reads. Required when Merging is set.
	GitLabAPIURL  string
	GitLabToken   string
	GitLabProject string
	// RequireAuthorityBinding enables the destructive S1c authority-plane
	// contract. Every kubectl/client-go operation then shares one frozen,
	// minified kubeconfig and every operator REST response must attest the exact
	// Downward-API Pod identity selected through Kubernetes. Diagnostic and unit
	// harnesses may leave this false; the full CLI gate always sets it true.
	RequireAuthorityBinding bool
	// ExpectedGitOpsRevision is the remote-main commit reviewed for this gate.
	// Preflight requires Flux's applied revision to end in this exact SHA when
	// set, preventing a healthy-but-stale reconciliation from authorizing a
	// destructive run.
	ExpectedGitOpsRevision string
	// ExpectedLoomCoreRevision independently binds the loom-core GitRepository
	// consumed by the loom-hub-servers Flux Kustomization. Platform GitOps and
	// loom-core can advance independently, so both sources must be fenced.
	ExpectedLoomCoreRevision string
	// GitOpsIdentityMode selects either exact commit identity or a reviewed
	// baseline plus unchanged protected source scope. GitOpsRepoPath is also
	// used to reconstruct the reviewed Flux Kustomization specs and is required
	// for a full gate; LoomCoreRepoPath is required for protected-scope mode.
	// Their working trees are never mutated; missing commit objects may be
	// fetched from origin/main.
	GitOpsIdentityMode string
	GitOpsRepoPath     string
	LoomCoreRepoPath   string
	// HudURL/HudAdminToken authorize exact spawn stop during failure cleanup.
	// A full gate must be able to terminalize both the workflow journal and the
	// mobile-hud-owned exec spawn before admission is reopened.
	HudURL        string
	HudAdminToken string

	// OperatorNS/OperatorSelector locate the operator pod for CRASH A.
	OperatorNS       string
	OperatorSelector string
	// HudNS/HudSelector locate the mobile-hud pod for CRASH B.
	HudNS       string
	HudSelector string
	// SpawnNS is where spawn pods land (plan §4: spawn pods + state ConfigMap
	// live in the devbox namespace, NOT loom-hub).
	SpawnNS string
	// LokiNS/LokiService identify the in-cluster Loki API used when a node's
	// kubelet log proxy is unavailable during a crash.
	LokiNS      string
	LokiService string

	// PollInterval/StepTimeout bound the await loops. TerminalTimeout bounds
	// the whole run-to-terminal wait (a claude-code implement spawn takes
	// minutes).
	PollInterval    time.Duration
	StepTimeout     time.Duration
	TerminalTimeout time.Duration
	// ProcessPollInterval and ProcessMaxSampleGap bound the crash-window process
	// sampling cadence. The observer starts synchronously before the operator
	// deletion and fails closed if cluster latency creates a larger blind window.
	ProcessPollInterval time.Duration
	ProcessMaxSampleGap time.Duration
	// ProcessProbeAttemptTimeout bounds each individual observation call
	// (exact-pod read, probe exec, post-failure rechecks) INSIDE a sample.
	// v6d run 1: a single probe exec hung through the whole 3s sample-gap
	// budget, so the transient-retry tick never got room to run and the
	// window failed on the gap breach. A healthy in-cluster call completes
	// in 100-300ms; killing a hung attempt early leaves the rest of the gap
	// budget for the retry. The overall sample deadline (previous sample +
	// ProcessMaxSampleGap) still caps everything — this cannot extend the
	// evidence contract, only stop one hung call from eating all of it.
	ProcessProbeAttemptTimeout time.Duration

	// Logf receives progress lines (default: stderr).
	Logf func(format string, args ...any)
}

// Defaults fills zero fields with the live-verified coordinates.
func (c Config) Defaults() Config {
	if c.KubectlBin == "" {
		c.KubectlBin = "kubectl"
	}
	if c.FluxBin == "" {
		c.FluxBin = "flux"
	}
	if c.GitOpsIdentityMode == "" {
		c.GitOpsIdentityMode = GitOpsIdentityModeExactRevision
	}
	if c.OperatorNS == "" {
		c.OperatorNS = "loom-mills"
	}
	if c.OperatorSelector == "" {
		c.OperatorSelector = "app.kubernetes.io/name=loom-mills-operator"
	}
	if c.HudNS == "" {
		c.HudNS = "loom-hub"
	}
	if c.HudSelector == "" {
		c.HudSelector = "app=mobile-hud"
	}
	if c.SpawnNS == "" {
		c.SpawnNS = "devbox"
	}
	if c.LokiNS == "" {
		c.LokiNS = "logging"
	}
	if c.LokiService == "" {
		c.LokiService = "http:loki:3100"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.StepTimeout <= 0 {
		c.StepTimeout = 5 * time.Minute
	}
	if c.TerminalTimeout <= 0 {
		c.TerminalTimeout = 30 * time.Minute
	}
	if c.ProcessPollInterval <= 0 {
		c.ProcessPollInterval = 250 * time.Millisecond
	}
	if c.ProcessMaxSampleGap <= 0 {
		c.ProcessMaxSampleGap = ProcessEvidenceMaxSampleGap
	}
	if c.ProcessProbeAttemptTimeout <= 0 {
		c.ProcessProbeAttemptTimeout = 1200 * time.Millisecond
	}
	if c.GitLabProject == "" {
		c.GitLabProject = "services/loom-core"
	}
	if c.Logf == nil {
		c.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	return c
}

// Harness executes the kill-test phases.
type Harness struct {
	cfg               Config
	http              *http.Client
	kubectlFn         func(context.Context, ...string) (string, error)
	deletePodFn       func(context.Context, string, string, string, string) error
	dryRunCreatePodFn func(context.Context, string, *corev1.Pod, metav1.CreateOptions) (*corev1.Pod, error)
	processProbeFn    func(context.Context, string, int, uint64, int, uint64) (CanaryProcessSample, error)
	// reviewedFluxRenderSpecsFn is a test seam for repositories synthesized
	// with non-addressable fixture revisions. Production always reads the
	// reviewed manifests directly from Git.
	reviewedFluxRenderSpecsFn    func(context.Context, string, string) (map[string]string, error)
	reviewedGitRepositorySpecsFn func(context.Context, string, string) (map[string]string, error)
	reviewedDeploymentsFn        func(context.Context, fluxSourceSnapshot) (map[string]reviewedDeployment, error)
	fluxBuildFn                  func(context.Context, string, string, string, string) ([]byte, error)
	fluxVersionFn                func(context.Context) (string, error)
	normalizeDeploymentFn        func(context.Context, *appsv1.Deployment, *appsv1.Deployment) (*appsv1.Deployment, error)
	finalPreDeleteCheckTimeout   time.Duration

	// spawnPodHistory retains exact pod UID observations made before and after
	// the crashes. CountSpawnPods is part of the pre-crash gate and cannot take
	// an Evidence pointer without breaking its public API, so AwaitTerminal
	// folds this retained history into the final evidence.
	spawnPodMu              sync.Mutex
	spawnPodHistory         map[string]map[string]PodIdentity
	reviewedDeploymentMu    sync.Mutex
	reviewedDeploymentCache map[string]map[string]reviewedDeployment

	kubeOnce sync.Once
	kube     kubernetes.Interface
	kubeErr  error

	kubeConfigOnce sync.Once
	frozenKube     *frozenKubeConfig
	kubeConfigErr  error

	authorityMu                sync.Mutex
	operatorResponseAuthority  OperatorResponseAuthority
	expectedOperatorPod        PodIdentity
	expectedOperatorDeployment DeploymentIdentity
	// reviewedOperatorDeployment is the immutable, pre-crash Deployment proof
	// used only to authorize fail-safe cleanup rebinding. Planned CRASH A may
	// advance expectedOperatorPod, but it must never replace this reviewed root.
	reviewedOperatorDeployment DeploymentIdentity
	clusterAuthority           KubernetesClusterAuthority
}

// New builds a harness. cfg is defaulted; OperatorURL is required for the
// launch/observe phases (preflight and crash phases are kubectl-only).
func New(cfg Config) *Harness {
	return &Harness{
		cfg: cfg.Defaults(), http: &http.Client{Timeout: 30 * time.Second},
		finalPreDeleteCheckTimeout: finalPreDeleteCheckTimeout,
	}
}

// Close removes the private frozen kubeconfig created for authority-bound
// production runs. It is safe to call when authority binding was not enabled.
func (h *Harness) Close() error {
	return h.closeFrozenKubeConfig()
}

func (h *Harness) freshOperatorURL(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return strings.TrimRight(h.cfg.OperatorURL, "/") + path + separator +
		"s1c_nonce=" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
}
