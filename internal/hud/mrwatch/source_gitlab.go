package mrwatch

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// Default enrichment bounds. GitLab's merge-request *list* endpoint returns no
// pipeline info at all (verified against the live instance 2026-07-18: list
// items carry neither `head_pipeline` nor `pipeline`); only the single-MR GET
// includes `head_pipeline`. So after listing, the source enriches a bounded
// number of MRs — newest-updated first — with one GET each. Bounds keep the
// per-poll API budget at 1 list + ≤maxEnrich GETs per project.
const (
	defaultMaxEnrich     = 20
	defaultEnrichTimeout = 10 * time.Second
)

// GitLabSource adapts the mills GitLab client to the mrwatch Source interface.
// It scopes the shared-token client to each watched project via ForProject,
// lists open MRs, then enriches up to maxEnrich of them (newest-updated first)
// with the single-MR GET that carries head_pipeline. An enrichment failure
// degrades that MR to its list-item view (classified from what is known);
// it never fails the whole poll.
type GitLabSource struct {
	client *clients.GitLabClient
	// perPage bounds the MR page fetched per project.
	perPage int
	// maxEnrich caps single-MR enrichment GETs per project per poll.
	maxEnrich int
	// enrichTimeout bounds each enrichment call.
	enrichTimeout time.Duration
	logger        *slog.Logger
}

// NewGitLabSource wraps a configured GitLab client with default bounds
// (perPage 50, maxEnrich 20, 10s per enrichment call).
func NewGitLabSource(client *clients.GitLabClient) *GitLabSource {
	return &GitLabSource{
		client:        client,
		perPage:       50,
		maxEnrich:     defaultMaxEnrich,
		enrichTimeout: defaultEnrichTimeout,
		logger:        slog.Default(),
	}
}

// SetMaxEnrich overrides the per-project enrichment cap. Values < 0 are
// treated as 0 (list-only, no enrichment).
func (s *GitLabSource) SetMaxEnrich(n int) {
	if n < 0 {
		n = 0
	}
	s.maxEnrich = n
}

// SetLogger overrides the logger (nil restores slog.Default()).
func (s *GitLabSource) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	s.logger = l
}

// ListOpenMRs implements Source. It lists open MRs for the project, enriches
// the newest-updated maxEnrich of them with the single-MR GET (the only call
// that returns head_pipeline), and maps each into an MRInfo the classifier
// understands. List errors surface to the poller, which degrades gracefully;
// per-MR enrichment errors degrade only that MR.
func (s *GitLabSource) ListOpenMRs(ctx context.Context, project string) ([]MRInfo, error) {
	scoped := s.client.ForProject(project)
	items, err := scoped.ListOpenMergeRequests(ctx, s.perPage)
	if err != nil {
		return nil, err
	}

	// Enrich newest-updated first so the freshest MRs get real CI state when
	// the cap bites. The API already orders by updated_at desc; sort
	// defensively so the bound is deterministic regardless of server order.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})

	out := make([]MRInfo, 0, len(items))
	enriched := 0
	for _, it := range items {
		if enriched < s.maxEnrich {
			enriched++
			if full, err := s.enrich(ctx, scoped, it.IID); err != nil {
				s.logger.Warn("mrwatch: MR enrichment failed; classifying from list view",
					"project", project, "mr_iid", it.IID, "error", err.Error())
			} else {
				it = full
			}
		}
		out = append(out, mapListItem(project, it))
	}
	return out, nil
}

// ListMergedMRs implements MergedLister: it lists the project's MRs merged
// since `since` so the registry can retain an explicit merged marker. No
// enrichment is performed — a merged MR's CI state is irrelevant and the extra
// GETs would double the per-poll API budget for no signal.
func (s *GitLabSource) ListMergedMRs(ctx context.Context, project string, since time.Time) ([]MRInfo, error) {
	items, err := s.client.ForProject(project).ListMergedMergeRequests(ctx, s.perPage, since)
	if err != nil {
		return nil, err
	}
	out := make([]MRInfo, 0, len(items))
	for _, it := range items {
		out = append(out, mapListItem(project, it))
	}
	return out, nil
}

// enrich performs the bounded single-MR GET with its own timeout so one slow
// call can't eat the whole poll budget.
func (s *GitLabSource) enrich(ctx context.Context, scoped *clients.GitLabClient, iid int64) (clients.MergeRequestListItem, error) {
	timeout := s.enrichTimeout
	if timeout <= 0 {
		timeout = defaultEnrichTimeout
	}
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return scoped.GetMergeRequest(ectx, iid)
}

// mapListItem converts a client item (list view or enriched single-MR view)
// into the source-agnostic MRInfo.
func mapListItem(project string, it clients.MergeRequestListItem) MRInfo {
	info := MRInfo{
		Repo:                      project,
		IID:                       it.IID,
		Title:                     it.Title,
		SourceBranch:              it.SourceBranch,
		TargetBranch:              it.TargetBranch,
		WebURL:                    it.WebURL,
		State:                     it.State,
		SHA:                       it.SHA,
		Draft:                     it.IsDraft(),
		HasConflicts:              it.HasConflicts,
		DetailedMergeStatus:       it.DetailedMergeStatus,
		MergeWhenPipelineSucceeds: it.MergeWhenPipelineSucceeds,
		CreatedAt:                 it.CreatedAt,
		UpdatedAt:                 it.UpdatedAt,
		MergedAt:                  it.MergedAt,
	}
	if hp := it.EffectiveHeadPipeline(); hp != nil {
		info.Pipeline = &PipelineInfo{
			ID:     hp.ID,
			Status: hp.Status,
			Source: hp.Source,
			WebURL: hp.WebURL,
		}
	}
	return info
}
