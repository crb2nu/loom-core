import { actionStore } from './action.svelte.ts';
import { fleetStore } from './fleet.svelte.ts';
import { toastStore } from './toasts.svelte.ts';
import {
  acceptHandoff,
  createHandoff,
  dispatchTask,
  fetchHandoffs,
  releaseClaim,
  sendNudge,
  type HandoffRecord,
} from '../clients/presenceActions.ts';

function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message || 'Unknown error';
  if (typeof e === 'string') return e;
  try { return JSON.stringify(e); } catch { return 'Unknown error'; }
}

class PresenceActionsStore {
  handoffs = $state<HandoffRecord[]>([]);
  handoffLoading = $state(false);
  handoffError = $state('');

  showHandoffModal = $state(false);
  newHandoffTo = $state('');
  newHandoffSummary = $state('');
  newHandoffContext = $state('');
  newHandoffType = $state<'full' | 'selective' | 'summary_only'>('summary_only');
  newHandoffEntryIds = $state<string[]>([]);
  newHandoffTokenBudget = $state(0);
  creatingHandoff = $state(false);

  showDispatchModal = $state(false);
  dispatchTargetAgent = $state('');
  dispatchTitle = $state('');
  dispatchContext = $state('');
  dispatchPriority = $state('medium');
  dispatchSubmitting = $state(false);

  showNudgeModal = $state(false);
  nudgeTargetAgent = $state('');
  nudgeType = $state('message');
  nudgeContent = $state('');
  nudgeSubmitting = $state(false);

  async refreshHandoffs(): Promise<void> {
    this.handoffLoading = true;
    this.handoffError = '';

    try {
      this.handoffs = await fetchHandoffs();
    } catch {
      this.handoffError = 'Failed to load handoffs';
    }

    this.handoffLoading = false;
  }

  openHandoffModal(): void {
    this.showHandoffModal = true;
  }

  closeHandoffModal(): void {
    this.showHandoffModal = false;
  }

  async submitHandoff(): Promise<void> {
    if (!this.newHandoffTo.trim() || !this.newHandoffSummary.trim()) return;

    this.creatingHandoff = true;
    const auditId = actionStore.start('Create handoff', 'PresencePanel:handoff/create');
    try {
      const payload: Parameters<typeof createHandoff>[0] = {
        target_agent_id: this.newHandoffTo.trim(),
        instructions: this.newHandoffContext.trim()
          ? `${this.newHandoffSummary.trim()}\n\n${this.newHandoffContext.trim()}`
          : this.newHandoffSummary.trim(),
        handoff_type: this.newHandoffType,
      };
      if (this.newHandoffType === 'selective' && this.newHandoffEntryIds.length > 0) {
        payload.entry_ids = this.newHandoffEntryIds;
      }
      if (this.newHandoffType === 'full' && this.newHandoffTokenBudget > 0) {
        payload.token_budget = this.newHandoffTokenBudget;
      }
      await createHandoff(payload);
      actionStore.succeed(auditId);
      toastStore.success('Handoff created');
      this.showHandoffModal = false;
      this.newHandoffTo = '';
      this.newHandoffSummary = '';
      this.newHandoffContext = '';
      this.newHandoffType = 'summary_only';
      this.newHandoffEntryIds = [];
      this.newHandoffTokenBudget = 0;
      await this.refreshHandoffs();
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      toastStore.error('Failed to create handoff');
    } finally {
      this.creatingHandoff = false;
    }
  }

  async onAcceptHandoff(id: string, targetAgentID: string): Promise<void> {
    if (!targetAgentID.trim()) {
      toastStore.error('Cannot accept handoff without a target agent');
      return;
    }
    const auditId = actionStore.start('Accept handoff', 'PresencePanel:handoff/accept');
    try {
      await acceptHandoff(id, { target_agent_id: targetAgentID.trim() });
      actionStore.succeed(auditId);
      toastStore.success('Handoff accepted');
      await this.refreshHandoffs();
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      toastStore.error('Failed to accept handoff');
    }
  }

  onOpenDispatch(agentId: string): void {
    this.dispatchTargetAgent = agentId;
    this.showDispatchModal = true;
  }

  onOpenDispatchWithDefaults(agentId: string, title: string, priority?: string): void {
    this.dispatchTargetAgent = agentId;
    this.dispatchTitle = title;
    if (priority) this.dispatchPriority = priority;
    this.showDispatchModal = true;
  }

  closeDispatchModal(): void {
    this.showDispatchModal = false;
  }

  async submitDispatch(): Promise<void> {
    if (!this.dispatchTargetAgent || !this.dispatchTitle.trim()) return;

    this.dispatchSubmitting = true;
    const auditId = actionStore.start(
      `Dispatch task to ${this.dispatchTargetAgent}`,
      'PresencePanel:dispatch/submit',
    );
    try {
      await dispatchTask({
        target_agent_id: this.dispatchTargetAgent,
        title: this.dispatchTitle.trim(),
        context: this.dispatchContext.trim() || undefined,
        priority: this.dispatchPriority,
      });
      actionStore.succeed(auditId);
      toastStore.success(`Task dispatched to ${this.dispatchTargetAgent}`);
      this.showDispatchModal = false;
      this.dispatchTitle = '';
      this.dispatchContext = '';
      this.dispatchPriority = 'medium';
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      toastStore.error('Failed to dispatch task');
    } finally {
      this.dispatchSubmitting = false;
    }
  }

  onOpenNudge(agentId: string): void {
    this.nudgeTargetAgent = agentId;
    this.nudgeType = 'message';
    this.nudgeContent = '';
    this.showNudgeModal = true;
  }

  closeNudgeModal(): void {
    this.showNudgeModal = false;
  }

  async submitNudge(): Promise<void> {
    if (!this.nudgeTargetAgent || !this.nudgeContent.trim()) return;

    this.nudgeSubmitting = true;
    const auditId = actionStore.start(
      `Nudge ${this.nudgeTargetAgent}`,
      'PresencePanel:nudge/submit',
    );
    try {
      await sendNudge({
        target_agent_id: this.nudgeTargetAgent,
        type: this.nudgeType,
        content: this.nudgeContent.trim(),
        from_agent: 'hud',
      });
      actionStore.succeed(auditId);
      toastStore.success(`Nudge sent to ${this.nudgeTargetAgent}`);
      this.showNudgeModal = false;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      toastStore.error('Failed to send nudge');
    } finally {
      this.nudgeSubmitting = false;
    }
  }

  /**
   * Force-release one file claim. Returns true only after a 2xx so bulk
   * callers can count failures instead of reporting a blanket success.
   * `opts.silent` suppresses the per-item toast (both here and in the audit
   * entry ActionToast renders) so one bulk pass reports once.
   */
  async onReleaseClaim(
    agentId: string,
    filePath: string,
    opts?: { silent?: boolean },
  ): Promise<boolean> {
    const silent = opts?.silent ?? false;
    const auditId = actionStore.start(
      `Release claim: ${filePath} (${agentId})`,
      'FileConflicts:release',
      { silentSuccess: silent, silentError: silent },
    );
    try {
      await releaseClaim(agentId, filePath);
      actionStore.succeed(auditId);
      if (!silent) toastStore.success('Claim released');
      await fleetStore.fetch();
      return true;
    } catch (e) {
      actionStore.fail(auditId, errorMessage(e));
      if (!silent) toastStore.error('Failed to release claim');
      return false;
    }
  }
}

export const presenceActionsStore = new PresenceActionsStore();
