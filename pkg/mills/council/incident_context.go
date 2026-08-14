package council

import (
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// IncidentContext is the council's normalized view of incident classification
// metadata. It is deliberately smaller than pipeline/store records: proposal
// planning needs the class, retry semantics, and dependency ownership, not the
// whole pipeline run.
type IncidentContext struct {
	Source               string
	RunID                string
	BacklogID            string
	BacklogTitle         string
	Classifier           string
	Class                CIIncidentClass
	FailureClass         string
	EscalationClass      string
	Disposition          CIIncidentDisposition
	ExternalDependencyID string
	ExternalDependency   string
	Retryable            *bool
	FreeRetry            *bool
	Terminal             *bool
	Evidence             string
	Reason               string
}

// IncidentContextsFromClassifiedCIFailures converts persisted ci_watch
// escalation summaries into council planning context.
func IncidentContextsFromClassifiedCIFailures(failures []*store.ClassifiedCIFailureSummary) []IncidentContext {
	if len(failures) == 0 {
		return nil
	}
	out := make([]IncidentContext, 0, len(failures))
	for _, f := range failures {
		if f == nil {
			continue
		}
		ctx := IncidentContext{
			Source:               "classified_ci_failure",
			RunID:                strings.TrimSpace(f.RunID),
			BacklogID:            strings.TrimSpace(f.BacklogID),
			BacklogTitle:         strings.TrimSpace(f.BacklogTitle),
			Classifier:           strings.TrimSpace(f.Classifier),
			Class:                canonicalClassifiedCIFailureClass(f),
			FailureClass:         strings.TrimSpace(f.FailureClass),
			EscalationClass:      strings.TrimSpace(f.EscalationClass),
			ExternalDependencyID: strings.TrimSpace(f.ExternalDependencyID),
			ExternalDependency:   strings.TrimSpace(f.ExternalDependency),
			Retryable:            f.Retryable,
			FreeRetry:            f.FreeRetry,
			Terminal:             f.Terminal,
		}
		out = append(out, NormalizeIncidentContext(ctx))
	}
	return out
}

// NormalizeIncidentContext fills derived planning semantics on an incident
// context while preserving caller-supplied evidence and source metadata.
func NormalizeIncidentContext(ctx IncidentContext) IncidentContext {
	if ctx.Class == "" {
		ctx.Class = classFromFailureMetadata(ctx.FailureClass, ctx.EscalationClass, ctx.ExternalDependency, ctx.ExternalDependencyID)
	}
	if ctx.ExternalDependency != "" || ctx.ExternalDependencyID != "" {
		ctx.Class = CIIncidentExternalDependency
	}
	if ctx.Disposition == "" {
		ctx.Disposition = dispositionForIncidentContext(ctx)
	}
	if ctx.Reason == "" {
		ctx.Reason = reasonForIncidentContext(ctx)
	}
	return ctx
}

// RenderIncidentPlanningContext renders incident classification metadata in a
// prompt-safe form for council proposal planning.
func RenderIncidentPlanningContext(contexts []IncidentContext) string {
	normalized := make([]IncidentContext, 0, len(contexts))
	for _, ctx := range contexts {
		ctx = NormalizeIncidentContext(ctx)
		if ctx.Class == "" || ctx.Class == CIIncidentUnclassified {
			continue
		}
		normalized = append(normalized, ctx)
	}
	if len(normalized) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Incident classification metadata available to this council run:\n\n")
	b.WriteString("Use these classifications as planning inputs. When class is `external_dependency_incident`, do not propose outside-system remediation; emit no proposal unless the follow-up changes repository-owned guardrails, classifiers, telemetry, docs, config, retry policy, or runbooks.\n\n")
	for _, ctx := range normalized {
		title := firstNonEmptyString(ctx.BacklogTitle, ctx.BacklogID, ctx.RunID, ctx.Source, "incident")
		fmt.Fprintf(&b, "- **%s**", title)
		if ctx.RunID != "" {
			fmt.Fprintf(&b, " run=`%s`", ctx.RunID)
		}
		if ctx.BacklogID != "" {
			fmt.Fprintf(&b, " backlog=`%s`", ctx.BacklogID)
		}
		fmt.Fprintf(&b, " class=`%s` disposition=`%s`", ctx.Class, ctx.Disposition)
		if ctx.Classifier != "" {
			fmt.Fprintf(&b, " classifier=`%s`", ctx.Classifier)
		}
		if ctx.FailureClass != "" {
			fmt.Fprintf(&b, " failure_class=`%s`", ctx.FailureClass)
		}
		if ctx.EscalationClass != "" {
			fmt.Fprintf(&b, " escalation_class=`%s`", ctx.EscalationClass)
		}
		if ctx.Retryable != nil {
			fmt.Fprintf(&b, " retryable=`%t`", *ctx.Retryable)
		}
		if ctx.FreeRetry != nil {
			fmt.Fprintf(&b, " free_retry=`%t`", *ctx.FreeRetry)
		}
		if ctx.Terminal != nil {
			fmt.Fprintf(&b, " terminal=`%t`", *ctx.Terminal)
		}
		if ctx.ExternalDependency != "" || ctx.ExternalDependencyID != "" {
			fmt.Fprintf(&b, " external=`%s`", firstNonEmptyString(ctx.ExternalDependency, ctx.ExternalDependencyID))
		}
		if ctx.Reason != "" {
			fmt.Fprintf(&b, ": %s", ctx.Reason)
		}
		if ctx.Evidence != "" {
			fmt.Fprintf(&b, " Evidence: `%s`", ctx.Evidence)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func classFromFailureMetadata(failureClass, escalationClass, dependency, dependencyID string) CIIncidentClass {
	if dependency != "" || dependencyID != "" {
		return CIIncidentExternalDependency
	}
	switch strings.TrimSpace(failureClass) {
	case "configuration":
		return CIIncidentCIConfiguration
	case "infrastructure":
		return CIIncidentRunnerInfrastructure
	case "code":
		return CIIncidentRepositoryRegression
	case "transient", "transient_quota":
		return CIIncidentFlakeOrTransient
	}
	switch strings.TrimSpace(escalationClass) {
	case "config":
		return CIIncidentCIConfiguration
	case "infra", "infrastructure":
		return CIIncidentRunnerInfrastructure
	case "code":
		return CIIncidentRepositoryRegression
	case "transient", "transient_quota":
		return CIIncidentFlakeOrTransient
	default:
		return CIIncidentUnclassified
	}
}

func dispositionForIncidentContext(ctx IncidentContext) CIIncidentDisposition {
	switch ctx.Class {
	case CIIncidentExternalDependency:
		return CIIncidentDispositionWaitDependency
	case CIIncidentRunnerInfrastructure:
		return CIIncidentDispositionEscalateRunner
	case CIIncidentCIConfiguration:
		return CIIncidentDispositionFixCIConfig
	case CIIncidentRepositoryRegression:
		return CIIncidentDispositionFixBranch
	case CIIncidentDependencyUpdate:
		return CIIncidentDispositionFixDependency
	case CIIncidentFlakeOrTransient:
		return CIIncidentDispositionRetryOnce
	case CIIncidentBranchOrPlanHygiene:
		return CIIncidentDispositionFixBranchHygiene
	default:
		return CIIncidentDispositionEscalateHuman
	}
}

func reasonForIncidentContext(ctx IncidentContext) string {
	switch ctx.Class {
	case CIIncidentExternalDependency:
		return "classification points to an external dependency outside repository ownership"
	case CIIncidentRunnerInfrastructure:
		return "classification points to runner, executor, or Kubernetes infrastructure"
	case CIIncidentCIConfiguration:
		return "classification points to CI configuration or guardrail behavior"
	case CIIncidentRepositoryRegression:
		return "classification points to branch-owned repository failure"
	case CIIncidentFlakeOrTransient:
		return "classification allows only bounded retry handling"
	default:
		return ""
	}
}
