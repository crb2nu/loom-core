package council

import (
	"context"
	"fmt"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// IntentsMissingMarker is the stable machine-readable marker stamped into a
// Council brief when the canonical roadmap-intent store is empty.
const IntentsMissingMarker = "<!-- loom:council intents_missing=true -->"

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
