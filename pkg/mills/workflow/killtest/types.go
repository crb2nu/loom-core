package killtest

import (
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/mills/worker"
)

const (
	AgentTypeClaudeCode = worker.AgentTypeClaudeCode
	AgentTypeCodex      = worker.AgentTypeCodex

	// FluxProvenanceContract identifies the serialized source-fence proof. The
	// version changes whenever required owners or comparison semantics change.
	FluxProvenanceContract        = "mills-s1c-flux-provenance"
	FluxProvenanceContractVersion = 5
	// DeploymentProvenanceContract identifies the exact-render/full-spec
	// workload proof. The contract is independent from the Flux object
	// provenance version because it binds the rendered consumer objects rather
	// than changing the Kustomization source snapshot itself.
	DeploymentProvenanceContract        = "mills-s1c-deployment-provenance"
	DeploymentProvenanceContractVersion = 1
	// PolicyConfigMapProvenanceContract identifies the reviewed policy source,
	// complete rendered/live payload, and operator rollout-checksum proof.
	PolicyConfigMapProvenanceContract        = "mills-s1c-policy-configmap-provenance"
	PolicyConfigMapProvenanceContractVersion = 1
)

// CanaryMergeEvidence is the PASS-3 audit record.
type CanaryMergeEvidence struct {
	SourceBranch            string    `json:"source_branch"`
	MRCount                 int       `json:"mr_count"`
	MRIID                   int64     `json:"mr_iid"`
	MRState                 string    `json:"mr_state"`
	MergeCommitSHA          string    `json:"merge_commit_sha"`
	JournalMergeSuccessRows int       `json:"journal_merge_success_rows"`
	CollectedAt             time.Time `json:"collected_at"`
}

// validateKnownCanaryTemplateVersion accepts the two live canary contracts.
// The exact mode is pinned at launch (expectedCanaryTemplateVersion) and at
// the final identity check (Evidence.MergingCanary); mid-run evidence checks
// only need the version to be a known contract, since run-identity stability
// checks already refuse a version swap within one run.
func validateKnownCanaryTemplateVersion(v string) error {
	switch v {
	case "v2", "v3":
		return nil
	}
	return fmt.Errorf("unknown canary template version %q", v)
}

// ValidateAgentType restricts the deployed S1c canary to the two agent
// harnesses whose cost-provenance contracts the verdict evaluator knows how
// to prove. Do not accept aliases: the canonical value is part of the durable
// run and evidence identity.
func ValidateAgentType(agentType string) error {
	switch agentType {
	case AgentTypeClaudeCode, AgentTypeCodex:
		return nil
	default:
		return fmt.Errorf("unsupported agent type %q (want %s or %s)",
			agentType, AgentTypeClaudeCode, AgentTypeCodex)
	}
}

// RunDetail is the GET /api/mills/workflow/runs/{id} response.
type RunDetail struct {
	Run               RunSummary                `json:"run"`
	Steps             []StepView                `json:"steps"`
	OperatorAuthority OperatorResponseAuthority `json:"operator_authority"`
}

// RunSummary mirrors workflowRunSummary.
type RunSummary struct {
	ID                 string  `json:"id"`
	Engine             string  `json:"engine"`
	Template           string  `json:"template"`
	AgentType          string  `json:"agent_type"`
	TemplateVersion    string  `json:"template_version"`
	InterpreterVersion string  `json:"interpreter_version"`
	State              string  `json:"state"`
	CostUSD            float64 `json:"cost_usd"`
	StepCount          int     `json:"step_count"`
}

// StepView mirrors workflowStepView.
type StepView struct {
	StepKey     string  `json:"step_key"`
	EventType   string  `json:"event_type"`
	Status      string  `json:"status"`
	SpawnID     string  `json:"spawn_id,omitempty"`
	CallHash    string  `json:"call_hash"`
	CostUSD     float64 `json:"cost_usd"`
	CostSource  string  `json:"cost_source,omitempty"`
	EffectCount int     `json:"effect_count"`
	Badge       string  `json:"badge"`
}

