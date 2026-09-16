package council

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

func init() {
	recurringInfrastructureSignatures = append(recurringInfrastructureSignatures,
		recurringInfrastructureSignature{
			dependency: "s3",
			pattern: regexp.MustCompile(
				`(?i)\blangfuse\b[\s\S]*\b(?:s3|bucket|object storage)\b[\s\S]*\b(?:error|fail(?:ed|ure)?|unavailable|timeout|timed out|connection refused|no such host)\b|` +
					`\b(?:error|fail(?:ed|ure)?|unavailable|timeout|timed out|connection refused|no such host)\b[\s\S]*\b(?:s3|bucket|object storage)\b[\s\S]*\blangfuse\b`,
			),
		},
		recurringInfrastructureSignature{
			dependency: "minio",
			pattern: regexp.MustCompile(
				`(?i)\bminio\b[\s\S]*\b(?:drive|disk)\b[\s\S]*\b(?:error|fail(?:ed|ure)?|faulty|offline|unavailable|not found|not accessible|I/O error)\b|` +
					`\b(?:error|fail(?:ed|ure)?|faulty|offline|unavailable|not found|not accessible|I/O error)\b[\s\S]*\b(?:drive|disk)\b[\s\S]*\bminio\b`,
			),
		},
		recurringInfrastructureSignature{
			dependency: "postgres",
			pattern: regexp.MustCompile(
				`(?i)\b(?:postgres(?:ql)?|psql|port\s+5432|:5432)\b[\s\S]*\bconnection refused\b|` +
					`\bconnection refused\b[\s\S]*\b(?:postgres(?:ql)?|psql|port\s+5432|:5432)\b`,
			),
		},
	)
}

// IntentsMissingMarker is the stable machine-readable marker stamped into a
// Council brief when the canonical roadmap-intent store is empty.
const IntentsMissingMarker = "<!-- loom:council intents_missing=true -->"

// ReviewerDegradedMarker is stamped into the editor-facing brief whenever at
// least one dispatched reviewer did not return usable output.
const ReviewerDegradedMarker = "<!-- loom:council reviewer_degraded=true -->"

// ReviewerOutcomeStatus is the closed result vocabulary for one reviewer
// route. Empty output is distinct from an execution failure.
type ReviewerOutcomeStatus string

const (
	ReviewerOutcomeOK      ReviewerOutcomeStatus = "ok"
	ReviewerOutcomeEmpty   ReviewerOutcomeStatus = "empty"
	ReviewerOutcomeError   ReviewerOutcomeStatus = "error"
	ReviewerOutcomeTimeout ReviewerOutcomeStatus = "timeout"
)

// ReviewerOutcomeClassification is the bounded operational classification
// attached to a reviewer result.
type ReviewerOutcomeClassification string

const (
	ReviewerClassificationNone                       ReviewerOutcomeClassification = "none"
	ReviewerClassificationFailure                    ReviewerOutcomeClassification = "reviewer_failure"
	ReviewerClassificationExternalDependencyIncident ReviewerOutcomeClassification = "external_dependency_incident"
)

// ReviewerOutcome is the deterministic, persistable projection of a raw
// reviewer response. It intentionally omits raw errors and review text.
type ReviewerOutcome struct {
	Reviewer       string
	Status         ReviewerOutcomeStatus
	Classification ReviewerOutcomeClassification
}

// EvaluateReviewerOutcomes records exactly one telemetry observation for each
// supplied output and fails closed unless at least one reviewer returned
// non-whitespace Markdown. Returned outcomes are sorted by reviewer name.
func EvaluateReviewerOutcomes(ctx context.Context, outputs []ReviewerOutput) ([]ReviewerOutcome, error) {
	outcomes := make([]ReviewerOutcome, 0, len(outputs))
	successes := 0
	for _, output := range outputs {
		outcome := classifyReviewerOutcome(output)
		if outcome.Status == ReviewerOutcomeOK {
			successes++
		}
		telemetry.RecordCouncilReviewerOutcome(ctx, string(outcome.Status), string(outcome.Classification))
		outcomes = append(outcomes, outcome)
	}
	sort.SliceStable(outcomes, func(i, j int) bool { return outcomes[i].Reviewer < outcomes[j].Reviewer })
	if successes == 0 {
		return outcomes, fmt.Errorf("council: no reviewer produced a successful non-empty result (%d routes)", len(outputs))
	}
	return outcomes, nil
}

