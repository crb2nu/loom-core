package council

import (
	"regexp"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// WorkspaceSignalRule describes one deterministic infrastructure signature.
// ID is stable so the operator runbook can prove it documents every rule.
type WorkspaceSignalRule struct {
	ID                 string
	NamespacePattern   string
	LogPattern         string
	ExternalDependency string

	namespace *regexp.Regexp
	log       *regexp.Regexp
}

func newWorkspaceSignalRule(id, namespacePattern, logPattern, dependency string) WorkspaceSignalRule {
	return WorkspaceSignalRule{
		ID:                 id,
		NamespacePattern:   namespacePattern,
		LogPattern:         logPattern,
		ExternalDependency: dependency,
		namespace:          regexp.MustCompile(namespacePattern),
		log:                regexp.MustCompile(logPattern),
	}
}

var infrastructureWorkspaceSignalRules = []WorkspaceSignalRule{
	newWorkspaceSignalRule(
		"gitlab-service-unavailable",
		`(?i)(^|[/_.-])(gitlab|gitlab-runner)([/_.-]|$)`,
		`(?i)(?:\b(?:gitlab\.com|gitlab)\b.*(?:status (?:401|403|429|5\d\d)\b|connection refused|i/o timeout|tls handshake timeout|no such host)|(?:status (?:401|403|429|5\d\d)\b|connection refused|i/o timeout|tls handshake timeout|no such host).*\b(?:gitlab\.com|gitlab)\b)`,
		"gitlab",
	),
	newWorkspaceSignalRule(
		"kubernetes-control-plane-unavailable",
		`(?i)(^|[/_.-])(kube-system|kubernetes|k8s)([/_.-]|$)`,
		`(?i)(?:kubernetes|kube-apiserver|api server).*(?:connection refused|i/o timeout|tls handshake timeout|no route to host|service unavailable|status 5\d\d)\b`,
		"kubernetes",
	),
	newWorkspaceSignalRule(
		"flux-upstream-unavailable",
		`(?i)(^|[/_.-])flux-system([/_.-]|$)`,
		`(?i)(?:gitlab|github|source-controller).*(?:connection refused|i/o timeout|tls handshake timeout|no such host|status (?:429|5\d\d))\b`,
		"git_provider",
	),
	newWorkspaceSignalRule(
		"observability-backend-unavailable",
		`(?i)(^|[/_.-])(monitoring|observability|loki|prometheus|grafana)([/_.-]|$)`,
		`(?i)(?:loki|prometheus|grafana).*(?:connection refused|i/o timeout|tls handshake timeout|no such host|service unavailable|status 5\d\d)\b`,
		"observability",
	),
}

// InfrastructureWorkspaceSignalRules returns the public, immutable projection
// of the rule table. Compiled regular expressions remain an implementation
// detail; callers can use the source patterns for documentation sync checks.
func InfrastructureWorkspaceSignalRules() []WorkspaceSignalRule {
	out := make([]WorkspaceSignalRule, len(infrastructureWorkspaceSignalRules))
	copy(out, infrastructureWorkspaceSignalRules)
	for i := range out {
		out[i].namespace = nil
		out[i].log = nil
	}
	return out
}

// ClassifyInfrastructureWorkspaceSignal stamps known infrastructure clusters
// with the store's durable external-dependency incident class. A rule requires
// both the namespace/service attribution and log signature, avoiding broad
// keyword matches against ordinary repository failures.
func ClassifyInfrastructureWorkspaceSignal(signal WorkspaceSignal) (WorkspaceSignal, bool) {
	if signal.IncidentClass != "" {
		return signal, false
	}
	namespace := strings.TrimSpace(signal.Service)
	logLine := strings.TrimSpace(signal.Sample)
	for _, rule := range infrastructureWorkspaceSignalRules {
		if rule.namespace.MatchString(namespace) && rule.log.MatchString(logLine) {
			signal.IncidentClass = CIIncidentClass(store.IncidentClassExternalDependency)
			signal.ExternalDependency = rule.ExternalDependency
			return signal, true
		}
	}
	return signal, false
}

// ClassifyExternalWorkspaceSignals returns a classified copy of signals. The
// infrastructure table runs first; the existing general external-incident
// classifier remains as a compatibility fallback for non-infrastructure
// provider evidence.
func ClassifyExternalWorkspaceSignals(signals []WorkspaceSignal) []WorkspaceSignal {
	if len(signals) == 0 {
		return nil
	}
	out := make([]WorkspaceSignal, 0, len(signals))
	for _, signal := range signals {
		if classified, ok := ClassifyRecurringInfrastructureWorkspaceSignal(signal); ok {
			out = append(out, classified)
			continue
		}
		if classified, ok := ClassifyInfrastructureWorkspaceSignal(signal); ok {
			out = append(out, classified)
			continue
		}
		if signal.IncidentClass == "" {
			decision := ClassifyExternalIncidentPlanning(ExternalIncidentPlanningInput{
				Source:     signal.Source,
				Title:      signal.Service,
				Body:       signal.Sample,
				Service:    signal.Service,
				LogExcerpt: signal.Sample,
			})
			if decision.Class == CIIncidentExternalDependency {
				signal.IncidentClass = CIIncidentClass(store.IncidentClassExternalDependency)
				signal.ExternalDependency = firstNonEmpty(signal.ExternalDependency, decision.Dependency)
			}
		}
		out = append(out, signal)
	}
	return out
}
