package council

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type roadmapIntentListerFunc func(context.Context) ([]*store.RoadmapIntent, error)

func (f roadmapIntentListerFunc) List(ctx context.Context) ([]*store.RoadmapIntent, error) {
	return f(ctx)
}

func TestIntentStorePreflightEmptyMarksBriefAndRecordsTelemetry(t *testing.T) {
	before := telemetry.CouncilIntentsMissingTotal()
	items, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return nil, nil },
	))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(items) != 0 || !missing {
		t.Fatalf("items = %v, missing = %v; want empty, true", items, missing)
	}

	body := renderIntents(items)
	if missing {
		body = IntentsMissingMarker + "\n" + body
	}
	if got := strings.Count(body, IntentsMissingMarker); got != 1 {
		t.Fatalf("marker count = %d, want 1 in %q", got, body)
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 1 {
		t.Fatalf("counter delta = %d, want 1", got)
	}
}

func TestIntentStorePreflightPopulatedIsUnchanged(t *testing.T) {
	want := []*store.RoadmapIntent{{ID: 1, Summary: "Ship the preflight"}}
	before := telemetry.CouncilIntentsMissingTotal()
	items, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return want, nil },
	))
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if missing {
		t.Fatal("populated store reported missing")
	}
	if len(items) != 1 || items[0] != want[0] {
		t.Fatalf("items = %#v, want unchanged %#v", items, want)
	}
	if strings.Contains(renderIntents(items), IntentsMissingMarker) {
		t.Fatal("populated intent rendering contains missing marker")
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 0 {
		t.Fatalf("counter delta = %d, want 0", got)
	}
}

func TestIntentStorePreflightStoreError(t *testing.T) {
	wantErr := errors.New("store unavailable")
	before := telemetry.CouncilIntentsMissingTotal()
	_, missing, err := preflightRoadmapIntents(context.Background(), roadmapIntentListerFunc(
		func(context.Context) ([]*store.RoadmapIntent, error) { return nil, wantErr },
	))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
	if missing {
		t.Fatal("store error reported missing")
	}
	if got := telemetry.CouncilIntentsMissingTotal() - before; got != 0 {
		t.Fatalf("counter delta = %d, want 0", got)
	}
}
