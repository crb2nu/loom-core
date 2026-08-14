package agentcontext

import (
	"context"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/flexinfer"
	"github.com/crb2nu/loom/pkg/httpclient"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const sessionsVectorSize = 4

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithLogger sets the structured logger for the service.
func WithLogger(logger *slog.Logger) ServiceOption {
	return func(s *Service) { s.logger = logger }
}

// WithTracer sets the OpenTelemetry TracerProvider for the service.
func WithTracer(tp trace.TracerProvider) ServiceOption {
	return func(s *Service) { s.tracer = tp.Tracer("agentcontext") }
}

// WithReranker overrides the recall reranker. Production wires this from
// WEAVER_RERANKER env config; tests inject a fake to exercise the recall
// second-stage rerank deterministically.
func WithReranker(r Reranker) ServiceOption {
	return func(s *Service) { s.reranker = r }
}

// WithCommandRunner wires a backend for engram `command:` proof verification.
// When set, HandleEngramVerify will execute commands via the runner instead of
// returning skipped. Production binaries adapt this to devbox; tests inject a
// fake runner.
func WithCommandRunner(r CommandRunner) ServiceOption {
	return func(s *Service) { s.commandRunner = r }
}

type Service struct {
	cfg     Config
	logger  *slog.Logger
	tracer  trace.Tracer
	metrics *Metrics

	qdrant *QdrantRegistry
	embed  embed.Embedder

	// Recall reranker (default NoopReranker / "off"). Constructed from
	// WEAVER_RERANKER env config in NewServiceFromEnv; overridable in tests
	// via WithReranker. HandleUnifiedRecall applies it as a second stage
	// after embedding retrieval.
	reranker Reranker

	vectorSize int

	// Workflow orchestration
	workflowEngine *WorkflowEngine

	// Knowledge graph (with persistence)
	knowledgeGraph          *KnowledgeGraph
	persistedKnowledgeGraph *persistedGraph

	// Memory hierarchy (with persistence)
	memoryHierarchy          *MemoryHierarchy
	persistedMemoryHierarchy *persistedMemoryHierarchy

	// Workflow engine (with persistence)
	persistedWorkflowEngine *persistedWorkflowEngine

	// Domain sub-services
	presence  *PresenceSvc
	claims    *ClaimSvc
	worktrees *WorktreeSvc
	tasks     *TaskSvc
	sess      *SessionSvc
	plans     *PlanSvc
	patterns  *PatternSvc

	// Context operations (entries, annotations, recall, search, summaries)
	ctxSvc *ContextSvc

	// Phase 2 domain extractions
	graph         *GraphSvc
	memory        *MemorySvc
	workflow      *WorkflowSvc
	sourceVersion *SourceVersionSvc
	handoffs      *HandoffSvc
	messages      *MessageSvc
	vendorSess    *VendorSessionsSvc
	mrStatus      *MRStatusSvc

	// Nudge queue
	nudges *NudgeSvc

	// Optional backend for engram command-proof verification (devbox in prod,
	// fake in tests). nil → command proofs are skipped.
	commandRunner CommandRunner

	// Background services
	compactionScheduler *CompactionScheduler
	planReconciler      *PlanReconciler
	bgCancel            context.CancelFunc
}

// embedResilientConfigFromEnv builds the embedder circuit-breaker policy,
// starting from the package defaults and applying optional env overrides:
//
//	AGENT_CONTEXT_EMBED_TIMEOUT            Go duration, per-call timeout (default 3s)
//	AGENT_CONTEXT_EMBED_BREAKER_THRESHOLD  int, consecutive failures to open (default 3)
//	AGENT_CONTEXT_EMBED_BREAKER_COOLDOWN   Go duration, open duration (default 30s)
//
// buildEmbedder constructs a raw (unwrapped) Embedder for the given provider.
// Shared by the primary and the optional write-path fallback so both honor the
// same provider defaults. flexinfer/ollama substitute sensible base URLs and
// models when the caller left the Morph defaults in place.
func buildEmbedder(hc *httpclient.Client, provider, baseURL, apiKey, model string) embed.Embedder {
	switch provider {
	case "flexinfer":
		if baseURL == "" || baseURL == flexinfer.RetiredMorphBaseURL {
			baseURL = env.StringChain([]string{"FLEXINFER_URL"}, flexinfer.DefaultProxyURL) + "/v1"
		}
		if model == "" || model == flexinfer.RetiredMorphModel {
			model = flexinfer.DefaultEmbedModel
		}
		return embed.NewFlexInferClient(hc, baseURL, apiKey, model)
	case "ollama":
		if baseURL == "" || baseURL == flexinfer.RetiredMorphBaseURL {
			baseURL = "http://localhost:11434"
		}
		if model == "" || model == flexinfer.RetiredMorphModel {
			model = "nomic-embed-text"
		}
		return embed.NewOllamaClient(hc, baseURL, model)
	case "dummy", "none":
		return embed.NewDummyEmbedder(1)
	default:
		return embed.NewMorphClient(hc, baseURL, apiKey, model)
	}
}

// newResilientEmbedder wraps a real provider with the circuit breaker + timeout
// policy. The dummy embedder never fails, so it is returned unwrapped.
func newResilientEmbedder(e embed.Embedder) embed.Embedder {
	if _, isDummy := e.(*embed.DummyEmbedder); isDummy {
		return e
	}
	return embed.NewResilientEmbedder(e, embedResilientConfigFromEnv())
}

func embedResilientConfigFromEnv() embed.ResilientConfig {
	c := embed.DefaultResilientConfig()
	if v := env.Duration("AGENT_CONTEXT_EMBED_TIMEOUT", 0); v > 0 {
		c.Timeout = v
	}
	if v := env.IntWithZero("AGENT_CONTEXT_EMBED_BREAKER_THRESHOLD", 0); v > 0 {
		c.FailureThreshold = v
	}
	if v := env.Duration("AGENT_CONTEXT_EMBED_BREAKER_COOLDOWN", 0); v > 0 {
		c.Cooldown = v
	}
	return c
}

func NewServiceFromEnv(opts ...ServiceOption) (*Service, error) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}

	hc := httpclient.NewDefault()

	// Primary embedder, wrapped with a circuit breaker + short per-call timeout
	// so a stalled/overloaded provider fails fast instead of head-of-line
	// blocking the single MCP stdio transport (which starves unrelated tools
	// like session_list / presence_heartbeat).
	embedder := newResilientEmbedder(buildEmbedder(hc, cfg.EmbedProvider, cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel))

	// Optional write-path fallback: when configured, document embeddings retry
	// on a secondary provider if the primary fails, so agent_context_add keeps
	// working through a primary outage. Queries never use the fallback (it is a
	// different vector space → search degrades to keyword instead). The fallback
	// model MUST emit the same vector dimension as the collection.
	if cfg.EmbedFallbackProvider != "" {
		secondary := buildEmbedder(hc, cfg.EmbedFallbackProvider, cfg.EmbedFallbackBaseURL, cfg.EmbedFallbackAPIKey, cfg.EmbedFallbackModel)
		if _, isDummy := secondary.(*embed.DummyEmbedder); !isDummy {
			// Fallback lanes are often cold/serverless and need a longer per-call
			// timeout than the primary to tolerate first-call activation.
			rc := embedResilientConfigFromEnv()
			if cfg.EmbedFallbackTimeout > 0 {
				rc.Timeout = cfg.EmbedFallbackTimeout
			}
			secondary = embed.NewResilientEmbedder(secondary, rc)
		}
		embedder = embed.NewFallbackEmbedder(embedder, secondary)
	}

	qdrantReg := NewQdrantRegistry(hc, cfg)

	svc := &Service{
		cfg:    cfg,
		qdrant: qdrantReg,
		embed:  embedder,

		nudges: NewNudgeSvc(),
	}

	// Best-effort: if the context collection already exists, remember its vector size
	// so we can avoid "unknown vector size" edge-cases on operations like share/summarize.
	if exists, size, err := svc.qdrant.Get(CollContext).GetCollectionVectorSize(context.Background()); err == nil && exists && size > 0 {
		svc.vectorSize = size
	}

	// Initialize workflow engine
	svc.workflowEngine = NewWorkflowEngine(nil) // Tool executor set by daemon

	// Initialize knowledge graph with persistence
	svc.knowledgeGraph = NewKnowledgeGraph()
	svc.persistedKnowledgeGraph = svc.knowledgeGraph.SetPersistence(&GraphPersistenceConfig{
		EntitiesQdrant:  svc.qdrant.Get(CollGraphEntities),
		RelationsQdrant: svc.qdrant.Get(CollGraphRelations),
		EmbedModel:      cfg.EmbedModel,
		VectorSize:      svc.vectorSize,
	})

	// Initialize memory hierarchy with persistence
	svc.memoryHierarchy = NewMemoryHierarchy()
	svc.persistedMemoryHierarchy = svc.memoryHierarchy.SetPersistence(&MemoryPersistenceConfig{
		MemoryQdrant: svc.qdrant.Get(CollMemory),
		EmbedModel:   cfg.EmbedModel,
		VectorSize:   svc.vectorSize,
	})

	// Initialize workflow engine with persistence
	svc.persistedWorkflowEngine = svc.workflowEngine.SetPersistence(&WorkflowPersistenceConfig{
		WorkflowsQdrant:    svc.qdrant.Get(CollWorkflows),
		WorkflowDefsQdrant: svc.qdrant.Get(CollWorkflowDefs),
	})

	// Apply functional options and set defaults.
	for _, opt := range opts {
		opt(svc)
	}
	if svc.logger == nil {
		svc.logger = slog.Default()
	}
	if svc.tracer == nil {
		svc.tracer = noop.NewTracerProvider().Tracer("agentcontext")
	}
	svc.metrics = GetMetrics()

	// Recall reranker: default-off (NoopReranker) unless WEAVER_RERANKER is
	// set. A WithReranker option (tests) takes precedence over env config.
	if svc.reranker == nil {
		rcfg := LoadRerankerConfigFromEnv()
		svc.reranker = NewReranker(rcfg.Kind, rcfg, svc.logger)
		if rcfg.Kind != RerankerKindOff {
			svc.logger.Info("recall reranker enabled",
				"backend", svc.reranker.Backend(),
				"base_url", rcfg.BaseURL,
				"model", rcfg.Model)
		}
	}

	// Initialize domain sub-services
	svc.presence = NewPresenceSvc(qdrantReg.Get(CollPresence), cfg, svc.logger, svc.metrics)
	svc.claims = NewClaimSvc(qdrantReg.Get(CollFileClaims), svc.logger, svc.metrics)
	svc.worktrees = NewWorktreeSvc(qdrantReg.Get(CollWorktree), cfg, svc.logger, svc.metrics)
	svc.tasks = NewTaskSvc(qdrantReg.Get(CollTasks), svc.embed, cfg, svc.logger, &svc.vectorSize)
	// One shared fail-closed tracker across all write paths (context + tasks)
	// so the fallback ratio reflects the whole service.
	embedDegradation := NewEmbedDegradationTracker(embedDegradationConfigFromEnv())
	svc.tasks.embedDegradation = embedDegradation
	svc.tasks.metrics = svc.metrics
	svc.plans = NewPlanSvc(qdrantReg.Get(CollPlans), qdrantReg.Get(CollPlanSlices), svc.embed, &svc.vectorSize, svc.logger)
	svc.patterns = NewPatternSvc(qdrantReg.Get(CollPatterns), svc.embed, &svc.vectorSize, svc.logger)
	// Slice claims enforce the file boundary via the file-claim service.
	svc.plans.claimFiles = func(ctx context.Context, agentID, sessionID, reason string, files []string) []string {
		return svc.claims.AcquireEnforced(ctx, agentID, sessionID, reason, files)
	}

	// Wire cross-domain callbacks for presence cleanup
	svc.presence.releaseClaimsForAgent = func(agentID string) {
		svc.claims.ReleaseAllForAgent(agentID)
	}
	svc.presence.orphanWorktrees = svc.orphanWorktreesForAgent
	svc.presence.endSessionsForAgent = svc.endActiveSessionsForAgent
	svc.presence.detectConflicts = func(agentID string, files []string) []map[string]any {
		conflicts := svc.presence.DetectActiveFileConflicts(agentID, files)
		conflicts = append(conflicts, svc.claims.DetectConflicts(agentID, files)...)
		return conflicts
	}

	// Wire worktree ↔ presence callbacks
	svc.worktrees.setPresenceWorktreeID = svc.presence.SetWorktreeID
	svc.worktrees.clearPresenceWorktreeID = svc.presence.ClearWorktreeID

	// Initialize session sub-service
	svc.sess = NewSessionSvc(qdrantReg.Get(CollSessions), cfg, svc.logger, svc.metrics)

	// Allow Allocate to fall back to session.working_dir when no explicit
	// repo_path / cfg.GitRepoPath is available.
	svc.worktrees.getSession = svc.getSession

	// Wire session cleanup callbacks
	svc.sess.releaseClaimsForAgent = func(agentID string) int {
		return svc.claims.ReleaseAllForAgent(agentID)
	}
	svc.sess.removePresence = svc.presence.Remove
	svc.sess.deletePresenceFromQdrant = func(ctx context.Context, agentID string) error {
		if svc.qdrant.Get(CollPresence) == nil {
			return nil
		}
		return svc.qdrant.Get(CollPresence).DeleteByFilter(ctx, FilterMust(Match("agent_id", agentID)))
	}
	svc.sess.orphanWorktrees = svc.orphanWorktreesForAgent
	svc.sess.markTasksStale = svc.markSessionTasksStale
	svc.sess.enrichResult = svc.enrichSessionStartResult
	svc.sess.liveAgentIDs = svc.presence.LiveAgentIDs
	svc.sess.isPresenceStale = svc.presence.IsAgentStale

	// Initialize context sub-service
	svc.ctxSvc = NewContextSvc(svc.qdrant, svc.embed, &svc.vectorSize, cfg, svc.logger, svc.metrics)
	svc.ctxSvc.embedDegradation = embedDegradation
	svc.ctxSvc.persistedMemoryHierarchy = svc.persistedMemoryHierarchy
	svc.ctxSvc.knowledgeGraph = svc.knowledgeGraph
	svc.ctxSvc.getSession = svc.getSession
	svc.ctxSvc.addSessionEntryStats = func(session *Session, entries int, tokens int) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		svc.sess.IncrementStats(ctx, session, entries, tokens)
	}
	svc.ctxSvc.readSessionStats = func(session *Session) (int, int, *time.Time) {
		svc.sess.mu.RLock()
		defer svc.sess.mu.RUnlock()
		return session.EntryCount, session.TotalTokens, session.LastSummaryAt
	}
	svc.ctxSvc.markSessionSummarized = func(session *Session, t time.Time) {
		svc.sess.mu.Lock()
		session.LastSummaryAt = &t
		svc.sess.mu.Unlock()
	}

	// Wire context entry counter for session stats recomputation.
	// On post-restart recovery (stale sessions with EntryCount==0), scroll
	// entries to recompute accurate entry count and token totals.
	svc.sess.countContextEntries = func(ctx context.Context, sessionID string) (int, int) {
		client := svc.qdrant.Get(CollContext)
		if client == nil {
			return 0, 0
		}
		filter := FilterMust(Match("session_id", sessionID))
		points, err := client.ScrollPoints(ctx, filter, 500, false)
		if err != nil {
			count, _ := client.Count(ctx, filter)
			return count, 0
		}
		totalTokens := 0
		for _, p := range points {
			if tc, ok := p.Payload["token_count"]; ok {
				if n, ok := tc.(float64); ok {
					totalTokens += int(n)
				}
			}
		}
		return len(points), totalTokens
	}

	// Initialize phase-2 domain sub-services.
	svc.graph = &GraphSvc{Service: svc}
	svc.memory = &MemorySvc{Service: svc}
	svc.workflow = &WorkflowSvc{Service: svc}
	svc.sourceVersion = &SourceVersionSvc{Service: svc}
	svc.handoffs = &HandoffSvc{Service: svc}
	svc.messages = &MessageSvc{Service: svc}
	svc.vendorSess = &VendorSessionsSvc{Service: svc, store: vendorSessionStore(cfg)}
	svc.mrStatus = &MRStatusSvc{Service: svc}

	// Wire session summary callbacks to ContextSvc
	svc.sess.generateSummary = svc.ctxSvc.GenerateSummary
	svc.sess.runSummaryAsync = svc.runSessionSummaryAsync
	svc.sess.runRetroAsync = svc.runSessionRetroAsync

	// Wire task callbacks
	svc.tasks.getSession = svc.getSession
	svc.tasks.upsertBatched = svc.ctxSvc.upsertBatched

	// Initialize compaction scheduler
	compactionConfig := DefaultCompactionConfig()
	compactionConfig.Enabled = cfg.CompactionEnabled
	if cfg.CompactionCheckInterval > 0 {
		compactionConfig.CheckInterval = time.Duration(cfg.CompactionCheckInterval) * time.Second
	}
	svc.compactionScheduler = NewCompactionScheduler(compactionConfig, svc.memoryHierarchy, nil, svc.logger)
	svc.compactionScheduler.SetPersistence(svc.persistedMemoryHierarchy)

	// Initialize task reconciler
	reconcilerConfig := DefaultTaskReconcilerConfig()
	reconcilerConfig.Enabled = cfg.TaskReconcilerEnabled
	if cfg.TaskReconcilerInterval > 0 {
		reconcilerConfig.CheckInterval = time.Duration(cfg.TaskReconcilerInterval) * time.Second
	}
	if cfg.TaskReconcilerCompletedRetention > 0 {
		reconcilerConfig.CompletedRetention = time.Duration(cfg.TaskReconcilerCompletedRetention) * time.Hour
	}
	if cfg.TaskReconcilerStaleTimeout > 0 {
		reconcilerConfig.StaleTimeout = time.Duration(cfg.TaskReconcilerStaleTimeout) * time.Hour
	}
	svc.tasks.reconciler = NewTaskReconciler(reconcilerConfig, svc.tasks, svc.logger)
	svc.tasks.reconciler.getSession = svc.getSession

	// Initialize worktree reconciler (stored on WorktreeSvc)
	wtReconcilerConfig := DefaultWorktreeReconcilerConfig()
	wtReconcilerConfig.Enabled = cfg.WorktreeReconcilerEnabled
	if cfg.WorktreeReconcilerInterval > 0 {
		wtReconcilerConfig.CheckInterval = time.Duration(cfg.WorktreeReconcilerInterval) * time.Second
	}
	if cfg.WorktreeOrphanGracePeriod > 0 {
		wtReconcilerConfig.OrphanGracePeriod = time.Duration(cfg.WorktreeOrphanGracePeriod) * time.Minute
	}
	wtReconcilerConfig.MaxTTLHours = cfg.WorktreeMaxTTLHours
	wtReconcilerConfig.ArtifactCleanupEnabled = cfg.WorktreeArtifactCleanupEnabled
	if cfg.WorktreeArtifactCleanupPatterns != "" {
		wtReconcilerConfig.ArtifactPatterns = parseArtifactPatterns(cfg.WorktreeArtifactCleanupPatterns)
	}
	wtReconcilerConfig.DiskScanEnabled = cfg.WorktreeDiskScanEnabled
	wtReconcilerConfig.DetectUntracked = cfg.WorktreeDetectUntracked
	svc.worktrees.reconciler = NewWorktreeReconciler(wtReconcilerConfig, svc.worktrees, svc.logger)

	// Initialize the plan truth sweep. It reads merged state from the same HUD
	// base URL agent_mr_status uses; without one it stays inert.
	planReconcilerConfig := DefaultPlanReconcilerConfig()
	planReconcilerConfig.Enabled = cfg.PlanReconcilerEnabled
	if cfg.PlanReconcileInterval > 0 {
		planReconcilerConfig.CheckInterval = cfg.PlanReconcileInterval
	}
	svc.planReconciler = NewPlanReconciler(planReconcilerConfig, svc.plans, cfg.HUDBaseURL, svc.metrics, svc.logger)

	// Load persisted state on startup (best-effort)
	ctx := context.Background()
	if err := svc.loadPersistedState(ctx); err != nil {
		svc.logger.Warn("failed to load persisted state", "error", err)
	}

	return svc, nil
}

