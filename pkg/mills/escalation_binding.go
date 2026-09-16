package mills

import (
	"context"
	"errors"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Escalation target binding. backlog_items.target_project is mutable — it may
// be edited (or retargeted) long after a run escalated — so nothing that
// authorizes external side effects may read it as evidence of where an
// escalated attempt actually ran. Runs that reached the mr stage carry durable
// stage provenance (PipelineDAO.AuthorizedProject); runs that escalated BEFORE
// the mr stage carry nothing. This event closes that gap: it is appended
// first-writer on the run's own event subject at the moment the run escalates
// with its item, freezing the item's resolved target project at escalation
// time in the append-only ledger. The ghost-spark merged-branch sweep later authorizes a
// cross-repo branch lookup against this binding, never against the live field.
const EscalationTargetBindingKind = "pipeline.run.escalation_target"

// AppendEscalationTargetBinding records the escalation-time target binding for
// a run, exactly once (first writer wins — a re-observation is a no-op, so a
// retried escalation cannot rewrite the frozen project). Returns whether this
// call appended the event. Nil arguments degrade to a no-op so callers without
// an events store keep their existing behavior.
func AppendEscalationTargetBinding(
	ctx context.Context,
	events *store.EventDAO,
	actor string,
	run *store.PipelineRun,
	item *store.BacklogItem,
	homeProject string,
) (bool, error) {
	if events == nil || run == nil || item == nil {
		return false, nil
	}
	targetProject := strings.TrimSpace(item.TargetProject)
	if targetProject == "" {
		targetProject = strings.TrimSpace(homeProject)
	}
	return events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       actor,
		Kind:        EscalationTargetBindingKind,
		SubjectKind: "pipeline_run",
		SubjectID:   run.ID,
		Payload: map[string]any{
			"backlog_id":     item.ID,
			"target_project": targetProject,
		},
	})
}

// escalationTargetBinding resolves the frozen escalation-time target project
// for a run. found=false means no binding was ever recorded (the run escalated
// before this event existed, or its escalation path predates emission) — the
// caller must fail closed for anything the binding would have authorized.
func escalationTargetBinding(ctx context.Context, events *store.EventDAO, runID string) (string, bool, error) {
	if events == nil {
		return "", false, nil
	}
	ev, err := events.FirstBySubjectKind(ctx, "pipeline_run", runID, EscalationTargetBindingKind)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	project, _ := ev.Payload["target_project"].(string)
	return strings.TrimSpace(project), true, nil
}

// escalationAuthorizedProject resolves the same immutable provenance arms used
// by ListEscalatedWithMR. Successful stage provenance remains authoritative;
// a rescue run that never reached the mr stage may instead use its frozen
// escalation-time binding. If both exist they must agree.
func escalationAuthorizedProject(ctx context.Context, pipeline *store.PipelineDAO, events *store.EventDAO, item *store.BacklogItem, homeProject, runID string) (string, error) {
	bound, haveBinding, err := escalationTargetBinding(ctx, events, runID)
	if err != nil {
		return "", err
	}
	if haveBinding && bound == "" {
		homeProject = strings.TrimSpace(homeProject)
		if item == nil || homeProject == "" || (strings.TrimSpace(item.TargetProject) != "" && !store.SameRepo(item.TargetProject, homeProject)) {
			return "", store.ErrPipelineProjectUnavailable
		}
		bound = homeProject
	}

	project, stageErr := pipeline.AuthorizedProject(ctx, runID)
	if stageErr == nil {
		if haveBinding && !store.SameRepo(project, bound) {
			return "", store.ErrPipelineProjectUnavailable
		}
		return project, nil
	}
	if !errors.Is(stageErr, store.ErrPipelineProjectUnavailable) || !haveBinding {
		return "", stageErr
	}

	// AuthorizedProject also reports "unavailable" for malformed or
	// conflicting artifacts. A binding may only fill the complete absence of
	// stage provenance, never paper over bad provenance.
	stages, err := pipeline.ListStages(ctx, runID)
	if err != nil {
		return "", err
	}
	for _, sr := range stages {
		if sr == nil || sr.Outcome == nil || *sr.Outcome != store.StageOutcomeSuccess {
			continue
		}
		key := ""
		switch sr.Stage {
		case "mr":
			key = "mr_project"
		case "ci_watch":
			key = "ci_project"
		case "merge":
			key = "merged_project"
		case "cleanup":
			key = "cleanup_project"
		}
		if key != "" {
			if _, exists := sr.Artifacts[key]; exists {
				return "", stageErr
			}
		}
	}
	return bound, nil
}
