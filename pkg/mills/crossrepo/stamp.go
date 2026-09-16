package crossrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// StampWriter is the narrow persistence contract used by the cross-repository
// stamp boundary. Keeping the actor/auth context out of this interface leaves
// room for the authorization slice to wrap writes without weakening target
// enforcement here.
type StampWriter interface {
	Put(context.Context, *store.Stamp) error
}

// ErrStampCollision means the normalized (target project, pattern ID) tuple
// already exists. It aliases the persistence sentinel so errors.Is works at
// both the crossrepo API and store boundaries.
var ErrStampCollision = store.ErrStampCollision

// LegacyStampBackfiller persists the resolved target for a legacy target-less
// stamp. Implementations must update only rows whose target is still empty.
type LegacyStampBackfiller interface {
	BackfillTargetProject(context.Context, string, string) (bool, error)
}

// BackfillLegacyTarget resolves a legacy target-less stamp to its source
// repository and persists that resolution. It is idempotent: stamps that
// already have a target cause no write.
func BackfillLegacyTarget(ctx context.Context, backfiller LegacyStampBackfiller, stamp *store.Stamp, sourceProject string) (bool, error) {
	if stamp == nil {
		return false, errors.New("crossrepo: stamp required for backfill")
	}
	if strings.TrimSpace(stamp.TargetProject) != "" {
		stamp.TargetProject = strings.TrimSpace(stamp.TargetProject)
		return false, nil
	}
	sourceProject = strings.TrimSpace(sourceProject)
	if sourceProject == "" {
		return false, errors.New("crossrepo: source project required for stamp backfill")
	}
	if backfiller == nil {
		return false, errors.New("crossrepo: stamp backfiller required")
	}
	updated, err := backfiller.BackfillTargetProject(ctx, strings.TrimSpace(stamp.ID), sourceProject)
	if err != nil {
		return false, err
	}
	stamp.TargetProject = sourceProject
	return updated, nil
}

// NewStamp constructs a target-bound stamp. It performs the same fail-closed
// validation as PersistStamp, allowing callers to validate before acquiring a
// writer or starting a larger transaction.
func NewStamp(id, targetProject string) (*store.Stamp, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("crossrepo: stamp ID required")
	}
	targetProject = strings.TrimSpace(targetProject)
	if targetProject == "" {
		return nil, errors.New("crossrepo: stamp target project required")
	}
	return &store.Stamp{ID: id, TargetProject: targetProject}, nil
}

// PersistStamp validates and writes one target-bound stamp. Empty and
// whitespace-only destinations are rejected before the writer is invoked.
func PersistStamp(ctx context.Context, writer StampWriter, id, targetProject string) (*store.Stamp, error) {
	stamp, err := NewStamp(id, targetProject)
	if err != nil {
		return nil, err
	}
	if writer == nil {
		return nil, errors.New("crossrepo: stamp writer required")
	}
	if err := writer.Put(ctx, stamp); err != nil {
		if errors.Is(err, store.ErrStampCollision) {
			return nil, fmt.Errorf("crossrepo: stamp %q for %q: %w", stamp.ID, stamp.TargetProject, ErrStampCollision)
		}
		return nil, err
	}
	return stamp, nil
}
