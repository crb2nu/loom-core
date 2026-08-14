package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var dedupePhrases = map[string][]string{
	"operator": {
		"re-attaching to in-flight spawn",
		"re-dispatching pending spawn with deterministic key",
	},
	"mobile-hud": {
		"idempotent spawn re-attach (already exists)",
		"k8s alreadyexists on derived spawn name — re-attaching to existing pod",
	},
}

func dedupeLineMatches(component, line, spawnID string) bool {
	phrases := dedupePhrases[component]
	if len(phrases) == 0 || strings.TrimSpace(spawnID) == "" {
		return false
	}
	lower := strings.ToLower(line)
	phraseMatched := false
	for _, phrase := range phrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			phraseMatched = true
			break
		}
	}
	if !phraseMatched {
		return false
	}
	return lineHasExactSpawnID(line, spawnID)
}

func lineHasExactSpawnID(line, spawnID string) bool {
	if strings.TrimSpace(spawnID) == "" {
		return false
	}
	// Accept both slog text (spawn_id=abc) and JSON
	// ("spawn_id":"abc") while rejecting prefix collisions such as abc2.
	identityPattern := `(?i)"?spawn_id"?\s*[:=]\s*"?` + regexp.QuoteMeta(spawnID) + `(?:"|[\s,}]|$)`
	return regexp.MustCompile(identityPattern).FindStringIndex(line) != nil
}

type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (h *Harness) queryLoki(ctx context.Context, namespace, pod string, start, end time.Time) ([]LogEvidence, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	query := fmt.Sprintf(`{namespace=%s,pod=%s}`, strconv.Quote(namespace), strconv.Quote(pod))
	values := url.Values{}
	values.Set("query", query)
	values.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	values.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	values.Set("limit", "1000")
	values.Set("direction", "forward")
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s/proxy/loki/api/v1/query_range?%s",
		h.cfg.LokiNS, h.cfg.LokiService, values.Encode())
	raw, err := h.kubectl(queryCtx, "get", "--raw", path)
	if err != nil {
		return nil, err
	}
	var response lokiResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, err
	}
	if response.Status != "success" {
		return nil, fmt.Errorf("loki returned status %q", response.Status)
	}
	var entries []LogEvidence
	for _, result := range response.Data.Result {
		streamPod := result.Stream["pod"]
		if streamPod != pod {
			return nil, fmt.Errorf("loki returned unexpected pod %q for exact query %q", streamPod, pod)
		}
		for _, pair := range result.Values {
			if len(pair) < 2 {
				continue
			}
			var rawTimestamp, line string
			if json.Unmarshal(pair[0], &rawTimestamp) != nil || json.Unmarshal(pair[1], &line) != nil {
				return nil, errors.New("loki returned an invalid timestamp/line pair")
			}
			nanos, err := strconv.ParseInt(rawTimestamp, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse Loki timestamp %q: %w", rawTimestamp, err)
			}
			entries = append(entries, LogEvidence{
				Namespace: namespace, Pod: streamPod, Timestamp: time.Unix(0, nanos).UTC(), Line: line,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	return entries, nil
}

func findDedupeEvidence(entries []LogEvidence, patterns []string, spawnID string, notBefore time.Time) *LogEvidence {
	for _, entry := range entries {
		if entry.Timestamp.Before(notBefore) {
			continue
		}
		lower := strings.ToLower(entry.Line)
		matched := false
		for _, pattern := range patterns {
			if strings.Contains(lower, strings.ToLower(pattern)) {
				matched = true
				break
			}
		}
		if matched && (spawnID == "" || lineHasExactSpawnID(entry.Line, spawnID)) {
			entry.Line = strings.TrimSpace(entry.Line)
			return &entry
		}
	}
	return nil
}

// CollectDedupeEvidence queries Loki for exact replacement pod names and only
// accepts entries emitted after that component's crash timestamp. It retries
// briefly for ingestion lag; kubelet logs and old-pod regexes are deliberately
// excluded because either can attribute historical evidence to the wrong pod.
func (h *Harness) CollectDedupeEvidence(ctx context.Context, spawnID string, crashAAt time.Time, operator PodIdentity, crashBAt time.Time, hud PodIdentity) (*LogEvidence, error) {
	probes := []struct {
		component, namespace, pod string
		notBefore                 time.Time
		patterns                  []string
	}{
		{"operator", h.cfg.OperatorNS, operator.Name, crashAAt, []string{
			"re-attaching to in-flight spawn",
			"re-dispatching pending spawn with deterministic key",
		}},
		{"mobile-hud", h.cfg.HudNS, hud.Name, crashBAt, dedupePhrases["mobile-hud"]},
	}
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for {
		for _, probe := range probes {
			entries, err := h.queryLoki(ctx, probe.namespace, probe.pod, probe.notBefore, time.Now().UTC().Add(15*time.Second))
			if err != nil {
				lastErr = err
				continue
			}
			if evidence := findDedupeEvidence(entries, probe.patterns, spawnID, probe.notBefore); evidence != nil &&
				dedupeLineMatches(probe.component, evidence.Line, spawnID) {
				evidence.Component = probe.component
				return evidence, nil
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("dedupe evidence unavailable after Loki retries: %w", lastErr)
			}
			return nil, errors.New("no post-crash dedupe evidence from either exact replacement pod within 2m")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.cfg.PollInterval):
		}
	}
}
