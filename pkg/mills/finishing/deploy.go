// Package finishing reports whether merged Mills work is present in the running operator build.
package finishing

import (
	"context"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type DeployState string

const (
	DeployLive          DeployState = "live"
	DeployPending       DeployState = "pending"
	DeployUnknown       DeployState = "unknown"
	DeployNotApplicable DeployState = "not_applicable"
)

type DeployCheck struct {
	State                      DeployState
	MergeSHA, BuildSHA, Reason string
}
type DeployProbe struct {
	RepoRoot, BuildSHA string
	IsAncestor         func(context.Context, string, string, string) (bool, error)
}

// Check is fail-soft: inability to inspect git becomes unknown evidence.
func (p DeployProbe) Check(ctx context.Context, mergeSHA string) DeployCheck {
	mergeSHA, buildSHA := strings.TrimSpace(mergeSHA), strings.TrimSpace(p.BuildSHA)
	check := DeployCheck{State: DeployNotApplicable, MergeSHA: mergeSHA, BuildSHA: buildSHA}
	if mergeSHA == "" || buildSHA == "" {
		return check
	}
	if p.IsAncestor == nil {
		check.State, check.Reason = DeployUnknown, "deployment ancestry probe is not configured"
		return check
	}
	live, err := p.IsAncestor(ctx, p.RepoRoot, mergeSHA, buildSHA)
	if err != nil {
		check.State, check.Reason = DeployUnknown, fmt.Sprintf("deployment ancestry unavailable: %v", err)
		return check
	}
	if live {
		check.State = DeployLive
	} else {
		check.State = DeployPending
	}
	return check
}

// DeploymentEvidence is the single reader of deployment stage artifacts.
func DeploymentEvidence(stages []*store.StageResult) (operatorChange bool, mergeSHA string) {
	for _, stage := range stages {
		if stage == nil || stage.Artifacts == nil {
			continue
		}
		if raw, ok := stage.Artifacts["files_changed"]; ok {
			for _, path := range artifactStrings(raw) {
				if strings.HasPrefix(path, "pkg/mills/") || strings.HasPrefix(path, "cmd/loom-mills-operator/") {
					operatorChange = true
				}
			}
		}
		if raw, ok := stage.Artifacts["merged_sha"].(string); ok && strings.TrimSpace(raw) != "" {
			mergeSHA = strings.TrimSpace(raw)
		}
	}
	return operatorChange, mergeSHA
}

func artifactStrings(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
