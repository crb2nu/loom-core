package policy

import "strings"

// StabilityItem is a planning candidate scored while Mills is in
// stability-first mode.
type StabilityItem struct {
	ID       string
	Title    string
	Labels   []string
	Files    []string
	Priority int
}

// StabilityDecision explains how strongly an item should be pulled forward.
type StabilityDecision struct {
	Remediation bool
	Score       int
	Reasons     []string
}

// PrioritizeStabilityFirst promotes remediation and infrastructure-health work
// over feature work while keeping the result deterministic and explainable.
func PrioritizeStabilityFirst(item StabilityItem) StabilityDecision {
	d := StabilityDecision{Score: item.Priority}
	for _, label := range item.Labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "remediation", "incident", "infra", "infrastructure", "health-gate", "stability":
			d.Remediation = true
			d.Score += 100
			d.Reasons = append(d.Reasons, "stability label "+label)
		}
	}
	for _, f := range item.Files {
		path := strings.ToLower(strings.TrimSpace(f))
		switch {
		case strings.Contains(path, "gates/health"),
			strings.Contains(path, "preflight"),
			strings.Contains(path, "k8s/"),
			strings.Contains(path, "monitor/"),
			strings.Contains(path, "alerting/"):
			d.Remediation = true
			d.Score += 25
			d.Reasons = append(d.Reasons, "stability path "+f)
		}
	}
	if strings.Contains(strings.ToLower(item.Title), "health") || strings.Contains(strings.ToLower(item.Title), "remediation") {
		d.Remediation = true
		d.Score += 50
		d.Reasons = append(d.Reasons, "stability title")
	}
	return d
}
