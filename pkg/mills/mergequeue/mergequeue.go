package mergequeue

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const MainRedExternalReason = "main_red_external"

type MainRedExternalController struct {
	Store    *store.Store
	Now      func() time.Time
	Escalate func(context.Context, store.MainRedExternalHold) error
}

// Reconcile returns the durable deferral reason while a hold is active. At
// expiry it atomically claims and emits the sole escalation.
func (c *MainRedExternalController) Reconcile(ctx context.Context, project, branch string) (string, error) {
	if c == nil || c.Store == nil {
		return "", nil
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	h, err := c.Store.MainRedExternalHold(ctx, project, branch)
	if err != nil || h == nil || h.ClearedAt != nil {
		return "", err
	}
	if now.Before(h.ExpiresAt) {
		return MainRedExternalReason, nil
	}
	won, err := c.Store.ClaimMainRedExternalEscalation(ctx, project, branch, now)
	if err != nil {
		return "", err
	}
	if won && c.Escalate != nil {
		h.EscalationSentAt = &now
		if err := c.Escalate(ctx, *h); err != nil {
			return "", err
		}
	}
	return "", nil
}

// deferMainRedExternal is enforced even when the operator has not supplied an
// optional controller override. Persist the reason without changing queue state.
func (p *Processor) deferMainRedExternal(ctx context.Context, e *store.MergeQueueEntry) (bool, error) {
	c := p.MainRedExternal
	if c == nil {
		c = &MainRedExternalController{Store: p.Store, Now: p.Now}
	}
	reason, err := c.Reconcile(ctx, e.Project, e.TargetBranch)
	if err != nil {
		return false, err
	}
	previous, _ := e.Detail["defer_reason"].(string)
	if previous != reason {
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		if reason == "" {
			delete(e.Detail, "defer_reason")
		} else {
			e.Detail["defer_reason"] = reason
		}
		_, err = p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{ID: e.ID, From: e.State, To: e.State, Detail: e.Detail})
	}
	return reason != "", err
}
