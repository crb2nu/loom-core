package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// LokiClient fetches recent error logs from an in-cluster Loki and clusters
// them into workspace signals for the council brief (W3.1 of .loom/126 — feed
// real workspace pain into council proposals instead of synthetic canaries).
//
// It is a plain HTTP client against Loki's query_range API (same pattern as the
// GitLab + FlexInfer clients), NOT routed through the MCP hub: the operator
// already has cluster-internal HTTP, and a read of the log index needs no
// auth boundary. A zero/empty URL yields a disabled client (Enabled()==false),
// so the council brief simply omits the signals section.
type LokiClient struct {
	baseURL     string
	http        *http.Client
	logger      *slog.Logger
	maxLines    int // cap log lines pulled per query (default 300)
	maxClusters int // cap clusters returned to the brief (default 8)
}

// NewLokiClient returns a client for baseURL (e.g.
// http://loki.logging.svc.cluster.local:3100). An empty baseURL returns nil so
// callers can wire it unconditionally and gate on the nil/Enabled check.
func NewLokiClient(baseURL string, logger *slog.Logger) *LokiClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LokiClient{
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:        &http.Client{Timeout: 15 * time.Second},
		logger:      logger,
		maxLines:    300,
		maxClusters: 8,
	}
}

// Enabled reports whether the client will query.
func (c *LokiClient) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// errorMatch is the LogQL line filter for error-class logs across the cluster.
// Case-insensitive; matches the common level encodings (logfmt level=error,
// JSON "level":"error", bare ERROR/FATAL, and panic:). Kept deliberately broad
// — the council brief wants signal, not precision, and the clustering below
// collapses the noise.
const lokiErrorSelector = `{namespace=~".+"} |~ ` +
	"`(?i)(level=(error|fatal)|\"level\":\"(error|fatal)\"|\\bERROR\\b|\\bFATAL\\b|panic:)`"

// lokiQueryRangeResponse is the subset of Loki's query_range payload we read.
type lokiQueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"` // [ [ "<unix-ns ts>", "<log line>" ], ... ]
		} `json:"result"`
	} `json:"data"`
}

// RecentErrorClusters queries Loki for error-class logs since `since`, clusters
// them by a normalized message signature, and returns the top clusters by
// volume as council workspace signals. Best-effort: any transport/parse failure
// returns the error so the caller (the brief assembler) can skip the section;
// it never panics on malformed payloads.
func (c *LokiClient) RecentErrorClusters(ctx context.Context, since time.Time) ([]council.WorkspaceSignal, error) {
	if !c.Enabled() {
		return nil, nil
	}
	end := time.Now()
	if !since.Before(end) {
		since = end.Add(-24 * time.Hour)
	}

	q := url.Values{}
	q.Set("query", lokiErrorSelector)
	q.Set("start", strconv.FormatInt(since.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(c.maxLines))
	q.Set("direction", "backward")
	reqURL := c.baseURL + "/loki/api/v1/query_range?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("loki: new request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki: query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("loki: status %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
	}
	var parsed lokiQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("loki: decode: %w", err)
	}
	return clusterLokiResult(parsed, c.maxClusters), nil
}

// errorCluster accumulates one normalized-signature group.
type errorCluster struct {
	signature string
	service   string
	sample    string
	count     int
}

// clusterLokiResult collapses raw log lines into the top error clusters. Pure +
// deterministic so it is unit-testable without a live Loki.
func clusterLokiResult(resp lokiQueryRangeResponse, maxClusters int) []council.WorkspaceSignal {
	if maxClusters <= 0 {
		maxClusters = 8
	}
	groups := map[string]*errorCluster{}
	order := []string{}
	for _, stream := range resp.Data.Result {
		svc := serviceLabel(stream.Stream)
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue
			}
			line := strings.TrimSpace(v[1])
			if line == "" {
				continue
			}
			sig := svc + "|" + normalizeLogSignature(line)
			g, ok := groups[sig]
			if !ok {
				g = &errorCluster{signature: sig, service: svc, sample: truncateLine(line, 200)}
				groups[sig] = g
				order = append(order, sig)
			}
			g.count++
		}
	}
	clusters := make([]*errorCluster, 0, len(order))
	for _, sig := range order {
		clusters = append(clusters, groups[sig])
	}
	// Sort by count desc, then signature asc for deterministic ties.
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].count != clusters[j].count {
			return clusters[i].count > clusters[j].count
		}
		return clusters[i].signature < clusters[j].signature
	})
	if len(clusters) > maxClusters {
		clusters = clusters[:maxClusters]
	}
	out := make([]council.WorkspaceSignal, 0, len(clusters))
	for _, g := range clusters {
		out = append(out, council.WorkspaceSignal{
			Source:  "loki",
			Service: g.service,
			Count:   g.count,
			Sample:  g.sample,
		})
	}
	return out
}

// serviceLabel picks the most useful identity from a stream's labels.
func serviceLabel(labels map[string]string) string {
	for _, key := range []string{"app", "app_kubernetes_io_name", "container", "job", "pod"} {
		if v := strings.TrimSpace(labels[key]); v != "" {
			if ns := strings.TrimSpace(labels["namespace"]); ns != "" {
				return ns + "/" + v
			}
			return v
		}
	}
	if ns := strings.TrimSpace(labels["namespace"]); ns != "" {
		return ns
	}
	return "unknown"
}

var (
	// reTimestamp strips ISO-8601 / RFC3339 timestamps so two lines that
	// differ only by when they fired collapse to one cluster. Case-insensitive
	// because normalizeLogSignature lowercases the line first (so the T/Z
	// separators arrive as t/z).
	reTimestamp = regexp.MustCompile(`(?i)\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(\.\d+)?(z|[+-]\d{2}:?\d{2})?`)
	// reHex collapses hex blobs (uuids, sha, addresses, pointers).
	reHex = regexp.MustCompile(`\b(0x)?[0-9a-fA-F]{6,}\b`)
	// reNum collapses bare numbers (ports, ids, durations).
	reNum = regexp.MustCompile(`\b\d+\b`)
	// reQuoted collapses quoted strings (which often carry the variable bit).
	reQuoted = regexp.MustCompile(`"[^"]*"`)
	// reWS squashes runs of whitespace.
	reWS = regexp.MustCompile(`\s+`)
)

// normalizeLogSignature reduces a log line to a stable signature so lines that
// differ only by variable data (timestamps, ids, ports, quoted values) cluster
// together. Lossy by design; the first ~120 chars of the normalized form is the
// cluster key.
func normalizeLogSignature(line string) string {
	s := strings.ToLower(line)
	s = reTimestamp.ReplaceAllString(s, "<ts>")
	s = reQuoted.ReplaceAllString(s, `"<v>"`)
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reNum.ReplaceAllString(s, "<n>")
	s = reWS.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func truncateLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
