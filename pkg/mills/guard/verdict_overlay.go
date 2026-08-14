package guard

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Verdict overlay (Trustworthy Verdicts S2, .loom/135). The learning-loop
// reports partition runs by their current VERDICT, not their frozen terminal
// class: an escalation later superseded (its MR merged — false escalation or
// human-rescued work) must stop counting as an escalation, or every rate the
// factory learns from trains on a resolved incident. Corrections are read
// from the same event scan the reports already perform.

// correctedRunIDs collects the run IDs whose escalation verdict was
// superseded inside [since, now]: explicit run.verdict.* events plus the
// reconciler's legacy ghost-spark closure event (see mills.ResolveRunVerdict
// — same recognition, bulk form). Callers apply it only to runs whose
// terminal state is escalated, mirroring the resolver's guard.
func correctedRunIDs(raw []*store.Event, since, now time.Time) map[string]struct{} {
	out := make(map[string]struct{})
	for _, e := range raw {
		if e == nil || e.SubjectID == "" {
			continue
		}
		if e.OccurredAt.Before(since) || e.OccurredAt.After(now) {
			continue
		}
		if strings.HasPrefix(e.Kind, mills.RunVerdictEventKindPrefix) ||
			e.Kind == mills.GhostSparkClosedEventKind {
			out[e.SubjectID] = struct{}{}
		}
	}
	return out
}