// Evidence is everything the verdicts are computed from, serialized to the
// evidence file so a PASS/FAIL is re-derivable after the fact.
type Evidence struct {
	// GateBinding makes a three-run gate one server-bound session instead of
	// three independently valid artifacts. Run 2 and 3 carry the exact SHA-256
	// of the preceding final evidence file; recovery-only attached runs leave
	// this zero-valued.
	GateBinding GateBinding `json:"gate_binding"`
	RunID       string      `json:"run_id"`
	AgentType   string      `json:"agent_type"`
	// AgentStepKey/SpawnID identify the crash-critical step.
	AgentStepKey string `json:"agent_step_key"`
	SpawnID      string `json:"spawn_id"`
	SpawnPodName string `json:"spawn_pod_name"`
	// ExpectedIdempotencyKey is independently derived from the immutable
	// journal identity. The durable spawn record must carry this exact value;
	// merely carrying any non-empty key is not proof of identity.
	ExpectedIdempotencyKey string `json:"expected_idempotency_key"`

	// MergingCanary marks an S6-full merging-canary run (template v3): the
	// final identity check expects v3 and PASS-3 is evaluated from CanaryMerge
	// instead of being deferred.
	MergingCanary bool `json:"merging_canary,omitempty"`
	// CanaryMerge is the PASS-3 exactly-once merge evidence, collected after
	// the run reaches done: the GitLab MR population for the deterministic
	// canary merge branch plus the journal's merge success rows.
	CanaryMerge *CanaryMergeEvidence `json:"canary_merge,omitempty"`

	// MaxConcurrentSpawnPods is the maximum simultaneously-Running/Pending
	// pod count observed for the spawn id across the whole test (sampled
	// every PollInterval). >1 at any sample is a hard FAIL.
	MaxConcurrentSpawnPods int `json:"max_concurrent_spawn_pods"`
	// TotalSpawnPodNames is every distinct pod name ever observed for the
	// spawn id. It remains useful for diagnosing identity drift, but pod names
	// alone are not an exactly-once proof because Kubernetes can delete and
	// recreate a pod under the same deterministic name.
	TotalSpawnPodNames []string `json:"total_spawn_pod_names"`
	// TotalSpawnPodIncarnations is every distinct Kubernetes pod UID observed
	// for the derived spawn pod, including the pre-crash sample. A pod that is
	// deleted and recreated under the same name has a new UID and is therefore
	// a second side-effect incarnation. Exactly one entry is required to pass.
	// PodIdentity also retains useful immutable attribution (node, pod start,
	// requested image, and resolved image digest) when the API reports it.
	TotalSpawnPodIncarnations []PodIdentity `json:"total_spawn_pod_incarnations"`
	// The list/watch resourceVersion handshake closes the observation holes
	// between point polls. Full-gate evidence records the launch request after
	// the namespace-wide watch is established; a PASS requires one continuous
	// stream from before launch through both crashes and terminal collection.
	SpawnPodWatchStartedAt  time.Time `json:"spawn_pod_watch_started_at"`
	CanaryLaunchRequestedAt time.Time `json:"canary_launch_requested_at"`
	SpawnPodWatchEndedAt    time.Time `json:"spawn_pod_watch_ended_at"`
	SpawnPodWatchInitialRV  string    `json:"spawn_pod_watch_initial_resource_version"`
	SpawnPodWatchContinuous bool      `json:"spawn_pod_watch_continuous"`
	// SpawnPodWatchEvents retains the bounded Added/Modified/Deleted audit
	// stream, including label identity, rather than collapsing a conflicting
	// later event into the first observation for the same UID.
	SpawnPodWatchEvents []SpawnPodWatchEvent `json:"spawn_pod_watch_events,omitempty"`
	// TotalSpawnRecordIDs is every durable mobile-hud spawn record whose
	// record appeared after the preflight baseline. Unlike an exact pod-name
	// query, this catches a duplicate created under a drifted second spawn id
	// even if a reconciler rebuilt it without the original request branch.
	BaselineSpawnRecordIDs []string `json:"baseline_spawn_record_ids"`
	TotalSpawnRecordIDs    []string `json:"total_spawn_record_ids"`
	// FinalActiveSpawnRecordIDs must be empty after the workflow reaches done;
	// a non-terminal record is the zombie that the original live S1c exposed.
	FinalActiveSpawnRecordIDs []string          `json:"final_active_spawn_record_ids"`
	FinalSpawnRecordStatuses  map[string]string `json:"final_spawn_record_statuses"`
	FinalSpawnIdempotencyKeys map[string]string `json:"final_spawn_idempotency_keys"`
	FinalActiveSpawnPodNames  []string          `json:"final_active_spawn_pod_names"`

	// CanaryHoldInitial proves the canary's exact side-effect-free foreground
	// process was actually running before fault injection. Pod Ready and the
	// durable spawn status only prove that the driver is starting; they do not
	// prove the agent has entered the intended crash window.
	CanaryHoldInitial      CanaryHoldObservation `json:"canary_hold_initial"`
	CanaryHoldBeforeCrashA CanaryHoldObservation `json:"canary_hold_before_crash_a"`
	CanaryHoldBeforeCrashB CanaryHoldObservation `json:"canary_hold_before_crash_b"`
	// PostCrashProcessSamples prove that every retained bounded inventory has no
	// second exec driver or orphaned zombie in the already-existing spawn pod.
	// Observation starts immediately before CRASH A and stays open until the
	// exact pod is confirmed absent. This is not a continuous process trace: a
	// process whose entire lifetime falls between inventories can be missed, so
	// PostCrashProcessMaxGapMS records the explicit worst-case blind window.
	ProcessObservationStartedAt time.Time                  `json:"process_observation_started_at"`
	PostCrashProcessSamples     []CanaryProcessSample      `json:"post_crash_process_samples"`
	PostCrashProcessObservedEnd time.Time                  `json:"post_crash_process_observed_end"`
	PostCrashProcessMaxGapMS    int64                      `json:"post_crash_process_max_gap_ms"`
	CrashAProcessAuthorization  ProcessDeleteAuthorization `json:"crash_a_process_authorization"`
	CrashBProcessAuthorization  ProcessDeleteAuthorization `json:"crash_b_process_authorization"`

	// PostCrashProcessTransientFailures records observation-transport attempts
	// (kubectl killed/timed out during crash churn) that were retried on the
	// next poll tick instead of aborting the window. They are honesty, not
	// verdict inputs: coverage is still guaranteed by the sampling-gap
	// contract, which fails closed when no attempt completes in time. The
	// stored list is capped; the count is always exact.
	PostCrashProcessTransientFailures     []string `json:"post_crash_process_transient_failures,omitempty"`
	PostCrashProcessTransientFailureCount int      `json:"post_crash_process_transient_failure_count,omitempty"`

	// DedupeEvidence is the log line proving the durable dedupe path fired
	// (operator "workflow resume: re-attaching to in-flight spawn", mobile-hud
	// "idempotent spawn re-attach (already exists)", or a k8s AlreadyExists on
	// the derived name). Empty = not observed (PASS-1 then fails: "one pod"
	// alone is insufficient per plan §3.4).
	DedupeEvidence string `json:"dedupe_evidence,omitempty"`
	// DedupeLog identifies the exact replacement pod and Loki timestamp that
	// emitted DedupeEvidence. This prevents an older pod's historical line from
	// satisfying the post-crash proof.
	DedupeLog *LogEvidence `json:"dedupe_log,omitempty"`

	// ObservationErrors makes the bounded sampling claim fail closed. A missed
	// required read or an excessive gap invalidates the evidence; it does not
	// turn the point-in-time process inventories into a continuous trace.
	ObservationErrors []string `json:"observation_errors,omitempty"`

	// CrashAAt / CrashBAt timestamp the two kills.
	CrashAAt time.Time `json:"crash_a_at"`
	CrashBAt time.Time `json:"crash_b_at"`
	// CrashAFluxProvenance and CrashBFluxProvenance bind each destructive
	// delete to two coherent Kubernetes List snapshots: the identity-resolved
	// prepared snapshot and the final no-I/O snapshot immediately before the
	// UID-preconditioned delete. PASS-1 validates this proof from JSON alone.
	CrashAFluxProvenance FluxSourceFenceEvidence `json:"crash_a_flux_provenance"`
	CrashBFluxProvenance FluxSourceFenceEvidence `json:"crash_b_flux_provenance"`
	// InitialPreflight and FinalPreflight close the evidence boundary around the
	// full run. CrashASafety/CrashBSafety bind each mutation to its immediate
	// fleet probe, target-workload proof, renewed lease, and DELETE boundary.
	InitialPreflight PreflightReport     `json:"initial_preflight"`
	FinalPreflight   PreflightReport     `json:"final_preflight"`
	CrashASafety     CrashSafetyEvidence `json:"crash_a_safety"`
	CrashBSafety     CrashSafetyEvidence `json:"crash_b_safety"`
	// Before/replacement identities make the UID transition and stable image
	// digest independently re-derivable from the serialized evidence.
	CrashABefore      PodIdentity `json:"crash_a_before"`
	CrashAReplacement PodIdentity `json:"crash_a_replacement"`
	CrashBBefore      PodIdentity `json:"crash_b_before"`
	CrashBReplacement PodIdentity `json:"crash_b_replacement"`

	// Final is the terminal run detail (the journal truth).
	Final RunDetail `json:"final"`
}

