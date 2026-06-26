package council

import (
	"context"
	"time"
)

// CompositeSignalSource fans a brief signal fetch out across multiple
// WorkspaceSignalSources (e.g. Loki error clusters + GitLab CI failures) and
// concatenates their results. Best-effort per source: one source erroring does
// not suppress the others, so a Loki blip never hides CI pain (and vice-versa).
type CompositeSignalSource struct {
	sources []WorkspaceSignalSource
}

// NewCompositeSignals returns a source over the non-nil inputs. It collapses to:
// nil when none are usable (the brief omits the section), the single source
// directly when only one, or a composite otherwise. Callers should pass only
// concrete non-nil clients (a typed-nil interface still passes the != nil check
// here, but every WorkspaceSignalSource implementation guards a nil receiver).
func NewCompositeSignals(sources ...WorkspaceSignalSource) WorkspaceSignalSource {
	nz := make([]WorkspaceSignalSource, 0, len(sources))
	for _, s := range sources {
		if s != nil {
			nz = append(nz, s)
		}
	}
	switch len(nz) {
	case 0:
		return nil
	case 1:
		return nz[0]
	default:
		return &CompositeSignalSource{sources: nz}
	}
}

// RecentErrorClusters concatenates clusters from every source. A source error
// is skipped (best-effort) rather than failing the whole fetch.
func (c *CompositeSignalSource) RecentErrorClusters(ctx context.Context, since time.Time) ([]WorkspaceSignal, error) {
	if c == nil {
		return nil, nil
	}
	var all []WorkspaceSignal
	for _, s := range c.sources {
		sigs, err := s.RecentErrorClusters(ctx, since)
		if err != nil {
			continue
		}
		all = append(all, sigs...)
	}
	return all, nil
}