// loadPersistedState loads all persisted data from Qdrant on startup
func (s *Service) loadPersistedState(ctx context.Context) error {
	// Load sessions
	if err := s.sess.LoadFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load sessions", "error", err)
	}

	// Load knowledge graph
	if err := s.persistedKnowledgeGraph.LoadGraphFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load knowledge graph", "error", err)
	}
	if err := s.persistedKnowledgeGraph.LoadReasoningChainsFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load reasoning chains", "error", err)
	}

	// Load memory hierarchy
	if err := s.persistedMemoryHierarchy.LoadMemoryFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load memory hierarchy", "error", err)
	}

	// Load workflows and definitions
	if err := s.persistedWorkflowEngine.LoadWorkflowsFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load workflows", "error", err)
	}
	if err := s.persistedWorkflowEngine.LoadDefinitionsFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load workflow definitions", "error", err)
	}

	// Load presence registry
	if err := s.presence.LoadFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load presence", "error", err)
	}

	// Load file claims
	if err := s.claims.LoadFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load file claims", "error", err)
	}

	// Load worktree assignments
	if err := s.worktrees.LoadFromQdrant(ctx); err != nil {
		s.logger.Warn("failed to load worktree assignments", "error", err)
	}

	return nil
}

// StartBackgroundServices starts background goroutines (compaction, presence cleanup)
func (s *Service) StartBackgroundServices(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	s.bgCancel = cancel

	s.logger.Info("starting background services",
		"compaction_enabled", s.cfg.CompactionEnabled,
		"task_reconciler_enabled", s.cfg.TaskReconcilerEnabled,
		"worktree_reconciler_enabled", s.cfg.WorktreeReconcilerEnabled,
		"plan_reconciler_enabled", s.cfg.PlanReconcilerEnabled,
		"session_reaper_enabled", s.cfg.SessionReaperEnabled,
		"presence_cleanup_interval_s", s.cfg.PresenceCleanupInterval,
	)

	// Seed the builtin pattern catalog + composed engrams (idempotent, best-
	// effort, non-blocking). The engrams are the building blocks a green stamp
	// verifies (Slice A2), so the engram catalog is non-empty from startup.
	if s.patterns != nil {
		go s.patterns.SeedBuiltins(bgCtx)
		go s.seedBuiltinEngrams(bgCtx)
	}

	// Start compaction scheduler
	if s.compactionScheduler != nil && s.cfg.CompactionEnabled {
		if err := s.compactionScheduler.Start(bgCtx); err != nil {
			s.logger.Warn("failed to start compaction scheduler", "error", err)
		}
	}

	// Start task reconciler
	if s.cfg.TaskReconcilerEnabled {
		s.tasks.StartReconciler(bgCtx)
	}

	// Start worktree reconciler
	if s.cfg.WorktreeReconcilerEnabled {
		s.worktrees.StartReconciler(bgCtx)
	}

	// Start plan truth sweep (self-disables without a HUD base URL)
	s.planReconciler.Start(bgCtx)

	// Start presence cleanup goroutine
	go s.presence.RunCleanup(bgCtx)

	// Start session reaper
	if s.cfg.SessionReaperEnabled {
		s.logger.Info("starting session reaper",
			"interval_s", s.cfg.SessionReaperInterval,
			"max_age_hours", s.cfg.SessionReaperMaxAge,
		)
		go s.sess.RunReaper(bgCtx)
	}
}

// StopBackgroundServices stops all background goroutines
func (s *Service) StopBackgroundServices() {
	s.logger.Info("stopping background services")
	if s.compactionScheduler != nil {
		s.compactionScheduler.Stop()
	}
	s.tasks.StopReconciler()
	s.worktrees.StopReconciler()
	s.planReconciler.Stop()
	if s.bgCancel != nil {
		s.bgCancel()
	}
}

// ConflictBus returns the ClaimSvc's ConflictBus used for F9 live file-claim
// conflict overlay. Returns nil if claims are not initialized. Subscribers
// (e.g. HUD SSE handler) get a process-local channel of ClaimConflictEvent.
func (s *Service) ConflictBus() *ConflictBus {
	if s == nil || s.claims == nil {
		return DefaultConflictBus()
	}
	if s.claims.conflictBus == nil {
		return DefaultConflictBus()
	}
	return s.claims.conflictBus
}
