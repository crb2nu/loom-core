package pm

import (
	"context"
	"errors"
	"strings"

	"github.com/crb2nu/loom/pkg/agentcontext"
)

// Store is the persistence boundary for risks. It is intentionally narrow so
// unit tests can inject a fake without a live Qdrant.
type Store interface {
	// EnsureReady creates/validates the backing collection.
	EnsureReady(ctx context.Context) error
	// Upsert stores (creates or replaces) a risk with the given vector.
	Upsert(ctx context.Context, r Risk, vector []float64) error
	// Get fetches a risk by ID. Returns (nil, nil) when not found.
	Get(ctx context.Context, id string) (*Risk, error)
	// List returns risks filtered by optional project and status (empty = no
	// filter on that field).
	List(ctx context.Context, project, status string, limit int) ([]Risk, error)
}

// QdrantStore is the production Store backed by a collection-scoped
// agentcontext.QdrantClient bound to the pm_risks collection.
type QdrantStore struct {
	q          *agentcontext.QdrantClient
	vectorSize int
}

// NewQdrantStore constructs a QdrantStore from config. The client is a plain
// QdrantClient pinned to the risks collection — the same client type
// agent-context uses for tasks/sessions.
func NewQdrantStore(cfg Config) *QdrantStore {
	q := agentcontext.NewQdrantClient(
		httpClient(),
		cfg.QdrantURL,
		cfg.QdrantAPIKey,
		cfg.Collection,
		cfg.QdrantDistance,
	)
	return &QdrantStore{q: q, vectorSize: VectorSize}
}

func (s *QdrantStore) EnsureReady(ctx context.Context) error {
	return s.q.EnsureCollection(ctx, s.vectorSize)
}

func (s *QdrantStore) Upsert(ctx context.Context, r Risk, vector []float64) error {
	return s.q.Upsert(ctx, []agentcontext.Point{{
		ID:      r.ID,
		Vector:  vector,
		Payload: riskToPayload(r),
	}}, true)
}

func (s *QdrantStore) Get(ctx context.Context, id string) (*Risk, error) {
	p, err := s.q.GetPoint(ctx, id, false)
	if err != nil {
		// Qdrant returns 404 for both a missing collection and a missing point.
		// Either way the risk does not exist: surface (nil, nil), not an error.
		if errors.Is(err, agentcontext.ErrCollectionNotFound) || strings.Contains(err.Error(), "HTTP 404") {
			return nil, nil
		}
		return nil, err
	}
	if len(p.Payload) == 0 {
		return nil, nil
	}
	r := riskFromPayload(p.Payload)
	return &r, nil
}

func (s *QdrantStore) List(ctx context.Context, project, status string, limit int) ([]Risk, error) {
	if limit <= 0 {
		limit = 500
	}
	var conds []any
	if project != "" {
		conds = append(conds, agentcontext.Match("project", project))
	}
	if status != "" {
		conds = append(conds, agentcontext.Match("status", status))
	}
	var filter map[string]any
	if len(conds) > 0 {
		filter = agentcontext.FilterMust(conds...)
	}

	points, err := s.q.ScrollPoints(ctx, filter, limit, false)
	if err != nil {
		return nil, err
	}
	risks := make([]Risk, 0, len(points))
	for _, p := range points {
		if len(p.Payload) == 0 {
			continue
		}
		risks = append(risks, riskFromPayload(p.Payload))
	}
	return risks, nil
}