// GateBinding is the immutable identity and predecessor link for one run in a
// canonical three-run S1c gate.
type GateBinding struct {
	Contract               string    `json:"contract"`
	ContractVersion        int       `json:"contract_version"`
	GateID                 string    `json:"gate_id"`
	RunIndex               int       `json:"run_index"`
	RequiredRuns           int       `json:"required_runs"`
	GateStartedAt          time.Time `json:"gate_started_at"`
	PreviousEvidenceSHA256 string    `json:"previous_evidence_sha256,omitempty"`
}

// Verdicts is the PASS/FAIL evaluation per plan §3.4/§3.5.
type Verdicts struct {
	Pass1NoDoubleSpawn  bool   `json:"pass1_no_double_spawn"`
	Pass1Reason         string `json:"pass1_reason"`
	Pass2JournalOnce    bool   `json:"pass2_journal_exactly_once"`
	Pass2Reason         string `json:"pass2_reason"`
	Pass3NotExercised   string `json:"pass3_note,omitempty"` // pre-merge canary only
	Pass3NoDoubleMerge  bool   `json:"pass3_no_double_merge,omitempty"`
	Pass3Reason         string `json:"pass3_reason,omitempty"`
	Pass4CostProvenance bool   `json:"pass4_cost_provenance"`
	Pass4Reason         string `json:"pass4_reason"`
	Pass5CounterExact   bool   `json:"pass5_counter_exact"`
	Pass5Reason         string `json:"pass5_reason"`
	Pass8CrashSafety    bool   `json:"pass8_crash_safety"`
	Pass8Reason         string `json:"pass8_reason"`
	Overall             bool   `json:"overall"`
}

