package pm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crb2nu/loom/pkg/codebase/embed"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mcplog"
)

// Service owns the risks domain: validation, best-effort embedding, and
// persistence via Store.
type Service struct {
	store  Store
	embed  embed.Embedder
	cfg    Config
	logger *slog.Logger

	// reader federates read-only project data (tasks, decisions) for
	// pm_project_status. Optional: nil → that rollup reports those sources as
	// partial rather than failing.
	reader ProjectReader
}

// SetProjectReader wires the federated read-only reader used by ProjectStatus.
// Kept separate from NewService so the risk-domain constructor signature (and
// its test callers) stay unchanged.
func (s *Service) SetProjectReader(r ProjectReader) { s.reader = r }

// NewService wires a Service from an explicit Store and Embedder. Used by tests
// to inject fakes.
func NewService(store Store, embedder embed.Embedder, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = mcplog.NewDefault()
	}
	return &Service{store: store, embed: embedder, cfg: cfg, logger: logger}
}

// NewServiceFromEnv builds the production Service from environment config.
func NewServiceFromEnv(logger *slog.Logger) *Service {
	cfg := LoadConfigFromEnv()
	hc := httpclient.NewDefault()
	svc := NewService(NewQdrantStore(cfg), buildEmbedder(hc, cfg), cfg, logger)
	svc.SetProjectReader(NewQdrantProjectReader(cfg))
	return svc
}

// CreateRiskInput captures the fields accepted by CreateRisk.
type CreateRiskInput struct {
	Project    string
	Title      string
	Likelihood string
	Impact     string
	Mitigation string
	Owner      string
	Status     string
}

// CreateRisk validates, embeds (best-effort), and persists a new risk.
// Returns the generated ID. A failed embedding never fails the write.
func (s *Service) CreateRisk(ctx context.Context, in CreateRiskInput) (string, error) {
	project := strings.TrimSpace(in.Project)
	title := strings.TrimSpace(in.Title)
	if project == "" {
		return "", fmt.Errorf("project is required")
	}
	if title == "" {
		return "", fmt.Errorf("title is required")
	}

	likelihood := normalizeLevel(in.Likelihood, LevelMedium)
	if !IsValidLevel(likelihood) {
		return "", fmt.Errorf("likelihood must be one of low|medium|high")
	}
	impact := normalizeLevel(in.Impact, LevelMedium)
	if !IsValidLevel(impact) {
		return "", fmt.Errorf("impact must be one of low|medium|high")
	}
	status := normalizeLevel(in.Status, StatusIdentified)
	if !IsValidStatus(status) {
		return "", fmt.Errorf("status must be one of identified|mitigating|accepted|closed")
	}

	now := time.Now().UTC()
	risk := Risk{
		ID:         uuid.New().String(),
		Project:    project,
		Title:      title,
		Likelihood: likelihood,
		Impact:     impact,
		Mitigation: strings.TrimSpace(in.Mitigation),
		Owner:      strings.TrimSpace(in.Owner),
		Status:     status,
		Links:      []string{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.store.EnsureReady(ctx); err != nil {
		return "", fmt.Errorf("ensure collection: %w", err)
	}

	vector := s.embedBestEffort(ctx, risk)
	if err := s.store.Upsert(ctx, risk, vector); err != nil {
		return "", fmt.Errorf("store risk: %w", err)
	}
	return risk.ID, nil
}

// ListRisks returns risks filtered by optional project and status.
func (s *Service) ListRisks(ctx context.Context, project, status string) ([]Risk, error) {
	project = strings.TrimSpace(project)
	status = normalizeLevel(status, "")
	if status != "" && !IsValidStatus(status) {
		return nil, fmt.Errorf("status must be one of identified|mitigating|accepted|closed")
	}
	if err := s.store.EnsureReady(ctx); err != nil {
		return nil, fmt.Errorf("ensure collection: %w", err)
	}
	risks, err := s.store.List(ctx, project, status, 0)
	if err != nil {
		return nil, fmt.Errorf("list risks: %w", err)
	}
	return risks, nil
}

// UpdateRiskInput carries optional mutable fields. Nil pointers are left
// unchanged.
type UpdateRiskInput struct {
	Title      *string
	Likelihood *string
	Impact     *string
	Mitigation *string
	Owner      *string
	Status     *string
}

// UpdateRisk applies the provided mutable fields to an existing risk. The point
// is re-embedded (best-effort) only when Title or Mitigation changed.
func (s *Service) UpdateRisk(ctx context.Context, id string, in UpdateRiskInput) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if err := s.store.EnsureReady(ctx); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get risk: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("risk %s not found", id)
	}

	reEmbed := false
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			return fmt.Errorf("title cannot be empty")
		}
		existing.Title = t
		reEmbed = true
	}
	if in.Mitigation != nil {
		existing.Mitigation = strings.TrimSpace(*in.Mitigation)
		reEmbed = true
	}
	if in.Owner != nil {
		existing.Owner = strings.TrimSpace(*in.Owner)
	}
	if in.Likelihood != nil {
		lv := normalizeLevel(*in.Likelihood, "")
		if !IsValidLevel(lv) {
			return fmt.Errorf("likelihood must be one of low|medium|high")
		}
		existing.Likelihood = lv
	}
	if in.Impact != nil {
		lv := normalizeLevel(*in.Impact, "")
		if !IsValidLevel(lv) {
			return fmt.Errorf("impact must be one of low|medium|high")
		}
		existing.Impact = lv
	}
	if in.Status != nil {
		st := normalizeLevel(*in.Status, "")
		if !IsValidStatus(st) {
			return fmt.Errorf("status must be one of identified|mitigating|accepted|closed")
		}
		existing.Status = st
	}

	existing.UpdatedAt = time.Now().UTC()

	var vector []float64
	if reEmbed {
		vector = s.embedBestEffort(ctx, *existing)
	} else {
		vector = fallbackVector()
	}
	if err := s.store.Upsert(ctx, *existing, vector); err != nil {
		return fmt.Errorf("store risk: %w", err)
	}
	return nil
}

