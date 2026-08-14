package audit

import (
	"errors"
	"io"
	"strings"

	"github.com/crb2nu/loom/internal/contracts"
)

// ClassifyIncident maps recurring raw workspace errors onto a bounded,
// structured incident contract. Unknown messages intentionally return
// Matched=false so callers do not treat code defects as dependency incidents.
func ClassifyIncident(err error) contracts.IncidentClassification {
	if err == nil {
		return unmatchedIncident()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return matchedIncident(contracts.IncidentDependencyNetwork, "network-eof", true)
	}
	return ClassifyIncidentMessage(err.Error())
}

// ClassifyIncidentMessage maps a raw error string onto an incident
// classification. Rules are ordered from specific to broad so provider-specific
// quota and config cases win before generic transport patterns.
func ClassifyIncidentMessage(message string) contracts.IncidentClassification {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return unmatchedIncident()
	}

	for _, rule := range incidentRules {
		if rule.matches(lower) {
			return matchedIncident(rule.dependency, rule.reason, rule.retryable)
		}
	}
	return unmatchedIncident()
}

type incidentRule struct {
	any        []string
	all        []string
	dependency contracts.IncidentDependency
	reason     string
	retryable  bool
}

func (r incidentRule) matches(message string) bool {
	for _, needle := range r.all {
		if !strings.Contains(message, needle) {
			return false
		}
	}
	if len(r.any) == 0 {
		return true
	}
	for _, needle := range r.any {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

var incidentRules = []incidentRule{
	{
		any: []string{
			"max concurrent spawns reached",
			"concurrent spawns",
		},
		dependency: contracts.IncidentDependencySpawnPool,
		reason:     "spawn-pool-saturated",
		retryable:  true,
	},
	{
		any: []string{"429", "too many requests", "rate limit", "rate_limit", "quota exceeded"},
		all: []string{"flexinfer"},

		dependency: contracts.IncidentDependencyFlexInfer,
		reason:     "flexinfer-rate-limit",
		retryable:  true,
	},
	{
		any: []string{"429", "too many requests", "rate limit", "rate_limit", "quota exceeded"},
		all: []string{"gitlab"},

		dependency: contracts.IncidentDependencyGitLab,
		reason:     "gitlab-rate-limit",
		retryable:  true,
	},
	{
		any: []string{"status 405", "method not allowed"},
		all: []string{"gitlab"},

		dependency: contracts.IncidentDependencyGitLab,
		reason:     "gitlab-merge-config",
		retryable:  false,
	},
	{
		any: []string{
			"gitlab: get",
			"gitlab: post",
			"gitlab: put",
			"gitlab: delete",
		},
		all: []string{"status 5"},

		dependency: contracts.IncidentDependencyGitLab,
		reason:     "gitlab-service-unavailable",
		retryable:  true,
	},
	{
		any: []string{
			"flexinfer chat",
			"flexinfer-proxy",
		},
		all: []string{"context deadline exceeded"},

		dependency: contracts.IncidentDependencyFlexInfer,
		reason:     "flexinfer-timeout",
		retryable:  true,
	},
	{
		any: []string{"status 500", "status 502", "status 503", "status 504", "bad gateway", "service unavailable", "gateway timeout"},
		all: []string{"flexinfer"},

		dependency: contracts.IncidentDependencyFlexInfer,
		reason:     "flexinfer-service-unavailable",
		retryable:  true,
	},
	{
		any: []string{
			"pod not found during reconciliation",
			"error dialing backend",
			"kubelet",
			"apiserver",
			"konnectivity",
		},

		dependency: contracts.IncidentDependencyKubernetes,
		reason:     "kubernetes-control-plane",
		retryable:  true,
	},
	{
		any: []string{
			"websocket: close",
			"backend unavailable",
			"broken pipe",
			"transport closed",
			"use of closed network connection",
		},

		dependency: contracts.IncidentDependencyMCPGateway,
		reason:     "mcp-gateway-transport",
		retryable:  true,
	},
	{
		any: []string{
			"create buildah pod",
			"buildah build failed",
			"ensure sandbox: generate dockerfile",
			"sandbox image",
			"pull image",
			"image build failed",
		},

		dependency: contracts.IncidentDependencySandbox,
		reason:     "sandbox-infrastructure",
		retryable:  false,
	},
	{
		any: []string{
			"no such host",
			"connection refused",
			"connection reset by peer",
			"i/o timeout",
			"unexpected eof",
			"context deadline exceeded",
			"status 502",
			"status 503",
			"status 504",
			"bad gateway",
			"service unavailable",
			"gateway timeout",
		},

		dependency: contracts.IncidentDependencyNetwork,
		reason:     "network-transport",
		retryable:  true,
	},
}

func unmatchedIncident() contracts.IncidentClassification {
	return contracts.IncidentClassification{
		Classifier: contracts.IncidentClassifierExternalDependency,
		Matched:    false,
	}
}

func matchedIncident(dependency contracts.IncidentDependency, reason string, retryable bool) contracts.IncidentClassification {
	return contracts.IncidentClassification{
		Classifier: contracts.IncidentClassifierExternalDependency,
		Matched:    true,
		Category:   contracts.IncidentCategoryExternalDependency,
		Dependency: dependency,
		Reason:     reason,
		Retryable:  retryable,
		Labels: []string{
			"kind/incident",
			"incident/external-dependency",
			"dependency/" + string(dependency),
			"incident_reason/" + reason,
			"retryable/" + boolLabel(retryable),
		},
	}
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
