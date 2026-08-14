package pipeline

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const IncidentClassifierName = "mills-incident-classifier"

type incidentRule struct {
	source     string
	dependency string
	shape      string
	summary    string
	retryable  bool
	all        []string
	any        []string
	anyGroups  [][]string
}

// ClassifyIncident deterministically recognizes the external GitLab CI,
// model-provider, and storage failure shapes Mills can safely act on. Unknown
// and ambiguous messages return matched=false.
func ClassifyIncident(source, message string) (record store.IncidentRecord, matched bool) {
	normalizedSource := normalizeIncidentText(source)
	normalizedMessage := normalizeIncidentText(message)
	if normalizedMessage == "" {
		return store.IncidentRecord{}, false
	}
	for _, rule := range incidentRules {
		if rule.matches(normalizedSource, normalizedMessage) {
			fingerprint := sha256.Sum256([]byte(rule.source + "\x00" + rule.shape + "\x00" + normalizedMessage))
			return store.IncidentRecord{
				ID:         fmt.Sprintf("INC-%x", fingerprint[:8]),
				Class:      store.IncidentClassExternalDependency,
				Source:     rule.source,
				Dependency: rule.dependency,
				Shape:      rule.shape,
				Summary:    rule.summary,
				Evidence:   strings.TrimSpace(message),
				Retryable:  rule.retryable,
			}, true
		}
	}
	return store.IncidentRecord{}, false
}

// ClassifyExternalDependencyIncident is the explicit-name alias used by
// callers that classify several kinds of pipeline outcome.
func ClassifyExternalDependencyIncident(source, message string) (store.IncidentRecord, bool) {
	return ClassifyIncident(source, message)
}

func (r incidentRule) matches(source, message string) bool {
	if source != "" && source != r.source {
		return false
	}
	for _, needle := range r.all {
		if !strings.Contains(message, needle) {
			return false
		}
	}
	for _, group := range r.anyGroups {
		if !hasIncidentNeedle(message, group) {
			return false
		}
	}
	return len(r.any) == 0 || hasIncidentNeedle(message, r.any)
}

func hasIncidentNeedle(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func normalizeIncidentText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

var incidentRules = []incidentRule{
	{
		source: "gitlab-ci", dependency: "gitlab", shape: "service-unavailable",
		summary: "GitLab CI service is unavailable", retryable: true,
		all: []string{"gitlab"}, any: []string{"status 500", "status 502", "status 503", "status 504", "bad gateway", "service unavailable", "gateway timeout"},
	},
	{
		source: "gitlab-ci", dependency: "gitlab", shape: "rate-limit",
		summary: "GitLab CI request rate limit was exceeded", retryable: true,
		all: []string{"gitlab"}, any: []string{"status 429", "too many requests", "rate limit", "rate_limit"},
	},
	{
		source: "gitlab-ci", dependency: "gitlab-runner", shape: "runner-system-failure",
		summary: "GitLab runner reported a system failure", retryable: true,
		any: []string{"runner system failure", "stuck or timeout failure", "job execution timeout"},
	},
	{
		source: "model-provider", dependency: "model-provider", shape: "rate-limit",
		summary: "Model provider request rate limit was exceeded", retryable: true,
		any:       []string{"status 429", "too many requests", "rate limit", "rate_limit", "quota exceeded"},
		anyGroups: [][]string{{"provider", "flexinfer", "anthropic", "openai", "litellm"}},
	},
	{
		source: "model-provider", dependency: "model-provider", shape: "service-unavailable",
		summary: "Model provider is unavailable", retryable: true,
		any:       []string{"status 500", "status 502", "status 503", "status 504", "bad gateway", "service unavailable", "gateway timeout", "context deadline exceeded"},
		anyGroups: [][]string{{"provider", "flexinfer", "anthropic", "openai", "litellm"}},
	},
	{
		source: "model-provider", dependency: "model-provider", shape: "ungradeable-response",
		summary: "Model provider returned an ungradeable response", retryable: false,
		any:       []string{"no parseable score envelope", "unparseable response", "empty score envelope"},
		anyGroups: [][]string{{"judge", "rubric", "model"}},
	},
	{
		source: "storage", dependency: "object-storage", shape: "service-unavailable",
		summary: "Object storage service is unavailable", retryable: true,
		any:       []string{"status 500", "status 502", "status 503", "status 504", "service unavailable", "gateway timeout", "timed out"},
		anyGroups: [][]string{{"s3", "gcs", "minio", "object storage", "artifact storage", "cache storage"}},
	},
	{
		source: "storage", dependency: "container-registry-storage", shape: "manifest-write",
		summary: "Container registry storage rejected a manifest write", retryable: false,
		any:       []string{"error writing manifest", "manifest blob", "blob upload unknown"},
		anyGroups: [][]string{{"registry", "blob", "manifest"}},
	},
	{
		source: "storage", dependency: "storage", shape: "capacity-exhausted",
		summary: "Storage capacity is exhausted", retryable: false,
		any: []string{"no space left on device", "disk pressure", "insufficient ephemeral-storage", "no available disk", "no available disks"},
	},
}
