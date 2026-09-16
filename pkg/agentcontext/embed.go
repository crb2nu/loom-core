package agentcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmbedHealthResponse is the machine-readable embedder admission snapshot.
// FailClosedCounts contains counts observed during the trailing window ending
// at ObservedAt. Any non-zero count makes the snapshot unhealthy.
type EmbedHealthResponse struct {
	Status           string           `json:"status"`
	ObservedAt       time.Time        `json:"observed_at"`
	FailClosedCounts map[string]int64 `json:"fail_closed_counts"`
}

// ProbeEmbedHealth fetches and validates a fresh embedder health snapshot.
func ProbeEmbedHealth(ctx context.Context, client *http.Client, endpoint string, now time.Time, trailingWindow time.Duration) (EmbedHealthResponse, error) {
	var snapshot EmbedHealthResponse
	if client == nil {
		return snapshot, errors.New("embed health HTTP client is nil")
	}
	if strings.TrimSpace(endpoint) == "" {
		return snapshot, errors.New("embed health endpoint is empty")
	}
	if trailingWindow <= 0 {
		return snapshot, errors.New("embed health trailing window must be positive")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return snapshot, fmt.Errorf("build embed health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return snapshot, fmt.Errorf("request embed health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return snapshot, fmt.Errorf("request embed health: HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snapshot); err != nil {
		return snapshot, fmt.Errorf("decode embed health: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return snapshot, errors.New("decode embed health: trailing JSON data")
	}
	if snapshot.Status != "healthy" {
		return snapshot, fmt.Errorf("embed health status is %q, want healthy", snapshot.Status)
	}
	if snapshot.ObservedAt.IsZero() {
		return snapshot, errors.New("embed health observed_at is missing")
	}
	if snapshot.FailClosedCounts == nil {
		return snapshot, errors.New("embed health fail_closed_counts is missing")
	}
	age := now.Sub(snapshot.ObservedAt)
	if age < 0 || age > trailingWindow {
		return snapshot, fmt.Errorf("embed health snapshot is stale or clock-skewed: observed_at=%s age=%s window=%s", snapshot.ObservedAt.UTC(), age, trailingWindow)
	}
	for name, count := range snapshot.FailClosedCounts {
		if count < 0 {
			return snapshot, fmt.Errorf("embed health fail-closed count %q is negative: %d", name, count)
		}
		if count != 0 {
			return snapshot, fmt.Errorf("embed health fail-closed count %q is non-zero: %d", name, count)
		}
	}
	return snapshot, nil
}
