package guard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// EventLister is the narrowest events read a promotion review needs: one
// window-bounded, newest-first scan. *store.EventDAO satisfies it.
type EventLister interface {
	ListSince(ctx context.Context, since time.Time, limit int) ([]*store.Event, error)
}

// ActorPrefixLister is the optional store upgrade the reports prefer: the
// same window scan filtered to an actor prefix at the query, so the
// truncation cap counts the reviewed actors' events rather than the whole
// firehose. A busy mill's pipeline bookkeeping alone exceeds the cap over a
// two-week window, which made every report fail closed while the audited
// rows numbered a few hundred. *store.EventDAO satisfies it; fakes that only
// implement EventLister keep the unfiltered fallback.
type ActorPrefixLister interface {
	ListSinceByActorPrefix(ctx context.Context, prefix string, since time.Time, limit int) ([]*store.Event, error)
}

// KindLister is the same optional upgrade keyed by event kind, for the
// reports that aggregate specific kinds (judge verdicts, provenance stamps)
// instead of actor families.
type KindLister interface {
	ListSinceByKinds(ctx context.Context, kinds []string, since time.Time, limit int) ([]*store.Event, error)
}

const (
	// promotionReportEventLimit bounds the window scan. Saturating it is an
	// error rather than a truncation: a promotion review that silently
	// under-counts executed actions is worse than no review.
	promotionReportEventLimit = 10000
	// promotionSubjectSampleSize caps the per-action subject sample. The
	// sample is for recognising WHAT was acted on; UniqueSubjects carries
	// the magnitude.
	promotionSubjectSampleSize = 10
)

