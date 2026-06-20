package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// taskSyncRequest is the JSON body for POST /api/agent/task-sync.
type taskSyncRequest struct {
	AgentID   string         `json:"agent_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// handleAgentTaskSync bridges native Claude Code task tools (TaskCreate,
// TaskUpdate, TodoWrite) into the agent-context task system.
func (d *FleetDomain) handleAgentTaskSync(w http.ResponseWriter, r *http.Request) {
	var req taskSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "agent_id is required", nil)
		return
	}
	if strings.TrimSpace(req.ToolName) == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "tool_name is required", nil)
		return
	}

	agent := d.deps.Agent()
	if agent == nil {
		d.deps.WriteError(w, http.StatusBadGateway, "agent bridge unavailable", nil)
		return
	}

	// Resolve the active session for this agent.
	session, err := agent.GetActiveSession(req.AgentID)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadGateway, "failed to get active session", err)
		return
	}
	if session == nil {
		d.deps.WriteError(w, http.StatusBadRequest, "no active session for agent", nil)
		return
	}

	var syncedIDs []string

	switch req.ToolName {
	case "TaskCreate":
		ids, err := d.syncTaskCreate(agent, session.ID, req.ToolInput)
		if err != nil {
			d.deps.WriteError(w, http.StatusBadGateway, "failed to sync TaskCreate", err)
			return
		}
		syncedIDs = ids

	case "TaskUpdate":
		if err := d.syncTaskUpdate(agent, req.ToolInput); err != nil {
			d.deps.WriteError(w, http.StatusBadGateway, "failed to sync TaskUpdate", err)
			return
		}

	case "TodoWrite":
		ids, err := d.syncTodoWrite(agent, session.ID, req.ToolInput)
		if err != nil {
			d.deps.WriteError(w, http.StatusBadGateway, "failed to sync TodoWrite", err)
			return
		}
		syncedIDs = ids

	case "update_plan":
		// Codex's native plan/todo tool (parity with Claude's TodoWrite).
		ids, err := d.syncUpdatePlan(agent, session.ID, req.ToolInput)
		if err != nil {
			d.deps.WriteError(w, http.StatusBadGateway, "failed to sync update_plan", err)
			return
		}
		syncedIDs = ids

	default:
		d.deps.WriteError(w, http.StatusBadRequest, fmt.Sprintf("unsupported tool_name: %s", req.ToolName), nil)
		return
	}

	d.deps.BroadcastAgentEvent("agent.task.sync", map[string]any{
		"agent_id":   req.AgentID,
		"tool_name":  req.ToolName,
		"session_id": session.ID,
		"synced_ids": syncedIDs,
	})

	go d.deps.FleetRefresh()

	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"tool_name":  req.ToolName,
		"synced_ids": syncedIDs,
	})
}

// syncTaskCreate maps a Claude Code TaskCreate tool invocation to an
// agent-context task creation.
func (d *FleetDomain) syncTaskCreate(agent *bridge.AgentBridge, sessionID string, input map[string]any) ([]string, error) {
	title := stringFromMap(input, "subject")
	if title == "" {
		title = stringFromMap(input, "title")
	}
	if title == "" {
		title = "Untitled task"
	}
	description := stringFromMap(input, "description")

	result, err := agent.CreateTask(bridge.CreateTaskParams{
		SessionID: sessionID,
		Title:     title,
		Context:   description,
		Tags:      []string{"native-sync"},
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.TaskIDs, nil
}

// syncTaskUpdate maps a Claude Code TaskUpdate tool invocation to an
// agent-context task update.
func (d *FleetDomain) syncTaskUpdate(agent *bridge.AgentBridge, input map[string]any) error {
	taskID := stringFromMap(input, "id")
	if taskID == "" {
		taskID = stringFromMap(input, "task_id")
	}
	if taskID == "" {
		return fmt.Errorf("task id not found in tool_input")
	}

	status := stringFromMap(input, "status")
	description := stringFromMap(input, "description")

	return agent.UpdateTask(bridge.UpdateTaskParams{
		ID:         taskID,
		Status:     status,
		Resolution: description,
	})
}

// syncTodoWrite maps a Claude Code TodoWrite tool invocation to agent-context
// task creations. TodoWrite provides a batch of todo items; each becomes a task.
func (d *FleetDomain) syncTodoWrite(agent *bridge.AgentBridge, sessionID string, input map[string]any) ([]string, error) {
	// TodoWrite provides "todos" as an array of objects.
	todosRaw, ok := input["todos"]
	if !ok {
		// Fallback: single item mode.
		title := stringFromMap(input, "content")
		if title == "" {
			title = stringFromMap(input, "text")
		}
		if title == "" {
			return nil, nil
		}
		result, err := agent.CreateTask(bridge.CreateTaskParams{
			SessionID: sessionID,
			Title:     title,
			Tags:      []string{"native-sync", "todo"},
		})
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, nil
		}
		return result.TaskIDs, nil
	}

	todos, ok := todosRaw.([]any)
	if !ok {
		return nil, nil
	}

	var allIDs []string
	for _, item := range todos {
		todo, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content := stringFromMap(todo, "content")
		if content == "" {
			content = stringFromMap(todo, "text")
		}
		if content == "" {
			continue
		}

		status := stringFromMap(todo, "status")
		// Only sync pending/in-progress items (skip completed).
		if status == "completed" || status == "done" {
			continue
		}

		result, err := agent.CreateTask(bridge.CreateTaskParams{
			SessionID: sessionID,
			Title:     content,
			Tags:      []string{"native-sync", "todo"},
		})
		if err != nil {
			d.deps.Logger().Warn("task-sync: failed to create todo task", "content", content, "error", err)
			continue
		}
		if result != nil {
			allIDs = append(allIDs, result.TaskIDs...)
		}
	}
	return allIDs, nil
}

// stringFromMap safely extracts a string from a map[string]any.
func stringFromMap(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// syncUpdatePlan maps a Codex `update_plan` tool invocation to agent-context
// tasks — the Codex counterpart of syncTodoWrite. Codex's update_plan input is
// {explanation, plan:[{step, status}]} with status ∈ pending|in_progress|completed.
//
// update_plan resends the WHOLE evolving plan on every call, so this reconciles
// by (session, normalized title): existing plan-sourced tasks get a status
// update when it changed, and only genuinely-new steps are created. This avoids
// the duplicate-per-call explosion the Claude TodoWrite path is prone to (it
// re-creates every todo on each TodoWrite, and task IDs embed a timestamp so
// they never collapse).
func (d *FleetDomain) syncUpdatePlan(agent *bridge.AgentBridge, sessionID string, input map[string]any) ([]string, error) {
	planRaw, ok := input["plan"].([]any)
	if !ok {
		return nil, nil
	}

	// Index existing plan-sourced tasks for this session by normalized title.
	existing := map[string]bridge.TaskInfo{}
	if tasks, err := agent.Tasks(sessionID); err == nil {
		for _, t := range tasks {
			if hasTag(t.Tags, "codex-plan") {
				existing[normalizeTitle(t.Title)] = t
			}
		}
	}

	var syncedIDs []string
	for _, raw := range planRaw {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title := stringFromMap(step, "step")
		if title == "" {
			continue
		}
		status := mapPlanStatus(stringFromMap(step, "status"))

		if prev, found := existing[normalizeTitle(title)]; found {
			if status != "" && !strings.EqualFold(prev.Status, status) {
				if err := agent.UpdateTask(bridge.UpdateTaskParams{ID: prev.ID, Status: status}); err != nil {
					d.deps.Logger().Warn("task-sync: update_plan status update failed", "title", title, "error", err)
					continue
				}
			}
			syncedIDs = append(syncedIDs, prev.ID)
			continue
		}

		// A genuinely new step. Skip steps that are already completed — there is
		// nothing left to track.
		if status == "completed" {
			continue
		}
		result, err := agent.CreateTask(bridge.CreateTaskParams{
			SessionID: sessionID,
			Title:     title,
			Tags:      []string{"native-sync", "codex-plan"},
		})
		if err != nil {
			d.deps.Logger().Warn("task-sync: update_plan create failed", "title", title, "error", err)
			continue
		}
		if result == nil {
			continue
		}
		syncedIDs = append(syncedIDs, result.TaskIDs...)
		// CreateTask defaults to pending; promote in-progress steps.
		if status == "in_progress" {
			for _, id := range result.TaskIDs {
				if err := agent.UpdateTask(bridge.UpdateTaskParams{ID: id, Status: "in_progress"}); err != nil {
					d.deps.Logger().Warn("task-sync: update_plan promote-in-progress failed", "title", title, "error", err)
				}
			}
		}
	}
	return syncedIDs, nil
}

// mapPlanStatus normalizes a Codex update_plan status to an agent-context task
// status. Codex uses pending|in_progress|completed, which map 1:1.
func mapPlanStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "in_progress":
		return "in_progress"
	case "completed", "done":
		return "completed"
	case "pending", "":
		return "pending"
	default:
		return "pending"
	}
}

// normalizeTitle lowercases and trims a title for stable reconcile matching.
func normalizeTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// hasTag reports whether tag is present in tags (case-insensitive).
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}