// SpawnIdentity is the deterministic identity of one agent() side effect.
type SpawnIdentity struct {
	SpawnID        string `json:"spawn_id"`
	PodName        string `json:"pod_name"`
	IdempotencyKey string `json:"idempotency_key"`
}

// LogEvidence is one Loki entry attributed to an exact replacement pod.
type LogEvidence struct {
	Component string    `json:"component"`
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

// FluxRenderSpecIdentity binds a live Kustomization to the reviewed platform
// Git manifest that defines it. The explicit fields reject source/root
// redirects, SpecSHA256 fingerprints the complete live API spec, and the
// reviewed fields prove that fingerprint was reconstructed from the protected
// Git scope rather than merely remaining stable during the gate.
type FluxRenderSpecIdentity struct {
	Path                string `json:"path"`
	SourceRefKind       string `json:"source_ref_kind"`
	SourceRefName       string `json:"source_ref_name"`
	SourceRefNamespace  string `json:"source_ref_namespace"`
	TargetNamespace     string `json:"target_namespace"`
	ManifestPath        string `json:"manifest_path"`
	SpecSHA256          string `json:"spec_sha256"`
	ReviewedSpecSHA256  string `json:"reviewed_spec_sha256"`
	ReviewedRevision    string `json:"reviewed_revision"`
	ReviewedScopeDigest string `json:"reviewed_scope_digest"`
}

// GitRepositorySpecIdentity binds one live Flux source object to the complete
// spec reconstructed from the reviewed platform Git manifest. SecretRefName
// records only the referenced Secret identity; secret contents are never read
// or serialized by the kill-test.
type GitRepositorySpecIdentity struct {
	URL                 string `json:"url"`
	RefBranch           string `json:"ref_branch"`
	SecretRefName       string `json:"secret_ref_name"`
	ManifestPath        string `json:"manifest_path"`
	SpecSHA256          string `json:"spec_sha256"`
	ReviewedSpecSHA256  string `json:"reviewed_spec_sha256"`
	ReviewedRevision    string `json:"reviewed_revision"`
	ReviewedScopeDigest string `json:"reviewed_scope_digest"`
}

// GitRepositoryProvenance is the complete non-secret source identity required
// by S1c. Both controller-wide status.observedGeneration and the two
// source-controller conditions are retained so readiness cannot be borrowed
// from a previous spec generation.
type GitRepositoryProvenance struct {
	Name                                string                    `json:"name"`
	Namespace                           string                    `json:"namespace"`
	UID                                 string                    `json:"uid"`
	ResourceVersion                     string                    `json:"resource_version"`
	Generation                          int64                     `json:"generation"`
	DeletionTimestamp                   string                    `json:"deletion_timestamp"`
	Terminating                         bool                      `json:"terminating"`
	StatusObservedGeneration            int64                     `json:"status_observed_generation"`
	ReadyObservedGeneration             int64                     `json:"ready_observed_generation"`
	ReadyStatus                         string                    `json:"ready_status"`
	ArtifactInStorageObservedGeneration int64                     `json:"artifact_in_storage_observed_generation"`
	ArtifactInStorageStatus             string                    `json:"artifact_in_storage_status"`
	ArtifactRevision                    string                    `json:"artifact_revision"`
	ArtifactDigest                      string                    `json:"artifact_digest"`
	Spec                                GitRepositorySpecIdentity `json:"spec"`
	ProtectedIdentity                   GitOpsScopeIdentity       `json:"protected_identity"`
}

// GitRepositoryProvenanceSnapshot is the second source List in the
// GitRepository-A -> Kustomization-List -> GitRepository-B observation
// bracket. Only the two source objects consumed by S1c are persisted.
type GitRepositoryProvenanceSnapshot struct {
	ListResourceVersion string                    `json:"list_resource_version"`
	ObservedAt          time.Time                 `json:"observed_at"`
	Repositories        []GitRepositoryProvenance `json:"repositories"`
}

// FluxSourceProvenance is the independently verifiable state of one required
// Flux Kustomization in a coherent collection snapshot.
type FluxSourceProvenance struct {
	Name                    string                 `json:"name"`
	UID                     string                 `json:"uid"`
	ResourceVersion         string                 `json:"resource_version"`
	Generation              int64                  `json:"generation"`
	DeletionTimestamp       string                 `json:"deletion_timestamp"`
	Terminating             bool                   `json:"terminating"`
	ReadyObservedGeneration int64                  `json:"ready_observed_generation"`
	ReadyStatus             string                 `json:"ready_status"`
	AppliedRevision         string                 `json:"applied_revision"`
	AttemptedRevision       string                 `json:"attempted_revision"`
	RenderSpec              FluxRenderSpecIdentity `json:"render_spec"`
	ProtectedIdentity       GitOpsScopeIdentity    `json:"protected_identity"`
}

// FluxSourceProvenanceSnapshot records one Kubernetes List resourceVersion and
// the four render owners required by the S1c crash-safety contract. Sources are
// serialized in contract order but validated by name.
type FluxSourceProvenanceSnapshot struct {
	Contract                string                          `json:"contract"`
	ContractVersion         int                             `json:"contract_version"`
	ListResourceVersion     string                          `json:"list_resource_version"`
	GitRepositoriesOpenedAt time.Time                       `json:"git_repositories_opened_at"`
	ObservedAt              time.Time                       `json:"observed_at"`
	GitRepositories         GitRepositoryProvenanceSnapshot `json:"git_repositories"`
	Sources                 []FluxSourceProvenance          `json:"sources"`
}

// FluxSourceFenceEvidence proves that no required render owner changed between
// the identity-resolved prepared snapshot and the final pre-delete snapshot.
type FluxSourceFenceEvidence struct {
	Prepared FluxSourceProvenanceSnapshot `json:"prepared"`
	Final    FluxSourceProvenanceSnapshot `json:"final"`
}

// CrashLeaseEvidence is the non-secret, server-authored identity and lifetime
// of a lease. The bearer token is deliberately excluded from persisted proof.
type CrashLeaseEvidence struct {
	RequestID         string                    `json:"request_id"`
	RunID             string                    `json:"run_id"`
	SpawnID           string                    `json:"spawn_id"`
	ObservedAt        time.Time                 `json:"observed_at"`
	ExpiresAt         time.Time                 `json:"expires_at"`
	OperatorAuthority OperatorResponseAuthority `json:"operator_authority"`
}

// ProcessDeleteAuthorization binds a delete boundary to the exact completed,
// live process sample that authorized it. SampleIndex is zero-based in
// PostCrashProcessSamples.
type ProcessDeleteAuthorization struct {
	SampleIndex       int       `json:"sample_index"`
	SampleObservedAt  time.Time `json:"sample_observed_at"`
	SampleCompletedAt time.Time `json:"sample_completed_at"`
	AuthorizedAt      time.Time `json:"authorized_at"`
}

// CrashTargetSafetyEvidence serializes every successful read performed by the
// final target-specific crash gate. A verdict can therefore re-derive that the
// allowed workflow and its exact spawn were the fleet's only active work.
type CrashTargetSafetyEvidence struct {
	Quiescence            QuiescenceSnapshot        `json:"quiescence"`
	QuiescenceCollectedAt time.Time                 `json:"quiescence_collected_at"`
	Run                   RunSummary                `json:"run"`
	RunAuthority          OperatorResponseAuthority `json:"run_authority"`
	AgentStep             StepView                  `json:"agent_step"`
	DerivedSpawn          SpawnIdentity             `json:"derived_spawn"`
	SpawnState            SpawnStateSnapshot        `json:"spawn_state"`
	ActiveSpawnPodNames   []string                  `json:"active_spawn_pod_names"`
	ExactSpawnPodActive   int                       `json:"exact_spawn_pod_active"`
	ExactSpawnPodReady    int                       `json:"exact_spawn_pod_ready"`
	ExactSpawnPodNames    []string                  `json:"exact_spawn_pod_names"`
	ObservedAt            time.Time                 `json:"observed_at"`
}

// CrashSafetyEvidence binds the immediate full-fleet preflight, the exact
// target proof, and both lease observations to one DELETE request boundary.
type CrashSafetyEvidence struct {
	ImmediatePreflight     PreflightReport              `json:"immediate_preflight"`
	Target                 CrashTargetSafetyEvidence    `json:"target"`
	PolicyDeleteBoundary   PolicyDeleteBoundaryEvidence `json:"policy_delete_boundary"`
	LeaseAcquired          CrashLeaseEvidence           `json:"lease_acquired"`
	LeaseRenewed           CrashLeaseEvidence           `json:"lease_renewed"`
	DeleteIntentRecordedAt time.Time                    `json:"delete_intent_recorded_at"`
	DeleteRequestedAt      time.Time                    `json:"delete_requested_at"`
	DeleteAcceptedAt       time.Time                    `json:"delete_accepted_at"`
}

// PreflightReport is the §3.2 precondition probe result.
type PreflightReport struct {
	AuthorityPlane                  AuthorityPlaneEvidence        `json:"authority_plane"`
	FluxSourcesStart                FluxSourceProvenanceSnapshot  `json:"flux_sources_start"`
	FluxSourcesEnd                  FluxSourceProvenanceSnapshot  `json:"flux_sources_end"`
	NamespacesOK                    bool                          `json:"namespaces_ok"`
	OperatorImage                   string                        `json:"operator_image"`
	Operator                        PodIdentity                   `json:"operator"`
	OperatorDeployment              DeploymentIdentity            `json:"operator_deployment"`
	HudImage                        string                        `json:"hud_image"`
	Hud                             PodIdentity                   `json:"hud"`
	HudDeployment                   DeploymentIdentity            `json:"hud_deployment"`
	GitOpsStartRevision             string                        `json:"gitops_start_revision"`
	GitOpsStartIdentity             GitOpsScopeIdentity           `json:"gitops_start_identity"`
	GitOpsRevision                  string                        `json:"gitops_revision"`
	GitOpsAttempted                 string                        `json:"gitops_attempted_revision"`
	GitOpsReady                     bool                          `json:"gitops_ready"`
	GitOpsIdentity                  GitOpsScopeIdentity           `json:"gitops_identity"`
	GitOpsBootstrapStartRevision    string                        `json:"gitops_bootstrap_start_revision"`
	GitOpsBootstrapStartIdentity    GitOpsScopeIdentity           `json:"gitops_bootstrap_start_identity"`
	GitOpsBootstrapRevision         string                        `json:"gitops_bootstrap_revision"`
	GitOpsBootstrapAttempted        string                        `json:"gitops_bootstrap_attempted_revision"`
	GitOpsBootstrapReady            bool                          `json:"gitops_bootstrap_ready"`
	GitOpsBootstrapIdentity         GitOpsScopeIdentity           `json:"gitops_bootstrap_identity"`
	GitOpsSystemStartRevision       string                        `json:"gitops_system_start_revision"`
	GitOpsSystemStartIdentity       GitOpsScopeIdentity           `json:"gitops_system_start_identity"`
	GitOpsSystemRevision            string                        `json:"gitops_system_revision"`
	GitOpsSystemAttempted           string                        `json:"gitops_system_attempted_revision"`
	GitOpsSystemReady               bool                          `json:"gitops_system_ready"`
	GitOpsSystemIdentity            GitOpsScopeIdentity           `json:"gitops_system_identity"`
	LoomCoreStartRevision           string                        `json:"loom_core_start_revision"`
	LoomCoreStartIdentity           GitOpsScopeIdentity           `json:"loom_core_start_identity"`
	LoomCoreRevision                string                        `json:"loom_core_revision"`
	LoomCoreAttempted               string                        `json:"loom_core_attempted_revision"`
	LoomCoreReady                   bool                          `json:"loom_core_ready"`
	LoomCoreIdentity                GitOpsScopeIdentity           `json:"loom_core_identity"`
	PolicyChecksum                  string                        `json:"policy_checksum"`
	PolicyConfigMapIdentity         KubernetesObjectIdentity      `json:"policy_configmap_identity"`
	PolicyConfigMapReview           PolicyConfigMapReviewIdentity `json:"policy_configmap_review"`
	WorkflowsFlag                   string                        `json:"workflows_flag"`
	ConfigMapPolicyEnabled          bool                          `json:"configmap_policy_enabled"`
	FlagEnabled                     bool                          `json:"flag_enabled"`
	SubstrateK8sOnly                bool                          `json:"substrate_k8s_only"`
	EffectivePolicyEnabled          bool                          `json:"effective_policy_enabled"`
	EffectiveFlagEnabled            bool                          `json:"effective_flag_enabled"`
	EffectiveSubstrateK8sOnly       bool                          `json:"effective_substrate_k8s_only"`
	EffectivePolicyMatchesConfigMap bool                          `json:"effective_policy_matches_configmap"`
	EffectivePolicyAuthority        OperatorResponseAuthority     `json:"effective_policy_authority"`
	SpawnConfigMap                  bool                          `json:"spawn_configmap_in_devbox"`
	SpawnConfigMapUID               string                        `json:"spawn_configmap_uid"`
	SpawnConfigMapIdentity          KubernetesObjectIdentity      `json:"spawn_configmap_identity"`
	SpawnConfigMapUpdateAllowed     bool                          `json:"spawn_configmap_update_allowed"`
	SpawnRecordIDs                  []string                      `json:"spawn_record_ids"`
	ActiveSpawnIDs                  []string                      `json:"active_spawn_ids"`
	ActiveSpawnPodNames             []string                      `json:"active_spawn_pod_names"`
	Quiescence                      QuiescenceSnapshot            `json:"quiescence"`
	LokiReady                       bool                          `json:"loki_ready"`
	AllPreconditions                bool                          `json:"all_preconditions"`
}

// QuiescenceSnapshot mirrors GET /api/mills/safety/quiescence. All counts are
// produced by one SQLite read snapshot and the endpoint returns non-2xx on any
// query error, so zero means observed zero rather than unavailable.
type QuiescenceSnapshot struct {
	ObservedAt        time.Time                  `json:"observed_at"`
	Quiescent         bool                       `json:"quiescent"`
	Counts            QuiescenceCounts           `json:"counts"`
	InMemory          QuiescenceInMemoryActivity `json:"in_memory"`
	OperatorAuthority OperatorResponseAuthority  `json:"operator_authority"`
}

type QuiescenceInMemoryActivity struct {
	AdmissionClosed      bool                `json:"admission_closed"`
	CrashLeaseActive     bool                `json:"crash_lease_active"`
	PolicyGeneration     uint64              `json:"policy_generation"`
	SourcesReady         bool                `json:"sources_ready"`
	SampleStable         bool                `json:"sample_stable"`
	WiringRequired       bool                `json:"wiring_required"`
	ActivitySources      int                 `json:"activity_sources"`
	SourceGeneration     uint64              `json:"source_generation"`
	SourceOperations     map[string]int64    `json:"source_operations"`
	SourceRunIDs         map[string][]string `json:"source_run_ids"`
	MissingSources       []string            `json:"missing_sources"`
	WiringError          string              `json:"wiring_error,omitempty"`
	ActiveAdmissions     int64               `json:"active_admissions"`
	CanaryAdmissions     int64               `json:"canary_admissions"`
	BackgroundOperations int64               `json:"background_operations"`
	SpinWorkers          int64               `json:"spin_workers"`
	AuditOutstanding     int64               `json:"audit_outstanding"`
}

func (a QuiescenceInMemoryActivity) idle() bool {
	return a.idleWithAllowances("", false)
}

func (a QuiescenceInMemoryActivity) idleForWorkflow(runID string) bool {
	return a.idleWithAllowances(runID, false)
}

func (a QuiescenceInMemoryActivity) idleForWorkflowCrashLease(runID string) bool {
	return a.idleWithAllowances(runID, true)
}

func (a QuiescenceInMemoryActivity) idleWithAllowances(runID string, requireCrashLease bool) bool {
	if !a.AdmissionClosed || a.CrashLeaseActive != requireCrashLease || a.PolicyGeneration == 0 || !a.SourcesReady || !a.SampleStable ||
		!a.WiringRequired || a.ActivitySources < 6 || a.SourceGeneration == 0 ||
		len(a.MissingSources) != 0 || a.WiringError != "" || a.ActiveAdmissions != 0 ||
		a.CanaryAdmissions != 0 || a.SpinWorkers != 0 ||
		a.AuditOutstanding != 0 {
		return false
	}
	for _, name := range []string{"reconciler", "pipeline", "cross_run", "council", "canary", "workflow"} {
		active, ok := a.SourceOperations[name]
		if !ok {
			return false
		}
		if name == "workflow" {
			ids := a.SourceRunIDs[name]
			if runID == "" && (active != 0 || len(ids) != 0) {
				return false
			}
			if runID != "" {
				if active == 0 && len(ids) == 0 {
					continue
				}
				if active != 1 || len(ids) != 1 || ids[0] != runID {
					return false
				}
			}
			continue
		}
		if active != 0 {
			return false
		}
	}
	workflowActive := a.SourceOperations["workflow"]
	if a.BackgroundOperations != workflowActive {
		return false
	}
	for name, active := range a.SourceOperations {
		if name != "workflow" && active != 0 {
			return false
		}
	}
	return true
}

type QuiescenceCounts struct {
	QueuedBacklog          int `json:"queued_backlog"`
	ActivePipelineRuns     int `json:"active_pipeline_runs"`
	ActiveWorkflowRuns     int `json:"active_workflow_runs"`
	ActiveSpinningRoomRuns int `json:"active_spinning_room_runs"`
	ActiveCouncilRuns      int `json:"active_council_runs"`
	ActiveCrossRepoRuns    int `json:"active_cross_repo_runs"`
	PendingDispatches      int `json:"pending_dispatches"`
}

func (c QuiescenceCounts) unrelatedIdle(expectedWorkflowRuns int) bool {
	return c.QueuedBacklog == 0 && c.ActivePipelineRuns == 0 &&
		c.ActiveWorkflowRuns == expectedWorkflowRuns && c.ActiveSpinningRoomRuns == 0 &&
		c.ActiveCouncilRuns == 0 && c.ActiveCrossRepoRuns == 0 && c.PendingDispatches == 0
}

// DeploymentIdentity proves a singleton deployment is fully observed and
// stable before one of its pods is selected for a destructive test.
type DeploymentIdentity struct {
	Name               string `json:"name"`
	Namespace          string `json:"namespace"`
	UID                string `json:"uid"`
	ResourceVersion    string `json:"resource_version"`
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observed_generation"`
	DesiredReplicas    int32  `json:"desired_replicas"`
	Replicas           int32  `json:"replicas"`
	UpdatedReplicas    int32  `json:"updated_replicas"`
	ReadyReplicas      int32  `json:"ready_replicas"`
	AvailableReplicas  int32  `json:"available_replicas"`
	Image              string `json:"image"`
	ContainerName      string `json:"container_name"`
	Strategy           string `json:"strategy"`
	PolicyChecksum     string `json:"policy_checksum,omitempty"`
	// SpecSHA256 fingerprints the complete live Deployment spec returned by
	// the API. ReviewedSpecSHA256 fingerprints the exact-commit render after a
	// server-side dry-run UPDATE applies the same Kubernetes defaults and
	// admission as the live object. Equality is the admission invariant.
	SpecSHA256                string                   `json:"spec_sha256"`
	ReviewedSpecSHA256        string                   `json:"reviewed_spec_sha256"`
	PodTemplateSHA256         string                   `json:"pod_template_sha256"`
	ReviewedPodTemplateSHA256 string                   `json:"reviewed_pod_template_sha256"`
	SelectorSHA256            string                   `json:"selector_sha256"`
	ReviewedSelectorSHA256    string                   `json:"reviewed_selector_sha256"`
	Review                    DeploymentReviewIdentity `json:"review"`
}

// DeploymentReviewIdentity makes a full-spec Deployment comparison
// independently attributable to the exact source commits, Flux transform,
// and renderer that produced the reviewed desired object. SourceRevision is
// platform GitOps for the operator and loom-core for mobile-hud; the platform
// fields always bind the Flux Kustomization manifest and its transforms.
type DeploymentReviewIdentity struct {
	Contract            string `json:"contract"`
	ContractVersion     int    `json:"contract_version"`
	FluxOwner           string `json:"flux_owner"`
	FluxSpecSHA256      string `json:"flux_spec_sha256"`
	Renderer            string `json:"renderer"`
	RendererVersion     string `json:"renderer_version"`
	RenderedSpecSHA256  string `json:"rendered_spec_sha256"`
	PlatformRevision    string `json:"platform_revision"`
	PlatformScopeDigest string `json:"platform_scope_digest"`
	SourceRevision      string `json:"source_revision"`
	SourceScopeDigest   string `json:"source_scope_digest"`
}

// PolicyConfigMapReviewIdentity makes the live policy payload independently
// attributable to the exact reviewed platform commit and apps Flux render.
// SourceSHA256 is SHA-256 over the exact committed source-file bytes (without
// YAML reserialization or newline normalization). RenderedPayloadSHA256 and
// LivePayloadSHA256 cover the complete data, binaryData, and immutable fields.
type PolicyConfigMapReviewIdentity struct {
	Contract              string `json:"contract"`
	ContractVersion       int    `json:"contract_version"`
	Name                  string `json:"name"`
	Namespace             string `json:"namespace"`
	FluxOwner             string `json:"flux_owner"`
	FluxSpecSHA256        string `json:"flux_spec_sha256"`
	Renderer              string `json:"renderer"`
	RendererVersion       string `json:"renderer_version"`
	PlatformRevision      string `json:"platform_revision"`
	PlatformScopeDigest   string `json:"platform_scope_digest"`
	SourcePath            string `json:"source_path"`
	SourceSHA256          string `json:"source_sha256"`
	RenderedPayloadSHA256 string `json:"rendered_payload_sha256"`
	LivePayloadSHA256     string `json:"live_payload_sha256"`
}

// PodIdentity records both standalone spawn-pod attribution and the stronger
// controller identity required at a destructive crash boundary. Controller
// pods populate every field; standalone spawn observations intentionally leave
// the controller/owner fields empty. UID and container ID change across a
// planned crash while the image and Deployment owner identity must not.
type PodIdentity struct {
	Name                         string    `json:"name"`
	Namespace                    string    `json:"namespace"`
	UID                          string    `json:"uid"`
	ResourceVersion              string    `json:"resource_version"`
	PodCensusListResourceVersion string    `json:"pod_census_list_resource_version"`
	PodCensusCount               int       `json:"pod_census_count"`
	Node                         string    `json:"node"`
	Image                        string    `json:"image"`
	ImageID                      string    `json:"image_id"`
	StartedAt                    time.Time `json:"started_at"`

	ContainerName         string    `json:"container_name"`
	ContainerID           string    `json:"container_id"`
	ContainerRestartCount int32     `json:"container_restart_count"`
	ContainerStartedAt    time.Time `json:"container_started_at"`

	ReplicaSetName                 string `json:"replicaset_name"`
	ReplicaSetUID                  string `json:"replicaset_uid"`
	ReplicaSetResourceVersion      string `json:"replicaset_resource_version"`
	ReplicaSetPodTemplateSHA256    string `json:"replicaset_pod_template_sha256"`
	ReplicaSetSelectorSHA256       string `json:"replicaset_selector_sha256"`
	ReplicaSetGeneration           int64  `json:"replicaset_generation"`
	ReplicaSetObservedGeneration   int64  `json:"replicaset_observed_generation"`
	ReplicaSetDesiredReplicas      int32  `json:"replicaset_desired_replicas"`
	ReplicaSetReplicas             int32  `json:"replicaset_replicas"`
	ReplicaSetFullyLabeledReplicas int32  `json:"replicaset_fully_labeled_replicas"`
	ReplicaSetReadyReplicas        int32  `json:"replicaset_ready_replicas"`
	ReplicaSetAvailableReplicas    int32  `json:"replicaset_available_replicas"`
	PodExecutionContract           string `json:"pod_execution_contract"`
	PodExecutionContractVersion    int    `json:"pod_execution_contract_version"`
	PodExecutionRenderer           string `json:"pod_execution_renderer"`
	PodExecutionRendererVersion    string `json:"pod_execution_renderer_version"`
	LivePodSpecSHA256              string `json:"live_pod_spec_sha256"`
	DryRunPodSpecSHA256            string `json:"dry_run_pod_spec_sha256"`
	DeploymentName                 string `json:"deployment_name"`
	DeploymentUID                  string `json:"deployment_uid"`
}

// SpawnStateSnapshot is the durable mobile-hud view used to detect duplicates
// under a different derived spawn id and terminal workflow zombies.
type SpawnStateSnapshot struct {
	ConfigMapUID      string                   `json:"configmap_uid"`
	ConfigMapIdentity KubernetesObjectIdentity `json:"configmap_identity"`
	RecordIDs         []string                 `json:"record_ids"`
	ActiveIDs         []string                 `json:"active_ids"`
	Statuses          map[string]string        `json:"statuses"`
	IdempotencyKeys   map[string]string        `json:"idempotency_keys"`
}
