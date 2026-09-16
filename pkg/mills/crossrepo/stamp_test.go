package crossrepo

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type recordingStampWriter struct {
	calls int
	stamp *store.Stamp
	err   error
}

type recordingBackfiller struct {
	calls  int
	id     string
	target string
}

func (b *recordingBackfiller) BackfillTargetProject(_ context.Context, id, target string) (bool, error) {
	b.calls++
	b.id, b.target = id, target
	return true, nil
}

func (w *recordingStampWriter) Put(_ context.Context, stamp *store.Stamp) error {
	w.calls++
	w.stamp = stamp
	return w.err
}

func TestPersistStampRejectsBlankTargetBeforeWrite(t *testing.T) {
	for _, target := range []string{"", " ", "\t\n"} {
		writer := &recordingStampWriter{}
		if _, err := PersistStamp(context.Background(), writer, "stamp-widget", target); err == nil {
			t.Fatalf("PersistStamp target %q succeeded", target)
		}
		if writer.calls != 0 {
			t.Fatalf("writer called %d times for target %q", writer.calls, target)
		}
	}
}

func TestPersistStampRejectsBlankIDBeforeWrite(t *testing.T) {
	for _, id := range []string{"", " ", "\t\n"} {
		writer := &recordingStampWriter{}
		if _, err := PersistStamp(context.Background(), writer, id, "services/widgets"); err == nil {
			t.Fatalf("PersistStamp ID %q succeeded", id)
		}
		if writer.calls != 0 {
			t.Fatalf("writer called %d times for ID %q", writer.calls, id)
		}
	}
}

func TestPersistStampSuppliesNormalizedTarget(t *testing.T) {
	writer := &recordingStampWriter{}
	stamp, err := PersistStamp(context.Background(), writer, " stamp-widget ", " services/widgets ")
	if err != nil {
		t.Fatalf("PersistStamp: %v", err)
	}
	if writer.calls != 1 || writer.stamp != stamp {
		t.Fatalf("writer = calls:%d stamp:%p, want one call with %p", writer.calls, writer.stamp, stamp)
	}
	if stamp.ID != "stamp-widget" || stamp.TargetProject != "services/widgets" {
		t.Fatalf("stamp = %+v", stamp)
	}
}

type identityStampWriter struct {
	stamps map[string]store.Stamp
}

func (w *identityStampWriter) Put(_ context.Context, stamp *store.Stamp) error {
	key := stamp.TargetProject + "\x00" + stamp.ID
	if _, exists := w.stamps[key]; exists {
		return store.ErrStampCollision
	}
	w.stamps[key] = *stamp
	return nil
}

func TestPersistStampIdentityIncludesTargetProject(t *testing.T) {
	writer := &identityStampWriter{stamps: make(map[string]store.Stamp)}
	first, err := PersistStamp(context.Background(), writer, " pattern-rest ", " services/widgets ")
	if err != nil {
		t.Fatalf("first PersistStamp: %v", err)
	}
	second, err := PersistStamp(context.Background(), writer, "pattern-rest", "services/catalog")
	if err != nil {
		t.Fatalf("second PersistStamp: %v", err)
	}
	if first.ID != second.ID || first.TargetProject == second.TargetProject || len(writer.stamps) != 2 {
		t.Fatalf("stamps = first:%+v second:%+v stored:%+v", first, second, writer.stamps)
	}
}

func TestPersistStampCollisionLeavesOriginalUnchanged(t *testing.T) {
	writer := &identityStampWriter{stamps: make(map[string]store.Stamp)}
	original, err := PersistStamp(context.Background(), writer, "pattern-rest", "services/widgets")
	if err != nil {
		t.Fatalf("original PersistStamp: %v", err)
	}
	original.CreatedAt = original.CreatedAt.Add(24 * time.Hour)

	_, err = PersistStamp(context.Background(), writer, " pattern-rest ", " services/widgets ")
	if !errors.Is(err, ErrStampCollision) {
		t.Fatalf("collision error = %v, want ErrStampCollision", err)
	}
	got := writer.stamps["services/widgets\x00pattern-rest"]
	if !got.CreatedAt.IsZero() || len(writer.stamps) != 1 {
		t.Fatalf("stored stamp changed after collision: %+v", got)
	}
}

func TestPersistStampDoesNotMisclassifyWriterError(t *testing.T) {
	want := errors.New("disk unavailable")
	writer := &recordingStampWriter{err: fmt.Errorf("write stamp: %w", want)}
	_, err := PersistStamp(context.Background(), writer, "pattern-rest", "services/widgets")
	if !errors.Is(err, want) || errors.Is(err, ErrStampCollision) {
		t.Fatalf("PersistStamp error = %v", err)
	}
}

func TestBackfillLegacyTargetUsesSourceAndIsIdempotent(t *testing.T) {
	stamp := &store.Stamp{ID: " legacy ", TargetProject: " \t"}
	backfiller := &recordingBackfiller{}
	updated, err := BackfillLegacyTarget(context.Background(), backfiller, stamp, " services/loom-core ")
	if err != nil {
		t.Fatalf("BackfillLegacyTarget: %v", err)
	}
	if !updated || backfiller.calls != 1 || backfiller.id != "legacy" || backfiller.target != "services/loom-core" {
		t.Fatalf("backfill = updated:%v calls:%d id:%q target:%q", updated, backfiller.calls, backfiller.id, backfiller.target)
	}
	updated, err = BackfillLegacyTarget(context.Background(), backfiller, stamp, "services/loom-core")
	if err != nil {
		t.Fatalf("second BackfillLegacyTarget: %v", err)
	}
	if updated || backfiller.calls != 1 || stamp.TargetProject != "services/loom-core" {
		t.Fatalf("second backfill = updated:%v calls:%d stamp:%+v, want no-op", updated, backfiller.calls, stamp)
	}
}