// PromotionReport is the dry-run→promote review artifact: what the guarded
// actors under one actor prefix actually did in a window, split into soak
// decisions and committed actions. It is the evidence a human reads before
// flipping an agent's dry_run off.
type PromotionReport struct {
	ActorPrefix string    `json:"actor_prefix"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// TotalActions is every audited action in the window, dry-run plus
	// executed — the report's evidence VOLUME.
	TotalActions  int              `json:"total_actions"`
	TotalDryRun   int              `json:"total_dry_run"`
	TotalExecuted int              `json:"total_executed"`
	PerActor      []PromotionActor `json:"per_actor"`
	// ZeroEvidence marks a window that recorded nothing at all. A soak that
	// never acted passes every "no false positives" reading trivially, so
	// the absence of evidence has to be a stated finding rather than an
	// empty table a reviewer mistakes for a clean run.
	ZeroEvidence bool `json:"zero_evidence"`
}

// PromotionActor groups one actor's action rows.
type PromotionActor struct {
	Actor     string            `json:"actor"`
	PerAction []PromotionAction `json:"per_action"`
}

// PromotionAction is one actor's evidence for one action.
type PromotionAction struct {
	Action string `json:"action"`
	// DryRun and Executed count the same action recorded under its
	// ".dryrun" and committed kinds; a promotion window normally holds
	// both, and the ratio is the promotion question.
	DryRun   int `json:"dry_run"`
	Executed int `json:"executed"`
	// UniqueSubjects counts distinct subjects, so an action that retried
	// one item fifty times cannot read as broad coverage.
	UniqueSubjects int       `json:"unique_subjects"`
	SubjectSample  []string  `json:"subject_sample"`
	First          time.Time `json:"first"`
	Last           time.Time `json:"last"`
}

// promotionAgg accumulates one (actor, action) cell before it is sorted into
// the report.
type promotionAgg struct {
	dryRun      int
	executed    int
	subjects    map[string]struct{}
	first, last time.Time
}

// BuildPromotionReport aggregates the audited actions of every actor under
// actorPrefix over [since, now] into a promotion review artifact. Pure over
// the events surface: no store writes, no clock reads.
func BuildPromotionReport(ctx context.Context, events EventLister, actorPrefix string, since, now time.Time) (PromotionReport, error) {
	if events == nil {
		return PromotionReport{}, errors.New("promotion report: events lister required")
	}
	// An empty prefix would silently widen the report to every writer in the
	// events table, including the pipeline's own bookkeeping; a review names
	// the actors it is reviewing.
	if actorPrefix == "" {
		return PromotionReport{}, errors.New("promotion report: actor prefix required")
	}
	if !since.Before(now) {
		return PromotionReport{}, fmt.Errorf("promotion report: window start %s is not before end %s",
			since.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	// Prefer the prefix-filtered scan so the truncation cap below counts the
	// reviewed actors' events, not every writer in the table. The in-memory
	// HasPrefix filter stays: it also guards the fallback path.
	var raw []*store.Event
	var err error
	if pl, ok := events.(ActorPrefixLister); ok {
		raw, err = pl.ListSinceByActorPrefix(ctx, actorPrefix, since, promotionReportEventLimit)
	} else {
		raw, err = events.ListSince(ctx, since, promotionReportEventLimit)
	}
	if err != nil {
		return PromotionReport{}, fmt.Errorf("promotion report: %w", err)
	}
	if len(raw) >= promotionReportEventLimit {
		return PromotionReport{}, fmt.Errorf("promotion report: window holds at least %d events; narrow the window rather than review a truncated count", promotionReportEventLimit)
	}

	byActor := make(map[string]map[string]*promotionAgg)
	for _, e := range raw {
		if e == nil || !strings.HasPrefix(e.Actor, actorPrefix) {
			continue
		}
		// ListSince bounds the window's start; the end is bounded here so a
		// clock-skewed future event cannot land in a closed review window.
		if e.OccurredAt.Before(since) || e.OccurredAt.After(now) {
			continue
		}
		action, dry := splitActionKind(e.Actor, e.Kind)
		actions, ok := byActor[e.Actor]
		if !ok {
			actions = make(map[string]*promotionAgg)
			byActor[e.Actor] = actions
		}
		agg, ok := actions[action]
		if !ok {
			agg = &promotionAgg{subjects: make(map[string]struct{}), first: e.OccurredAt, last: e.OccurredAt}
			actions[action] = agg
		}
		if dry {
			agg.dryRun++
		} else {
			agg.executed++
		}
		if ref := subjectRef(e); ref != "" {
			agg.subjects[ref] = struct{}{}
		}
		if e.OccurredAt.Before(agg.first) {
			agg.first = e.OccurredAt
		}
		if e.OccurredAt.After(agg.last) {
			agg.last = e.OccurredAt
		}
	}

	rep := PromotionReport{
		ActorPrefix: actorPrefix,
		WindowStart: since.UTC(),
		WindowEnd:   now.UTC(),
		PerActor:    make([]PromotionActor, 0, len(byActor)),
	}
	for _, actor := range sortedKeys(byActor) {
		actions := byActor[actor]
		row := PromotionActor{Actor: actor, PerAction: make([]PromotionAction, 0, len(actions))}
		for _, action := range sortedKeys(actions) {
			agg := actions[action]
			row.PerAction = append(row.PerAction, PromotionAction{
				Action:         action,
				DryRun:         agg.dryRun,
				Executed:       agg.executed,
				UniqueSubjects: len(agg.subjects),
				SubjectSample:  subjectSample(agg.subjects),
				First:          agg.first.UTC(),
				Last:           agg.last.UTC(),
			})
			rep.TotalDryRun += agg.dryRun
			rep.TotalExecuted += agg.executed
		}
		rep.PerActor = append(rep.PerActor, row)
	}
	rep.TotalActions = rep.TotalDryRun + rep.TotalExecuted
	rep.ZeroEvidence = rep.TotalActions == 0
	return rep, nil
}

// splitActionKind decomposes an event kind into its action name and dry-run
// flag. ActionRecorder writes "<actor>.<action>" for committed actions and
// "<actor>.<action>.dryrun" for soak decisions, so a review must fold both
// back into one action row. A kind that does not carry its actor's prefix is
// reported verbatim and counted as executed rather than dropped: unexpected
// evidence is still evidence, and a strange action name is visible to the
// reviewer.
func splitActionKind(actor, kind string) (string, bool) {
	action, ok := strings.CutPrefix(kind, actor+".")
	if !ok {
		return kind, false
	}
	if trimmed, dry := strings.CutSuffix(action, ".dryrun"); dry {
		return trimmed, true
	}
	return action, false
}

// subjectRef renders an event's subject as a stable dedup key. Events without
// a subject (tick summaries) contribute no coverage and return "".
func subjectRef(e *store.Event) string {
	if e.SubjectID == "" {
		return ""
	}
	if e.SubjectKind == "" {
		return e.SubjectID
	}
	return e.SubjectKind + "/" + e.SubjectID
}

// subjectSample returns up to promotionSubjectSampleSize subjects in a stable
// order — the report is diffed across soak days, so the sample must not
// reshuffle on identical evidence.
func subjectSample(subjects map[string]struct{}) []string {
	out := make([]string, 0, len(subjects))
	for s := range subjects {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > promotionSubjectSampleSize {
		out = out[:promotionSubjectSampleSize]
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