func classifyReviewerOutcome(output ReviewerOutput) ReviewerOutcome {
	outcome := ReviewerOutcome{
		Reviewer:       output.Lens.Name,
		Status:         ReviewerOutcomeOK,
		Classification: ReviewerClassificationNone,
	}
	if output.Err == nil {
		if strings.TrimSpace(output.Markdown) == "" {
			outcome.Status = ReviewerOutcomeEmpty
		}
		return outcome
	}
	if errors.Is(output.Err, context.DeadlineExceeded) {
		outcome.Status = ReviewerOutcomeTimeout
	} else {
		outcome.Status = ReviewerOutcomeError
	}
	if isReviewerAuthOrQuotaError(output.Err) {
		outcome.Classification = ReviewerClassificationExternalDependencyIncident
	} else {
		outcome.Classification = ReviewerClassificationFailure
	}
	return outcome
}

type reviewerHTTPStatusError interface{ StatusCode() int }

var reviewerAuthQuotaPattern = regexp.MustCompile(`(?i)(?:\b401\b|\b403\b|\b429\b|unauthori[sz]ed|forbidden|invalid (?:api )?key|authentication (?:failed|required)|quota (?:exceeded|exhausted)|rate limit|resource exhausted)`)

func isReviewerAuthOrQuotaError(err error) bool {
	var statusErr reviewerHTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode() {
		case 401, 403, 429:
			return true
		}
	}
	return reviewerAuthQuotaPattern.MatchString(err.Error())
}

// RenderReviewerStatusTable renders stable, bounded reviewer status rows for
// inclusion in a Council brief. Reviewer names are escaped as table data.
func RenderReviewerStatusTable(outcomes []ReviewerOutcome) string {
	ordered := append([]ReviewerOutcome(nil), outcomes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Reviewer < ordered[j].Reviewer })
	var b strings.Builder
	b.WriteString("| Reviewer | Status | Classification |\n")
	b.WriteString("|---|---|---|\n")
	for _, outcome := range ordered {
		name := strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ").Replace(outcome.Reviewer)
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, outcome.Status, outcome.Classification)
	}
	return b.String()
}

// ApplyReviewerOutcomes makes reviewer health visible to every Editor
// implementation without requiring each provider adapter to duplicate the
// rendering contract. Repeated dispatches replace the prior block.
func ApplyReviewerOutcomes(brief *Brief, outcomes []ReviewerOutcome) {
	if brief == nil {
		return
	}
	degraded := false
	for _, outcome := range outcomes {
		if outcome.Status != ReviewerOutcomeOK {
			degraded = true
			break
		}
	}

	const heading = "\n\n## Reviewer Dispatch Status\n\n"
	if at := strings.Index(brief.Markdown, heading); at >= 0 {
		brief.Markdown = brief.Markdown[:at]
	}
	if degraded {
		brief.Markdown += heading + ReviewerDegradedMarker + "\n" + RenderReviewerStatusTable(outcomes)
	}
}

type roadmapIntentLister interface {
	List(context.Context) ([]*store.RoadmapIntent, error)
}

// preflightRoadmapIntents checks the canonical intent store before the rest of
// the planning brief is assembled. Store failures abort planning; an empty
// successful result is explicitly marked and counted.
func preflightRoadmapIntents(ctx context.Context, intents roadmapIntentLister) ([]*store.RoadmapIntent, bool, error) {
	items, err := intents.List(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("brief: list roadmap: %w", err)
	}
	if len(items) != 0 {
		return items, false, nil
	}
	telemetry.RecordCouncilIntentsMissing()
	return items, true, nil
}
