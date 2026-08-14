package crossrepo

import (
	"context"
	"errors"
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
		return nil, err
	}
	return stamp, nil
}
