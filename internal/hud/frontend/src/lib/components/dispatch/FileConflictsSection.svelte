<script lang="ts">
  import { mergeQueueStore } from '../../stores/mergeQueue.svelte.ts';
  import { presenceActionsStore } from '../../stores/presenceActions.svelte.ts';
  import EmptyState from '../shared/EmptyState.svelte';

  let { collapsed = $bindable(false) }: { collapsed?: boolean } = $props();

  let conflicts = $derived(mergeQueueStore.conflicts);

  function nudgeAgent(agentId: string, context: string) {
    presenceActionsStore.nudgeContent = context;
    presenceActionsStore.onOpenNudge(agentId);
  }

  function releaseConflict(agentId: string, files: string[]) {
    for (const file of files) {
      presenceActionsStore.onReleaseClaim(agentId, file);
    }
  }

  function releaseFile(agentId: string, file: string) {
    presenceActionsStore.onReleaseClaim(agentId, file);
  }
</script>

<section class="dispatch-section">
  <div class="section-head">
    <button class="section-toggle" onclick={() => collapsed = !collapsed}>
      <span class="toggle-icon">{collapsed ? '\u25B6' : '\u25BC'}</span>
      <h3 class="section-title">File conflicts</h3>
      <span class="section-count">{conflicts.length}</span>
    </button>
    <div class="section-subtitle">
      Merge-blocking file claim overlaps between agents
    </div>
  </div>

  {#if !collapsed}
    {#if conflicts.length > 0}
      <div class="conflict-list">
        {#each conflicts as conflict, i (conflict.left_agent + conflict.right_agent + i)}
          <div class="conflict-card">
            <div class="conflict-agents">
              <span class="agent-badge left">{conflict.left_agent}</span>
              <span class="conflict-vs">vs</span>
              <span class="agent-badge right">{conflict.right_agent}</span>
              <span class="conflict-type-tag">{conflict.conflict_type}</span>
            </div>
            {#if conflict.files?.length}
              <div class="conflict-files">
                {#each conflict.files as file}
                  <div class="conflict-file">
                    <span class="conflict-file-path">{file}</span>
                    <span class="conflict-file-actions">
                      <button
                        type="button"
                        class="file-release-btn file-release-left"
                        title={`Release ${file} from ${conflict.left_agent}`}
                        aria-label={`Release ${file} from ${conflict.left_agent}`}
                        onclick={() => releaseFile(conflict.left_agent, file)}
                      >
                        {'←'} L
                      </button>
                      <button
                        type="button"
                        class="file-release-btn file-release-right"
                        title={`Release ${file} from ${conflict.right_agent}`}
                        aria-label={`Release ${file} from ${conflict.right_agent}`}
                        onclick={() => releaseFile(conflict.right_agent, file)}
                      >
                        R {'→'}
                      </button>
                    </span>
                  </div>
                {/each}
              </div>
            {/if}
            {#if conflict.detail}
              <div class="conflict-detail">{conflict.detail}</div>
            {/if}
            <div class="conflict-actions">
              <button
                class="btn btn-sm btn-nudge"
                onclick={() => nudgeAgent(conflict.left_agent, `File conflict with ${conflict.right_agent}: ${conflict.files?.join(', ') || 'multiple files'}`)}
              >
                Nudge {conflict.left_agent}
              </button>
              <button
                class="btn btn-sm btn-nudge"
                onclick={() => nudgeAgent(conflict.right_agent, `File conflict with ${conflict.left_agent}: ${conflict.files?.join(', ') || 'multiple files'}`)}
              >
                Nudge {conflict.right_agent}
              </button>
              {#if conflict.files?.length}
                <button
                  class="btn btn-sm btn-release"
                  onclick={() => releaseConflict(conflict.left_agent, conflict.files ?? [])}
                >
                  Release left
                </button>
                <button
                  class="btn btn-sm btn-release"
                  onclick={() => releaseConflict(conflict.right_agent, conflict.files ?? [])}
                >
                  Release right
                </button>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    {:else}
      <EmptyState
        icon={'\u2713'}
        heading="No file conflicts"
        description="No agents are claiming overlapping files. Merge paths are clear."
        compact
      />
    {/if}
  {/if}
</section>

<style>
  .conflict-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .conflict-card {
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: var(--bg-tertiary);
  }

  .conflict-agents {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-1);
  }

  .agent-badge {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 600;
    padding: 1px 6px;
    border-radius: var(--radius-sm);
  }

  .agent-badge.left {
    background: rgba(var(--accent-rgb), 0.15);
    color: var(--accent);
  }

  .agent-badge.right {
    background: rgba(var(--warning-rgb), 0.15);
    color: var(--warning);
  }

  .conflict-vs {
    font-size: 10px;
    color: var(--fg-muted);
    font-weight: 600;
    text-transform: uppercase;
  }

  .conflict-type-tag {
    font-size: 9px;
    padding: 1px 4px;
    border-radius: 2px;
    background: rgba(var(--error-rgb), 0.15);
    color: var(--error);
    margin-left: auto;
  }

  .conflict-files {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin: var(--space-1) 0;
  }

  .conflict-file {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    padding-left: var(--space-2);
  }

  .conflict-file-path {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .conflict-file-actions {
    display: inline-flex;
    gap: 4px;
    flex-shrink: 0;
  }

  .file-release-btn {
    font-family: var(--font-mono);
    font-size: 9px;
    font-weight: 600;
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    border: 1px solid transparent;
    background: transparent;
    transition: background var(--transition-fast), border-color var(--transition-fast);
  }

  .file-release-btn.file-release-left {
    color: var(--accent);
    border-color: rgba(var(--accent-rgb), 0.4);
  }

  .file-release-btn.file-release-left:hover {
    background: rgba(var(--accent-rgb), 0.15);
  }

  .file-release-btn.file-release-right {
    color: var(--warning);
    border-color: rgba(var(--warning-rgb), 0.4);
  }

  .file-release-btn.file-release-right:hover {
    background: rgba(var(--warning-rgb), 0.15);
  }

  .conflict-detail {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    margin: var(--space-1) 0;
  }

  .conflict-actions {
    display: flex;
    gap: var(--space-1);
    margin-top: var(--space-2);
  }

  .btn-nudge {
    font-size: 10px;
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    border: 1px solid var(--accent);
    background: transparent;
    color: var(--accent);
    transition: background var(--transition-fast);
  }

  .btn-nudge:hover {
    background: var(--accent);
    color: var(--bg-primary);
  }

  .btn-release {
    font-size: 10px;
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    border: 1px solid var(--error);
    background: transparent;
    color: var(--error);
    transition: background var(--transition-fast);
  }

  .btn-release:hover {
    background: rgba(var(--error-rgb), 0.15);
  }

  .section-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    background: none;
    border: none;
    cursor: pointer;
    padding: 0;
    color: inherit;
  }

  .toggle-icon {
    font-size: 10px;
    color: var(--fg-muted);
    width: 12px;
  }

  .section-toggle .section-title {
    margin: 0;
  }

  .section-count {
    font-family: var(--font-mono);
    font-size: 10px;
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    padding: 0 5px;
    border-radius: var(--radius-lg);
  }
</style>
