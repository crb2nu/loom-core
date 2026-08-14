package guard

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// The real DAO satisfies both optional upgrades.
var (
	_ ActorPrefixLister = (*store.EventDAO)(nil)
	_ KindLister        = (*store.EventDAO)(nil)
)

// fakeFilteredEvents implements EventLister plus both filtered upgrades the
// way EventDAO does: the limit caps the FILTERED set. It seeds a firehose
// bigger than every report cap so the tests prove the upgrade path is what
// keeps the reports alive on a busy mill.
type fakeFilteredEvents struct {
	events []*store.Event
}

func (f *fakeFilteredEvents) list(since time.Time, limit int, keep func(*store.Event) bool) []*store.Event {
	out := make([]*store.Event, 0)
	for _, e := range f.events {
		if !e.OccurredAt.Before(since) && keep(e) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeFilteredEvents) ListSince(_ context.Context, since time.Time, limit int) ([]*store.Event, error) {
	return f.list(since, limit, func(*store.Event) bool { return true }), nil
}

func (f *fakeFilteredEvents) ListSinceByActorPrefix(_ context.Context, prefix string, since time.Time, limit int) ([]*store.Event, error) {
	return f.list(since, limit, func(e *store.Event) bool { return strings.HasPrefix(e.Actor, prefix) }), nil
}

func (f *fakeFilteredEvents) ListSinceByKinds(_ context.Context, kinds []string, since time.Time, limit int) ([]*store.Event, error) {
	set := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	return f.list(since, limit, func(e *store.Event) bool {
		_, ok := set[e.Kind]
		return ok
	}), nil
}

// firehoseEvents seeds promotionReportEventLimit+1 unrelated pipeline events
// plus a handful the reports actually aggregate. Millisecond spacing keeps
// the whole firehose inside the tests' one-hour window.
func firehoseEvents() *fakeFilteredEvents {
	f := &fakeFilteredEvents{}
	for i := 0; i < promotionReportEventLimit+1; i++ {
		f.events = append(f.events, ev("pipeline", "stage.started", "run", "r", time.Duration(i)*time.Millisecond))
	}
	// Audit kinds follow "<actor>.<action>[.dryrun]" (see splitActionKind).
	f.events = append(f.events,
		ev("overseer.foreman", "overseer.foreman.alert.dryrun", "anomaly", "a1", time.Minute),
		ev("overseer.foreman", "overseer.foreman.alert", "anomaly", "a2", 2*time.Minute),
	)
	return f
}

func TestBuildPromotionReport_FilteredListerSurvivesFirehose(t *testing.T) {
	f := firehoseEvents()
	rep, err := BuildPromotionReport(context.Background(), f, "overseer.", reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("filtered path must survive a saturated firehose: %v", err)
	}
	if rep.TotalDryRun != 1 || rep.TotalExecuted != 1 {
		t.Fatalf("want 1 dry-run + 1 executed, got %+v", rep)
	}
}

func TestBuildPromotionReport_FallbackStillFailsClosed(t *testing.T) {
	// Same firehose through a lister WITHOUT the upgrade: the cap must still
	// refuse to review a truncated count.
	f := &fakeEvents{events: firehoseEvents().events}
	_, err := BuildPromotionReport(context.Background(), f, "overseer.", reportNow.Add(-time.Hour), reportNow)
	if err == nil || !strings.Contains(err.Error(), "narrow the window") {
		t.Fatalf("fallback must fail closed on saturation, got err=%v", err)
	}
}

func TestBuildJudgeCalibration_FilteredListerSurvivesFirehose(t *testing.T) {
	f := firehoseEvents()
	rep, err := BuildJudgeCalibrationReport(context.Background(), f, &fakeRunLister{}, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("filtered path must survive a saturated firehose: %v", err)
	}
	if rep.TotalVerdicts != 0 {
		t.Fatalf("no verdict events seeded; got %d", rep.TotalVerdicts)
	}
}

func TestBuildConfigOutcomes_FilteredListerSurvivesFirehose(t *testing.T) {
	f := firehoseEvents()
	rep, err := BuildConfigOutcomeReport(context.Background(), f, &fakeRunLister{}, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("filtered path must survive a saturated firehose: %v", err)
	}
	if rep.StampedRuns != 0 {
		t.Fatalf("no provenance events seeded; got %d", rep.StampedRuns)
	}
}

var _ = mills.RunProvenanceEventKind // keep the import honest if kinds change
