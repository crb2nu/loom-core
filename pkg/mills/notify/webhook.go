// Package notify hosts external-notification hooks for Mills. First
// slice ships a generic webhook poster wired into pipelineRunner.OnMerged
// so the operator gets a real-time "Mills shipped X" ping (Slack /
// Discord / generic webhook).
//
// Notification failures must never block the OnMerged chain: the hook
// catches and logs every failure and always returns nil so downstream
// hooks (audit triggers, etc.) still fire.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	defaultHTTPTimeout = 10 * time.Second
	// maxMessageRunes caps the rendered text length so a giant log_tail
	// can't blow up the Slack/Discord 4000-char limit.
	maxMessageRunes = 2000
)

// PipelineStore is the slim subset of *store.Store the webhook reads
// to count attempts + summarise error classes for the message body.
// Implemented by *store.Store.PipelineDAO; trimmed so tests can stub
// without standing up the full store.
type PipelineStore interface {
	ListStages(ctx context.Context, runID string) ([]*store.StageResult, error)
}

// HTTPClient is the http.Client surface we depend on, narrowed so tests
// can pass an httptest.Server's client without exposing the full type.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config captures the operator-tunable knobs. The zero value disables
// the hook (URL empty → OnMerged returns nil immediately).
type Config struct {
	// URL is the destination POST endpoint. Slack incoming-webhook URLs
	// take a `{ "text": "..." }` payload; Discord webhooks take the same.
	URL string
	// Timeout caps the POST. Default 10s.
	Timeout time.Duration
	// MRBaseURL prefixes the MR link in the message. Optional — when
	// empty the message falls back to the bare backlog id.
	MRBaseURL string
}

// WebhookHook is the pipelineRunner.OnMerged-shaped notifier. Wire via
// hook chain in cmd/loom-mills-operator/main.go.
type WebhookHook struct {
	cfg    Config
	http   HTTPClient
	store  PipelineStore
	logger *slog.Logger
}

// New returns a configured hook. Disabled-by-default safety: if
// cfg.URL is empty, the returned hook's OnMerged is a no-op (logged
// once at construction).
func New(cfg Config, st PipelineStore, h HTTPClient, logger *slog.Logger) *WebhookHook {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultHTTPTimeout
	}
	if h == nil {
		h = &http.Client{Timeout: cfg.Timeout}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookHook{cfg: cfg, http: h, store: st, logger: logger}
}

// Enabled reports whether the hook will actually post — useful for
// startup logs.
func (h *WebhookHook) Enabled() bool {
	return h != nil && strings.TrimSpace(h.cfg.URL) != ""
}

// OnMerged satisfies the pipelineRunner.OnMerged hook shape. Posts a
// summary of the merge to the configured webhook. Always returns nil:
// a notification failure must not propagate and block downstream
// hooks (audit triggers, eval attribution, etc.).
func (h *WebhookHook) OnMerged(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if h == nil || !h.Enabled() {
		return nil
	}
	if run == nil || item == nil {
		return nil
	}
	summary := h.buildSummary(ctx, run, item)
	payload := map[string]any{"text": summary}
	if err := h.post(ctx, payload); err != nil {
		h.logger.Warn("notify webhook post failed",
			"run", run.ID, "backlog", item.ID, "err", err)
	}
	return nil
}

// buildSummary renders the message body. Stage-result lookup is best-
// effort — if the store call fails the summary still renders with the
// run-level facts we already have.
func (h *WebhookHook) buildSummary(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🛠 Mills merged %s — *%s*\n", item.ID, item.Title)
	if run.MRIID != nil && *run.MRIID > 0 {
		if h.cfg.MRBaseURL != "" {
			fmt.Fprintf(&b, "MR: %s/-/merge_requests/%d\n",
				strings.TrimRight(h.cfg.MRBaseURL, "/"), *run.MRIID)
		} else {
			fmt.Fprintf(&b, "MR: !%d\n", *run.MRIID)
		}
	}
	if run.CostUSD > 0 {
		fmt.Fprintf(&b, "cost: $%.4f · attempts (run): %d\n", run.CostUSD, run.Attempts)
	}
	if h.store != nil {
		if stages, err := h.store.ListStages(ctx, run.ID); err == nil && len(stages) > 0 {
			retries, errClasses := summarizeStages(stages)
			if retries > 0 {
				fmt.Fprintf(&b, "stage retries: %d\n", retries)
			}
			if len(errClasses) > 0 {
				fmt.Fprintf(&b, "errors recovered from: %s\n", strings.Join(errClasses, ", "))
			}
		}
	}
	if msg := b.String(); len([]rune(msg)) > maxMessageRunes {
		runes := []rune(msg)
		return string(runes[:maxMessageRunes-1]) + "…"
	}
	return b.String()
}

// summarizeStages returns (totalRetries, uniqueLogTailHints). Retries
// is the count of stage_result rows past attempt 1. Hints is a small
// set of distinct log_tail prefixes so the operator can see "transient
// recovery happened" without grepping logs.
func summarizeStages(stages []*store.StageResult) (int, []string) {
	retries := 0
	hintSet := map[string]struct{}{}
	for _, sr := range stages {
		if sr.Attempt > 1 {
			retries++
		}
		if sr.LogTail == "" {
			continue
		}
		if hint := classHintFromLogTail(sr.LogTail); hint != "" {
			hintSet[hint] = struct{}{}
		}
	}
	hints := make([]string, 0, len(hintSet))
	for h := range hintSet {
		hints = append(hints, h)
	}
	return retries, hints
}

// classHintFromLogTail extracts a short category from a stage_result
// log_tail. Keep in lock-step with pkg/mills/pipeline/error_class.go's
// Classify — same patterns, but here we just label, not classify.
func classHintFromLogTail(t string) string {
	lower := strings.ToLower(t)
	switch {
	case strings.Contains(lower, "pod not found during reconciliation"):
		return "k8s-pod-gc"
	case strings.Contains(lower, "websocket: close"):
		return "mcp-transport"
	case strings.Contains(lower, "broken pipe"):
		return "mcp-broken-pipe"
	case strings.Contains(lower, "context deadline exceeded"):
		return "flexinfer-timeout"
	case strings.Contains(lower, "already exists"):
		return "buildah-pod-conflict"
	case strings.Contains(lower, "buildah build failed"):
		return "buildah-build-fail"
	}
	return ""
}

// PostEvent posts an arbitrary JSON payload to the configured webhook,
// reusing the exact URL/timeout/transport OnMerged uses so callers get the
// same policy.notify.webhook_url wiring verbatim. Unlike OnMerged (which
// swallows failures to protect the merge-hook chain) PostEvent surfaces the
// error to the caller so an overseer can record it in its audit trail;
// returns ErrDisabled when no URL is configured. Additive: OnMerged behavior
// is unchanged.
func (h *WebhookHook) PostEvent(ctx context.Context, payload map[string]any) error {
	if h == nil || !h.Enabled() {
		return ErrDisabled
	}
	return h.post(ctx, payload)
}

// post submits the JSON payload to the configured URL. Returns an
// error only on transport failures or non-2xx — OnMerged swallows
// these and logs.
func (h *WebhookHook) post(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	postCtx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, h.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
	}
	return nil
}

// Sentinel for tests that want to assert disabled-state behavior
// without inspecting cfg directly.
var ErrDisabled = errors.New("notify: webhook URL not configured")
