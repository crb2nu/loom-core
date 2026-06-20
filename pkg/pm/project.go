package pm

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/agentcontext"
)

// TaskBrief is the per-project task projection in pm_project_status, mirroring
// the flexdeck /api/projects/<id> "tasks" shape so an agent sees the same view
// the PM page shows.
type TaskBrief struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Priority  string `json:"priority,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// DecisionBrief is the per-project decision projection (agent-context decision
// journal entries with entry_type=decision).
type DecisionBrief struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	DecidedAt string `json:"decided_at,omitempty"`
}

// ProjectReader federates read-only project data mcp-pm does NOT own — agent
// tasks and the decision journal, both living in agent-context's Qdrant
// collections. It exists so pm_project_status can present the unified rollup the
// flexdeck /projects page shows, but callable by an agent over MCP in one call.
type ProjectReader interface {
	TasksByProject(ctx context.Context, project string) ([]TaskBrief, error)
	DecisionsByProject(ctx context.Context, project string) ([]DecisionBrief, error)
}

// ProjectStatusResult is the pm_project_status payload.
type ProjectStatusResult struct {
	Project    string `json:"project"`
	OpenTasks  int    `json:"open_tasks"`
	InProgress int    `json:"in_progress"`
	Blocked    int    `json:"blocked"`
	OpenRisks  int    `json:"open_risks"`

	Tasks     []TaskBrief     `json:"tasks"`
	Risks     []Risk          `json:"risks"`
	Decisions []DecisionBrief `json:"decisions"`

	// Partial is set when a federated source failed; PartialReasons names which.
	// One dead source never fails the call — it yields an empty slice + a flag,
	// mirroring the flexdeck federation's per-source error isolation.
	Partial        bool     `json:"partial"`
	PartialReasons []string `json:"partial_reasons,omitempty"`
}

// ProjectStatus returns the unified per-project rollup: open agent tasks, open
// risks (pm_risks), and recent decisions (agent-context journal), correlated by
// the shared `project` key (GitLab path_with_namespace). Each source is
// error-isolated.
func (s *Service) ProjectStatus(ctx context.Context, project string) (*ProjectStatusResult, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	res := &ProjectStatusResult{
		Project:   project,
		Tasks:     []TaskBrief{},
		Risks:     []Risk{},
		Decisions: []DecisionBrief{},
	}

	// Risks — owned by mcp-pm.
	if err := s.store.EnsureReady(ctx); err != nil {
		res.markPartial("risks", err)
	} else if risks, err := s.store.List(ctx, project, "", 0); err != nil {
		res.markPartial("risks", err)
	} else {
		res.Risks = risks
		for _, r := range risks {
			if !strings.EqualFold(r.Status, StatusClosed) {
				res.OpenRisks++
			}
		}
	}

	// Tasks + decisions — federated read-only from agent-context.
	if s.reader == nil {
		res.markPartial("tasks/decisions", fmt.Errorf("reader not configured"))
		return res, nil
	}
	if tasks, err := s.reader.TasksByProject(ctx, project); err != nil {
		res.markPartial("tasks", err)
	} else {
		for _, t := range tasks {
			switch strings.ToLower(strings.TrimSpace(t.Status)) {
			case "completed", "cancelled", "done", "":
				// terminal/unknown — not an open task
			default:
				res.Tasks = append(res.Tasks, t)
				res.OpenTasks++
				if strings.EqualFold(t.Status, "in_progress") {
					res.InProgress++
				}
				if strings.EqualFold(t.Status, "blocked") {
					res.Blocked++
				}
			}
		}
	}
	if decisions, err := s.reader.DecisionsByProject(ctx, project); err != nil {
		res.markPartial("decisions", err)
	} else {
		res.Decisions = decisions
	}
	return res, nil
}

func (r *ProjectStatusResult) markPartial(source string, err error) {
	r.Partial = true
	r.PartialReasons = append(r.PartialReasons, source+": "+err.Error())
}

// QdrantProjectReader is the production ProjectReader: read-only scrolls of the
// agent-context tasks/context collections by `project`. It reuses the same
// QdrantClient type and shared connection config as the risks store.
type QdrantProjectReader struct {
	tasks         *agentcontext.QdrantClient
	context       *agentcontext.QdrantClient
	taskLimit     int
	decisionLimit int
}

// NewQdrantProjectReader builds the production reader from config.
func NewQdrantProjectReader(cfg Config) *QdrantProjectReader {
	hc := httpClient()
	return &QdrantProjectReader{
		tasks:         agentcontext.NewQdrantClient(hc, cfg.QdrantURL, cfg.QdrantAPIKey, cfg.TasksCollection, cfg.QdrantDistance),
		context:       agentcontext.NewQdrantClient(hc, cfg.QdrantURL, cfg.QdrantAPIKey, cfg.ContextCollection, cfg.QdrantDistance),
		taskLimit:     200,
		decisionLimit: 50,
	}
}

func (r *QdrantProjectReader) TasksByProject(ctx context.Context, project string) ([]TaskBrief, error) {
	filter := agentcontext.FilterMust(agentcontext.Match("project", project))
	pts, err := r.tasks.ScrollPoints(ctx, filter, r.taskLimit, false)
	if err != nil {
		return nil, err
	}
	out := make([]TaskBrief, 0, len(pts))
	for _, p := range pts {
		if len(p.Payload) == 0 {
			continue
		}
		out = append(out, TaskBrief{
			ID:        payloadString(p.Payload, "id"),
			Title:     payloadString(p.Payload, "title"),
			Status:    payloadString(p.Payload, "status"),
			Priority:  payloadString(p.Payload, "priority"),
			SessionID: payloadString(p.Payload, "session_id"),
		})
	}
	return out, nil
}

func (r *QdrantProjectReader) DecisionsByProject(ctx context.Context, project string) ([]DecisionBrief, error) {
	filter := agentcontext.FilterMust(
		agentcontext.Match("project", project),
		agentcontext.Match("entry_type", "decision"),
	)
	pts, err := r.context.ScrollPoints(ctx, filter, r.decisionLimit, false)
	if err != nil {
		return nil, err
	}
	out := make([]DecisionBrief, 0, len(pts))
	for _, p := range pts {
		if len(p.Payload) == 0 {
			continue
		}
		out = append(out, DecisionBrief{
			ID:        payloadString(p.Payload, "id"),
			Title:     payloadString(p.Payload, "title"),
			DecidedAt: payloadString(p.Payload, "timestamp"),
		})
	}
	return out, nil
}

// payloadString safely extracts a string from a Qdrant payload map.
func payloadString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
