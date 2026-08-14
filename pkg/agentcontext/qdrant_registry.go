package agentcontext

import "github.com/crb2nu/loom/pkg/httpclient"

// Collection name constants used as keys in QdrantRegistry.
//
// SIMP-12 consolidation: annotations merged into the context collection
// (CollAnnotations is an alias), and the templates collection was dropped.
// 16 active collections — one client per key in NewQdrantRegistry below.
// Keep this count in sync with that map.
const (
	CollContext        = "context"
	CollSessions       = "sessions"
	CollTasks          = "tasks"
	CollHandoffs       = "handoffs"
	CollMessages       = "messages"
	CollGraphEntities  = "graphEntities"
	CollGraphRelations = "graphRelations"
	CollWorkflows      = "workflows"
	CollWorkflowDefs   = "workflowDefs"
	CollMemory         = "memory"
	CollPresence       = "presence"
	CollFileClaims     = "fileClaims"
	CollWorktree       = "worktree"
	CollPlans          = "plans"
	CollPlanSlices     = "planSlices"
	CollPatterns       = "patterns"

	// CollAnnotations is an alias for CollContext.
	// Annotations now share the context collection with a _record_type discriminator.
	CollAnnotations = CollContext
)

// QdrantRegistry manages a set of named QdrantClient instances, one per collection.
type QdrantRegistry struct {
	clients map[string]*QdrantClient
}

// NewQdrantRegistry creates a QdrantRegistry with all active collection clients:
// 16 collections (annotations share CollContext; there is no templates collection).
func NewQdrantRegistry(hc *httpclient.Client, cfg Config) *QdrantRegistry {
	mk := func(kind, collection string) *QdrantClient {
		c := NewQdrantClient(hc, cfg.QdrantURL, cfg.QdrantAPIKey, collection, cfg.QdrantDistance)
		c.SetKind(kind)
		return c
	}

	contextClient := mk(CollContext, cfg.ContextCollection)

	return &QdrantRegistry{
		clients: map[string]*QdrantClient{
			CollContext:        contextClient,
			CollSessions:       mk(CollSessions, cfg.SessionsCollection),
			CollTasks:          mk(CollTasks, cfg.TasksCollection),
			CollHandoffs:       mk(CollHandoffs, cfg.HandoffsCollection),
			CollMessages:       mk(CollMessages, cfg.MessagesCollection),
			CollGraphEntities:  mk(CollGraphEntities, cfg.GraphEntitiesCollection),
			CollGraphRelations: mk(CollGraphRelations, cfg.GraphRelationsCollection),
			CollWorkflows:      mk(CollWorkflows, cfg.WorkflowsCollection),
			CollWorkflowDefs:   mk(CollWorkflowDefs, cfg.WorkflowDefsCollection),
			CollMemory:         mk(CollMemory, cfg.MemoryCollection),
			CollPresence:       mk(CollPresence, cfg.PresenceCollection),
			CollFileClaims:     mk(CollFileClaims, cfg.FileClaimsCollection),
			CollWorktree:       mk(CollWorktree, cfg.WorktreeCollection),
			CollPlans:          mk(CollPlans, cfg.PlansCollection),
			CollPlanSlices:     mk(CollPlanSlices, cfg.PlanSlicesCollection),
			CollPatterns:       mk(CollPatterns, cfg.PatternsCollection),
		},
	}
}

// Get returns the QdrantClient for the named collection, or nil if not found.
func (r *QdrantRegistry) Get(name string) *QdrantClient {
	if r == nil {
		return nil
	}
	return r.clients[name]
}

// Names returns all registered collection names.
func (r *QdrantRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.clients))
	for k := range r.clients {
		names = append(names, k)
	}
	return names
}
