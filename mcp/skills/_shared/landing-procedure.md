# Shared Workflow Patterns

Canonical reference for the standardized patterns embedded in workflow skills.
Not consumed by the generator — this is documentation for contributors.

**This file is the single source of truth for the auto-ship tail**
(commit → push → MR → auto-merge → CI poll → cleanup). Workflow skills in
`mcp/context/skills-registry.yaml` embed only a compact self-sufficient stanza
pointing here; `commit` and `ci-green-merge-loop` carry the full detail because
they ARE the landing skills. When the procedure changes, update this file first,
then the compact stanzas and the two detailed skills.

## Landing Procedure

The canonical end-to-end shipping sequence. Workflow skills that produce code
changes embed a compact version of this as their closing steps.

```markdown
## Landing Procedure

1. **Stage scoped files** — only files for the active task (never `.env`, credentials, or unrelated changes)
2. **Commit with conventional format**
   - `type(scope): description` (e.g., `feat(auth): add OAuth2 PKCE flow`)
   - Include `Co-Authored-By: <agent> <noreply@anthropic.com>` trailer
   - Use a HEREDOC for multi-line messages
3. **Push the branch** — `git push origin <branch>` (with `-u` if first push); never force-push without explicit user approval
4. **Create or reuse MR/PR**
   - Check for existing MR on the same source branch before creating a new one
   - GitLab: `gitlab__list_merge_requests(source_branch=..., target_branch=main)`
   - GitHub: `gh pr list --head <branch>`
5. **Request auto-merge** (when project policy allows)
   - GitLab: `gitlab__merge_merge_request(auto_merge=true, sha=<HEAD>)`
   - GitHub: `gh pr merge --auto --squash`
   - **Serial merge queue**: when the environment sets `MILLS_MERGE_QUEUE_URL`,
     the GitLab tool routes the merge through the Mills serial merge queue
     instead of merging directly — the result says
     `"merge request queued; poll GitLab for completion"`. Treat that as
     success: the queue rebases the MR onto the exact target tip, re-proves it
     with a fresh pipeline, and merges it in turn. Keep polling the MR until
     `merged`. Never work around a queued result with a direct API merge or
     push — that breaks the serialization every other agent is relying on. A
     `MERGE_QUEUE_REJECTED` full-lane response means back off and retry after
     the queue drains; only a policy-disabled queue falls back to the direct
     merge automatically.
6. **Poll pipeline to terminal state**
   - GitLab: `gitlab__poll_pipeline` or `gitlab__pipeline_summary`
   - GitHub: `gh run watch`
   - If red: fix → re-commit → re-push → re-poll
7. **Confirm merged** or document the blocker with exact next action
8. **Release worktree** (if allocated): `agent_worktree_release(...)`
9. **End session**: `agent_session_end(summarize=true)`
```

## Session Bookends

Wrap implementation work in context tracking so it is resumable and handoff-ready.

```markdown
## Session Bookends

**Open:**
- `agent_session_start(namespace="<repo>/<workflow>", description="<task>")`
- `agent_recall(query="<task topic>", scope="context")`
- `agent_presence_register(agent_id="<id>", agent_type="<platform>")`

**Close:**
- `agent_context_add(entries=[{entry_type: "decision"|"finding", ...}])`
- `agent_task_update(..., status="completed", resolution="...")`
- `agent_session_end(summarize=true)`
```

## Worktree Allocation

Isolate multi-file work in a disposable git worktree.

```markdown
## Worktree Allocation

- `agent_worktree_allocate(agent_id=<id>, session_id=<session>, branch_name="<prefix>/<name>", base_branch="main", purpose="<description>")`
- Work exclusively in the returned `worktree_path`
- The worktree is cleaned up when the session ends (TTL/orphan reconciler)
- Quick single-file fixes on main are acceptable without a worktree
```

### Branch Naming Convention

| Skill Type | Prefix | Example |
|------------|--------|---------|
| Feature | `feat/` | `feat/user-auth` |
| Bug fix | `fix/` | `fix/login-timeout` |
| Dependency | `upgrade/` | `upgrade/react-19` |
| Refactor | `refactor/` | `refactor/auth-module` |
| Backlog | `backlog/` | `backlog/PROJ-123` |
| Tech debt | `debt/` | `debt/TD-45` |
| Docs | `docs/` | `docs/api-reference` |

## CI Verification

Verify pipeline status using MCP tools rather than manual polling.

```markdown
## CI Verification

1. Find the latest pipeline for the pushed branch
   - GitLab: `gitlab__list_pipelines` + `gitlab__pipeline_summary`
   - GitHub: inspect PR checks or `gh run list`
2. Poll to terminal state
   - GitLab: `gitlab__poll_pipeline`
   - GitHub: `gh run watch`
3. If failed:
   - Inspect logs: `gitlab__get_job_trace` / `gitlab__get_test_report`
   - Implement minimal fix
   - Rerun local verification
   - Push and re-poll
4. If green: confirm MR/PR is merged or queued
5. If blocked by external dependency: update task status to `blocked` with concrete details
```

## Platform-Specific Patterns

### Claude Code
- Use `Task` tool with `isolation: "worktree"` for parallel sub-work
- Delegate CI polling to a background `Task` agent when waiting
- Use `subagent_type="general-purpose"` for implementation, `"Plan"` for design

### Codex
- Use `multi_tool_use.parallel` for concurrent inventory/research calls
- Bundled scripts available at `${SKILL_PATH}/scripts/`
- For parallel slices, use `spawn_agent` with `max_threads` config

### Kilocode
- Map workflows to `.kilocode/workflows/<name>.yaml`
- Use staged phases with checkpoints between major steps
- Checkpoint between phases for partial-run resumption

### Gemini
- Use checkpointed phased decomposition for multi-step workflows
- Record progress markers between major steps for resumability
- Execute slices sequentially with explicit validation between each

### Antigravity
- Follow AGENTS.md instructions for workflow guidance
- Use agent-context MCP tools for session tracking and handoffs
- Leverage VS Code integrated terminal for local verification
