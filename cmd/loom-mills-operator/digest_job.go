package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/finishing/digest"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// finishingDigestKind is both the event kind and the subject kind of the
// once-per-UTC-day digest row; subject_id is the day (YYYY-MM-DD), which is
// the key AppendOnceBySubjectKind dedupes on.
const finishingDigestKind = "finishing.digest"

// errDigestDayIncomplete guards the append-once invariant: a digest written
// before its UTC day ends would freeze a partial ledger as that day's only
// digest, so the job refuses days whose boundary is still in the future.
var errDigestDayIncomplete = errors.New("finishing digest: day has not ended yet (UTC)")

func validDigestAt(raw string) bool { _, _, err := parseDigestAt(raw); return err == nil }

func parseDigestAt(raw string) (int, int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(raw))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid digest time %q: expected HH:MM UTC", raw)
	}
	return t.Hour(), t.Minute(), nil
}

// digestSlot is the HH:MM UTC slot on now's UTC day. A malformed schedule
// falls back to the 06:00 default rather than silencing the job.
func digestSlot(now time.Time, raw string) time.Time {
	h, m, err := parseDigestAt(raw)
	if err != nil {
		h, m = 6, 0
	}
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
}

// nextDigestRun is the first slot strictly after now.
func nextDigestRun(now time.Time, raw string) time.Time {
	next := digestSlot(now, raw)
	if !next.After(now.UTC()) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// runFinishingDigest composes one completed UTC day through the shift-report
// composer, appends it once to the event ledger (on the dedicated ledger
// writer), and hands the committed event to the reconciler's digest and
// mobile push hooks. inserted reports whether this call wrote the row. An
// error with inserted=true means the digest IS stored and only hook delivery
// failed: the durable event is authoritative, so callers surface that
// instead of retrying a write append-once would refuse anyway.
func (o *operator) runFinishingDigest(ctx context.Context, day time.Time) (digest.Digest, bool, error) {
	day = day.UTC()
	end := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	if end.After(o.shiftNow().UTC()) {
		return digest.Digest{}, false, errDigestDayIncomplete
	}
	report, err := o.composeShiftReport(ctx, end, 24*time.Hour)
	if err != nil {
		mills.FinishingDigestTotal.WithLabelValues("error").Inc()
		return digest.Digest{}, false, fmt.Errorf("finishing digest %s: compose: %w", day.Format("2006-01-02"), err)
	}
	d := digest.Compose(digest.Input{Day: day, Report: report})
	payload, err := digestPayload(d)
	if err != nil {
		mills.FinishingDigestTotal.WithLabelValues("error").Inc()
		return digest.Digest{}, false, fmt.Errorf("finishing digest %s: payload: %w", d.Day, err)
	}
	e := store.Event{Actor: "finishing-digest", Kind: finishingDigestKind, SubjectKind: finishingDigestKind, SubjectID: d.Day, OccurredAt: end, Payload: payload}
	inserted, err := o.store.Events.AppendOnceBySubjectKind(ctx, &e)
	if err != nil {
		mills.FinishingDigestTotal.WithLabelValues("error").Inc()
		return digest.Digest{}, false, fmt.Errorf("finishing digest %s: append: %w", d.Day, err)
	}
	if !inserted {
		mills.FinishingDigestTotal.WithLabelValues("skipped").Inc()
		return d, false, nil
	}
	// The committed event lands where the operator already reads: the
	// agent-context digest feed and the mobile push channel. Both hooks are
	// optional on the reconciler; a delivery failure is reported, never
	// retried, because append-once will not write the day twice.
	if o.reconciler != nil {
		hooks := []struct {
			name string
			fn   func(context.Context, store.Event) error
		}{{"digest", o.reconciler.DigestEvent}, {"mobile_push", o.reconciler.MobilePushEvent}}
		for _, hook := range hooks {
			if hook.fn == nil {
				continue
			}
			if err := hook.fn(ctx, e); err != nil {
				mills.FinishingDigestTotal.WithLabelValues("error").Inc()
				return d, true, fmt.Errorf("finishing digest %s stored; %s hook delivery failed: %w", d.Day, hook.name, err)
			}
		}
	}
	mills.FinishingDigestTotal.WithLabelValues("composed").Inc()
	return d, true, nil
}

// digestPayload stores the digest as its own JSON shape, so the read path
// (digestFromEvent) and the event payload can never drift apart.
func digestPayload(d digest.Digest) (map[string]any, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// runDigestScheduler composes yesterday's digest at the configured UTC slot
// every day. It follows the housekeeping boot-phase ordering: main.go gates
// it behind the reconciler's boot tick, and the first compose is the next
// slot — never the boot path — so a cold store and the boot tick's budget
// are never contended by a 24-hour ledger read. A restart that straddles
// the slot leaves that day to the admin rerun (append-once keeps a rerun
// safe); it is deliberately not caught up here.
func (o *operator) runDigestScheduler(ctx context.Context) error {
	for {
		next := nextDigestRun(o.shiftNow(), o.digestAt)
		timer := time.NewTimer(next.Sub(o.shiftNow()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			o.digestTick(ctx, next.AddDate(0, 0, -1))
		}
	}
}

// digestTick runs one scheduled compose and logs the outcome. The scheduler
// never stops on a failed day: the next slot or an admin rerun can still
// fill it.
func (o *operator) digestTick(ctx context.Context, day time.Time) {
	d, inserted, err := o.runFinishingDigest(ctx, day)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		o.logger.Warn("finishing digest", "day", day.UTC().Format("2006-01-02"), "inserted", inserted, "error", err)
	case inserted:
		o.logger.Info("finishing digest composed", "day", d.Day, "bolts", d.Counts.Bolts, "sparks", d.Counts.Sparks, "pending_rollout", d.Counts.Pending)
	}
}
