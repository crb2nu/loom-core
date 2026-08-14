package crossrepo

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type recordingStampWriter struct {
	calls int
	stamp *store.Stamp
}

func (w *recordingStampWriter) Put(_ context.Context, stamp *store.Stamp) error {
	w.calls++
	w.stamp = stamp
	return nil
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
