package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/boltcard"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const boltReadLimit = 200

type boltGitLabStats struct {
	Files, Added, Removed int
	URL                   string
	MergedAt              time.Time
}

type boltsResponse struct {
	GeneratedAt   time.Time       `json:"generated_at"`
	WindowSeconds int64           `json:"window_seconds"`
	Bolts         []boltcard.Card `json:"bolts"`
}

func (o *operator) handleBolts(w http.ResponseWriter, r *http.Request) {
	window := 24 * time.Hour
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid window", http.StatusBadRequest)
			return
		}
		window = parsed
	}
	now := time.Now().UTC()
	runs, err := o.store.Pipeline.ListRecentTerminalBolts(r.Context(), now.Add(-window), boltReadLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runless, err := o.store.Backlog.ListRunlessTerminal(r.Context(), now.Add(-window), boltReadLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	weekRuns, err := o.store.Pipeline.ListRecentTerminalBolts(r.Context(), now.Add(-7*24*time.Hour), boltReadLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	occurrences := map[string]int{}
	for _, run := range weekRuns {
		if run.State == store.PipelineEscalated && run.FailureSignature != "" {
			occurrences[run.FailureSignature]++
		}
	}
	cards := make([]boltcard.Card, 0, len(runs)+len(runless))
	seen := map[string]bool{}
	for _, run := range runs {
		item, err := o.store.Backlog.Get(r.Context(), run.BacklogID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			http.Error(w, err.Error(), 500)
			return
		}
		card, err := o.runBoltCard(r.Context(), run, item, occurrences)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		cards = append(cards, card)
		seen[item.ID] = true
	}
	for _, item := range runless {
		if seen[item.ID] {
			continue
		}
		cards = append(cards, o.runlessBoltCard(r.Context(), item))
	}
	writeJSON(w, http.StatusOK, boltsResponse{GeneratedAt: now, WindowSeconds: int64(window / time.Second), Bolts: cards})
}

func (o *operator) runBoltCard(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, occurrences map[string]int) (boltcard.Card, error) {
	stages, err := o.store.Pipeline.ListStages(ctx, run.ID)
	if err != nil {
		return boltcard.Card{}, err
	}
	gatesIn, err := o.store.Pipeline.ListGates(ctx, run.ID)
	if err != nil {
		return boltcard.Card{}, err
	}
	rid := run.ID
	evidence := o.runEvidence(ctx, run)
	card := boltcard.Card{RunID: &rid, BacklogID: item.ID, Title: item.Title, SpecHeadline: boltcard.SpecHeadline(item.SpecDoc), Outcome: boltcard.Outcome(run.State), MR: boltcard.MR{IID: run.MRIID}, Diff: boltcard.DiffFromStages(stages), Eval: boltcard.EvalFromEvidence(evidence), GatesFailed: boltcard.FinalFailedGates(gatesIn), Attempts: run.Attempts, CostUSD: run.CostUSD, Template: run.Template, Regression: evidence["regression"], Stamp: boltcard.StampFromEvidence(evidence), Grade: boltcard.GradeFromItem(item)}
	if card.Stamp == nil {
		card.Stamp = boltcard.StampFromPlanID(item.PlanID)
	}
	card.MR.URL = boltcard.StringArtifact(stages, "mr_url")
	if card.MR.URL == "" {
		card.MR.URL = boltcard.MRURL(o.mrProjectURL(item), run.MRIID)
	}
	if run.State == store.PipelineDone {
		card.MergedAt = run.EndedAt
	}
	if run.State == store.PipelineEscalated {
		card.Escalation = &boltcard.Escalation{Class: firstNonEmpty(run.FailureClass, run.EscalationClass), Signature: run.FailureSignature, WeekOccurrences: occurrences[run.FailureSignature]}
	}
	return card, nil
}

func (o *operator) runlessBoltCard(ctx context.Context, item *store.BacklogItem) boltcard.Card {
	card := boltcard.Card{BacklogID: item.ID, Title: item.Title, SpecHeadline: boltcard.SpecHeadline(item.SpecDoc), Outcome: backlogBoltOutcome(item.State), MR: boltcard.MR{IID: item.GitLabIssueIID}, Eval: boltcard.Eval{Verdicts: []any{}}, GatesFailed: []string{}, Regression: nil, Stamp: boltcard.StampFromPlanID(item.PlanID), Grade: boltcard.GradeFromItem(item)}
	if o.boltMRStats != nil && item.GitLabIssueIID != nil && *item.GitLabIssueIID > 0 {
		if stats, err := o.boltMRStats(ctx, firstNonEmpty(item.TargetProject, o.verdictDefaultProject), *item.GitLabIssueIID); err == nil {
			card.Diff = boltcard.Diff{Files: stats.Files, Added: stats.Added, Removed: stats.Removed, Source: "gitlab"}
			card.MR.URL = stats.URL
			if !stats.MergedAt.IsZero() {
				x := stats.MergedAt
				card.MergedAt = &x
			}
		}
	}
	if card.MR.URL == "" {
		card.MR.URL = boltcard.MRURL(o.mrProjectURL(item), card.MR.IID)
	}
	return card
}

func (o *operator) mrProjectURL(item *store.BacklogItem) string {
	project := firstNonEmpty(item.TargetProject, o.verdictDefaultProject)
	if o.gitlabBaseURL == "" || project == "" {
		return ""
	}
	return strings.TrimRight(o.gitlabBaseURL, "/") + "/" + strings.Trim(project, "/")
}
func backlogBoltOutcome(s store.BacklogState) string {
	if s == store.BacklogMerged {
		return "merged"
	}
	if s == store.BacklogEscalated {
		return "escalated"
	}
	return string(s)
}