// LinkRisk appends a reference (gitlab issue path or task id) to a risk's
// Links, de-duplicating.
func (s *Service) LinkRisk(ctx context.Context, id, ref string) error {
	id = strings.TrimSpace(id)
	ref = strings.TrimSpace(ref)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if ref == "" {
		return fmt.Errorf("ref is required")
	}
	if err := s.store.EnsureReady(ctx); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get risk: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("risk %s not found", id)
	}
	for _, l := range existing.Links {
		if l == ref {
			return nil // already linked; idempotent
		}
	}
	existing.Links = append(existing.Links, ref)
	existing.UpdatedAt = time.Now().UTC()

	// Link does not change embed text; reuse a fallback vector to avoid an
	// unnecessary embed call.
	if err := s.store.Upsert(ctx, *existing, fallbackVector()); err != nil {
		return fmt.Errorf("store risk: %w", err)
	}
	return nil
}

// embedBestEffort attempts to embed the risk text. On ANY failure it logs a
// warning and returns a deterministic fallback vector so the write proceeds.
func (s *Service) embedBestEffort(ctx context.Context, r Risk) []float64 {
	text := r.embedText()
	if text == "" {
		return fallbackVector()
	}
	ec := ctx
	if s.cfg.EmbedTimeout > 0 {
		var cancel context.CancelFunc
		ec, cancel = context.WithTimeout(ctx, s.cfg.EmbedTimeout)
		defer cancel()
	}
	vec, err := s.embed.EmbedQuery(ec, text)
	if err != nil || len(vec) == 0 {
		s.logger.Warn("risk embed failed; using fallback vector",
			"risk_id", r.ID, "error", err)
		return fallbackVector()
	}
	if len(vec) != VectorSize {
		s.logger.Warn("risk embed returned unexpected dimension; using fallback vector",
			"risk_id", r.ID, "got", len(vec), "want", VectorSize)
		return fallbackVector()
	}
	return vec
}

func normalizeLevel(s, def string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return def
	}
	return t
}
