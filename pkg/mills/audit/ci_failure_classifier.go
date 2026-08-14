package audit

import (
	"errors"
	"io"
	"strings"
)

const CIFailureClassifierName = "ci-failure-classifier"

// CIFailureCategory is the bounded taxonomy used to group recurring CI
// failures before opening or deduplicating operator-facing incident threads.
type CIFailureCategory string

const (
	CIFailureCategoryExternalDependency CIFailureCategory = "external_dependency"
	CIFailureCategoryInfrastructure     CIFailureCategory = "infrastructure"
	CIFailureCategoryConfiguration      CIFailureCategory = "configuration"
	CIFailureCategoryCode               CIFailureCategory = "code"
)

// CIFailureClassification is a deterministic, wire-safe summary of a CI
// failure signature. Unknown messages return Matched=false so callers do not
// accidentally group novel failures under a misleading recurring class.
type CIFailureClassification struct {
	Classifier string            `json:"classifier"`
	Matched    bool              `json:"matched"`
	Category   CIFailureCategory `json:"category,omitempty"`
	Stage      string            `json:"stage,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Dependency string            `json:"dependency,omitempty"`
	Retryable  bool              `json:"retryable"`
	Terminal   bool              `json:"terminal"`
	Labels     []string          `json:"labels,omitempty"`
}

// ClassifyCIFailure maps a CI/stage error onto the recurring failure taxonomy.
func ClassifyCIFailure(err error) CIFailureClassification {
	if err == nil {
		return unmatchedCIFailure()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return matchedCIFailure(ciFailureMatch{
			category:   CIFailureCategoryInfrastructure,
			stage:      "ci",
			reason:     "ci-transport-eof",
			dependency: "network",
			retryable:  true,
		})
	}
	return ClassifyCIFailureMessage(err.Error())
}

// ClassifyCIFailureMessage maps raw GitLab CI, Mills ci_watch, and guardrail
// output onto a bounded taxonomy of recurring pipeline failure signatures.
func ClassifyCIFailureMessage(message string) CIFailureClassification {
	normalized := normalizeCIFailureMessage(message)
	if normalized == "" {
		return unmatchedCIFailure()
	}

	if classification, ok := ClassifyExternalCIFailureMessage(message); ok {
		return classification
	}

	for _, rule := range ciFailureRules {
		if rule.matches(normalized) {
			return matchedCIFailure(ciFailureMatch{
				category:   rule.category,
				stage:      firstNonEmpty(rule.stage, stageFromMessage(normalized)),
				reason:     rule.reason,
				dependency: rule.dependency,
				retryable:  rule.retryable,
				terminal:   rule.terminal,
			})
		}
	}
	return unmatchedCIFailure()
}

type ciFailureRule struct {
	any        []string
	all        []string
	anyGroup   [][]string
	category   CIFailureCategory
	stage      string
	reason     string
	dependency string
	retryable  bool
	terminal   bool
}

type ciFailureMatch struct {
	category   CIFailureCategory
	stage      string
	reason     string
	dependency string
	retryable  bool
	terminal   bool
}

func (r ciFailureRule) matches(message string) bool {
	for _, needle := range r.all {
		if !strings.Contains(message, needle) {
			return false
		}
	}
	for _, group := range r.anyGroup {
		if !containsAny(message, group) {
			return false
		}
	}
	if len(r.any) == 0 {
		return true
	}
	return containsAny(message, r.any)
}

func containsAny(message string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

var ciFailureRules = []ciFailureRule{
	{
		any: []string{
			"pipeline poll timed out",
			"pipeline: poll deadline exceeded",
		},
		category:   CIFailureCategoryInfrastructure,
		stage:      "ci_watch",
		reason:     "ci-watch-poll-timeout",
		dependency: "gitlab_ci",
		retryable:  true,
	},
	{
		any: []string{
			"ci pipeline failed",
			"pipeline reached terminal",
			"pipeline status failed",
			"pipeline failed",
		},
		all:       []string{"ci_watch"},
		category:  CIFailureCategoryCode,
		stage:     "ci_watch",
		reason:    "ci-watch-terminal-pipeline",
		terminal:  true,
		retryable: false,
	},
	{
		any: []string{
			"status 405",
			"method not allowed",
			"status 422",
			"cannot be merged",
			"branch cannot be merged",
		},
		all:        []string{"gitlab"},
		category:   CIFailureCategoryConfiguration,
		stage:      "merge",
		reason:     "gitlab-merge-configuration",
		dependency: "gitlab",
		terminal:   true,
	},
	{
		any: []string{
			"missing changelog",
			"docs guardrail",
			"guardrails:docs-cli",
			"check_docs_guardrails",
			"[skip-docs-check]",
		},
		category:  CIFailureCategoryCode,
		stage:     "ci",
		reason:    "docs-guardrail-missing-entry",
		terminal:  true,
		retryable: false,
	},
	{
		any: []string{
			"status 500",
			"status 502",
			"status 503",
			"status 504",
			"bad gateway",
			"service unavailable",
			"gateway timeout",
		},
		all:        []string{"gitlab"},
		category:   CIFailureCategoryExternalDependency,
		stage:      "ci",
		reason:     "gitlab-service-unavailable",
		dependency: "gitlab",
		retryable:  true,
	},
	{
		any: []string{
			"status 429",
			"too many requests",
			"rate limit",
			"rate_limit",
			"quota exceeded",
		},
		anyGroup: [][]string{
			modelProviderTokens,
		},
		category:   CIFailureCategoryExternalDependency,
		reason:     "model-provider-rate-limit",
		dependency: "model_provider",
		retryable:  true,
	},
	{
		any: []string{
			"status 500",
			"status 502",
			"status 503",
			"status 504",
			"bad gateway",
			"service unavailable",
			"gateway timeout",
			"context deadline exceeded",
		},
		anyGroup: [][]string{
			modelProviderTokens,
		},
		category:   CIFailureCategoryExternalDependency,
		reason:     "model-provider-service-unavailable",
		dependency: "model_provider",
		retryable:  true,
	},
	{
		any: []string{
			"runner system failure",
			"runner failed",
			"stuck or timeout failure",
			"job execution timeout",
		},
		category:   CIFailureCategoryInfrastructure,
		stage:      "ci",
		reason:     "gitlab-runner-infrastructure",
		dependency: "gitlab_runner",
		retryable:  true,
	},
}

var modelProviderTokens = []string{
	"provider:",
	"flexinfer",
	"anthropic",
	"openai",
	"model provider",
}

func normalizeCIFailureMessage(message string) string {
	return strings.ToLower(strings.TrimSpace(message))
}

func stageFromMessage(message string) string {
	for _, stage := range []string{"ci_watch", "merge", "mr", "tests", "pr_self_review"} {
		if strings.Contains(message, stage) {
			return stage
		}
	}
	return "ci"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func unmatchedCIFailure() CIFailureClassification {
	return CIFailureClassification{
		Classifier: CIFailureClassifierName,
		Matched:    false,
	}
}

func matchedCIFailure(match ciFailureMatch) CIFailureClassification {
	labels := []string{
		"kind/ci-failure",
		"ci_failure/" + string(match.category),
		"ci_failure_reason/" + match.reason,
		"retryable/" + boolLabel(match.retryable),
		"terminal/" + boolLabel(match.terminal),
	}
	if match.stage != "" {
		labels = append(labels, "stage/"+match.stage)
	}
	if match.dependency != "" {
		labels = append(labels, "dependency/"+match.dependency)
	}
	if match.category == CIFailureCategoryExternalDependency {
		labels = append(labels, externalDependencyIncidentLabels(match.reason, match.dependency)...)
	}
	return CIFailureClassification{
		Classifier: CIFailureClassifierName,
		Matched:    true,
		Category:   match.category,
		Stage:      match.stage,
		Reason:     match.reason,
		Dependency: match.dependency,
		Retryable:  match.retryable,
		Terminal:   match.terminal,
		Labels:     labels,
	}
}
