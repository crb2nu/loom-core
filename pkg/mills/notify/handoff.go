package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// defaultHandoffTarget is the target_agent_id a merge handoff is addressed to
// when policy.notify.handoff_target is unset. The recipient recalls these via
// agent_handoff_inbox{agent_id: "mills-merges"}.
const defaultHandoffTarget = "mills-merges"

// HandoffCreator is the slim subset of pipeline.HandoffClient the merge
// notifier needs. *clients.HandoffClient (mcp-agent-context agent_handoff_create
// over the MCP hub) satisfies it; a stub satisfies it in tests. Declared locally
// so the notify package stays decoupled from the concrete client.
type HandoffCreator interface {
	CreateHandoff(ctx context.Context, req pipeline.HandoffRequest) (pipeline.HandoffResponse, error)
}

// ContextRecorder writes one entry into the operator's agent-context session.
// Same shape as pipeline.ContextRecorder, declared locally so notify keeps its
// existing decoupling from the concrete client. *clients.ContextRecorder
// satisfies it.
type ContextRecorder interface {
	AddContextEntry(ctx context.Context, sessionID, entryType, title, content string, tags []string) error
}

// HandoffHook is a pipelineRunner.OnMerged hook that posts a "Mills merged X"
// record to the agent_context handoff inbox instead of an external HTTP
// webhook. It is the in-cluster alternative to WebhookHook: no external
// dependency, routed through the same MCP hub the operator already uses for
// escalation handoffs.
//
// Best-effort like WebhookHook: a CreateHandoff failure is logged and swallowed
// so it never blocks the rest of the OnMerged chain (audit triggers, tick-on-
// merge, eval attribution).
type HandoffHook struct {
	creator   HandoffCreator
	recorder  ContextRecorder
	target    string
	mrBaseURL string
	logger    *slog.Logger
}

// NewHandoffHook returns a hook addressed to target (defaulted when empty). A
// nil creator yields a disabled hook (OnMerged is a no-op), so callers can wire
// it unconditionally and gate on Enabled().
func NewHandoffHook(creator HandoffCreator, target, mrBaseURL string, logger *slog.Logger) *HandoffHook {
	if strings.TrimSpace(target) == "" {
		target = defaultHandoffTarget
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &HandoffHook{creator: creator, target: target, mrBaseURL: mrBaseURL, logger: logger}
}

// SetContextRecorder attaches the operator's agent-context recorder so each
// merge also lands a `finding` entry in the operator session the handoff is
// packaged from. Optional: a nil recorder leaves the hook's pre-existing
// handoff-only behaviour. Kept as a setter rather than a constructor parameter
// so existing NewHandoffHook callers are unaffected.
func (h *HandoffHook) SetContextRecorder(r ContextRecorder) {
	if h == nil {
		return
	}
	h.recorder = r
}

// Enabled reports whether the hook will post — useful for startup logs.
func (h *HandoffHook) Enabled() bool {
	return h != nil && h.creator != nil && strings.TrimSpace(h.target) != ""
}

// Target returns the configured target_agent_id (for startup logs).
func (h *HandoffHook) Target() string {
	if h == nil {
		return ""
	}
	return h.target
}

// OnMerged satisfies the pipelineRunner.OnMerged hook shape. Posts the merge to
// the handoff inbox. Always returns nil: a handoff failure must not propagate
// and block downstream hooks. Safe on a nil receiver (Enabled guards it), so a
// late-bound closure can call it before assignment.
func (h *HandoffHook) OnMerged(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if !h.Enabled() || run == nil || item == nil {
		return nil
	}
	// Record the merge outcome before the handoff so the packaged operator
	// session carries the finding rather than shipping empty.
	h.recordMergeFinding(ctx, run, item)

	req := pipeline.HandoffRequest{
		From:        "loom-mills-operator",
		To:          h.target,
		Reason:      h.summary(run, item),
		Context:     h.mergeContext(run, item),
		BacklogID:   item.ID,
		PipelineRun: run.ID,
	}
	if _, err := h.creator.CreateHandoff(ctx, req); err != nil {
		h.logger.Warn("notify handoff post failed",
			"run", run.ID, "backlog", item.ID, "target", h.target, "err", err)
	}
	return nil
}

// recordMergeFinding writes the merge outcome as a `finding` entry in the
// operator's agent-context session. No-op without a recorder; best-effort like
// every other step in OnMerged — a failure is logged and swallowed.
func (h *HandoffHook) recordMergeFinding(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) {
	if h == nil || h.recorder == nil {
		return
	}
	title := fmt.Sprintf("merged %s — %s", item.ID, item.Title)

	var b strings.Builder
	fmt.Fprintf(&b, "Backlog item: %s — %s\n", item.ID, item.Title)
	fmt.Fprintf(&b, "Pipeline run: %s\n", run.ID)
	if run.MRIID != nil && *run.MRIID > 0 {
		fmt.Fprintf(&b, "Merge request: !%d\n", *run.MRIID)
		if url := h.mrURL(*run.MRIID); url != "" {
			fmt.Fprintf(&b, "MR URL: %s\n", url)
		}
	}
	// The merged commit SHA is not on PipelineRun — it lands in the merge
	// stage's `merged_sha` artifact, which this hook (run + item only) cannot
	// reach without a store read. The MR IID is the durable handle; a reader
	// resolves the SHA from it.
	fmt.Fprintf(&b, "Cost: $%.2f over %d attempt(s)\n", run.CostUSD, run.Attempts)

	tags := []string{"mills", "merge", "backlog:" + item.ID}
	if err := h.recorder.AddContextEntry(ctx, "", "finding", title, b.String(), tags); err != nil {
		h.logger.Warn("notify context record failed",
			"run", run.ID, "backlog", item.ID, "err", err)
	}
}

// mrURL renders the absolute MR link when a base URL is configured.
func (h *HandoffHook) mrURL(iid int64) string {
	base := strings.TrimRight(strings.TrimSpace(h.mrBaseURL), "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/-/merge_requests/%d", base, iid)
}

// summary renders the one-line human-readable Reason carried by the handoff.
func (h *HandoffHook) summary(run *store.PipelineRun, item *store.BacklogItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mills merged %s — %s", item.ID, item.Title)
	if run.MRIID != nil && *run.MRIID > 0 {
		if strings.TrimSpace(h.mrBaseURL) != "" {
			fmt.Fprintf(&b, " (%s/-/merge_requests/%d)", strings.TrimRight(h.mrBaseURL, "/"), *run.MRIID)
		} else {
			fmt.Fprintf(&b, " (!%d)", *run.MRIID)
		}
	}
	return b.String()
}

// mergeContext bundles the structured merge facts onto the handoff so the
// recipient has the run links without a follow-up lookup.
func (h *HandoffHook) mergeContext(run *store.PipelineRun, item *store.BacklogItem) map[string]any {
	c := map[string]any{
		"event":        "mills.merged",
		"backlog_id":   item.ID,
		"title":        item.Title,
		"pipeline_run": run.ID,
		"cost_usd":     run.CostUSD,
		"attempts":     run.Attempts,
	}
	if run.MRIID != nil {
		c["mr_iid"] = *run.MRIID
	}
	return c
}
