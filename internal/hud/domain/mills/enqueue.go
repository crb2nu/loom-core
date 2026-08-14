package mills

// enqueue.go -- server-side projection of a Pattern Loom stamp into a Mills
// BacklogItem. Unlike the reverse proxy (which forwards a browser's
// /api/mills/* call), this is a direct HUD->operator POST used when the HUD
// itself enqueues work: the Pattern Loom stamp path turns a stamped Plan into a
// queued BacklogItem the operator's reconciler will pick up.
//
// It mirrors the proxy's mutation contract exactly: POST /api/mills/backlog
// with the operator admin bearer. The operator owns dedupe (canary label) and
// defaulting (state=queued, priority=P3); we only send the item.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// enqueueClient is the HTTP client for operator backlog POSTs. A bounded
// timeout keeps a wedged operator from hanging the stamp request.
var enqueueClient = &http.Client{Timeout: 30 * time.Second}

// EnqueueBacklogItem POSTs item to the operator's backlog intake with the admin
// bearer and returns the persisted item (the operator fills in defaults like
// state=queued / priority=P3). force=true appends ?force=1 to bypass the
// operator's canary-style in-flight dedupe.
//
// Errors are returned (never panics): an unconfigured operator, a transport
// failure, or a non-2xx status (with the response body folded into the error so
// the caller can surface why the operator rejected the item).
func EnqueueBacklogItem(ctx context.Context, cfg Config, item store.BacklogItem, force bool) (*store.BacklogItem, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("mills operator not configured (no BaseURL)")
	}
	body, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("marshal backlog item: %w", err)
	}

	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/api/mills/backlog"
	if force {
		endpoint += "?force=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "loom-hud/pattern-stamp")
	if cfg.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	}

	resp, err := enqueueClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post backlog item: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("operator rejected backlog item: %s: %s",
			resp.Status, strings.TrimSpace(string(respBody)))
	}

	var created store.BacklogItem
	if err := json.Unmarshal(respBody, &created); err != nil {
		// The operator accepted it (2xx) but returned an unparseable body —
		// surface that rather than pretending we have the persisted item.
		return nil, fmt.Errorf("decode operator response (status %s): %w", resp.Status, err)
	}
	return &created, nil
}
