package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/overseer"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	reportRollupInterval       = 5 * time.Minute
	reportRollupRequestTimeout = 5 * time.Second
)

func (o *operator) writeReportRollup(w http.ResponseWriter, r *http.Request, name string, window time.Duration, key string) {
	if o.store == nil || o.store.Reports == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "report snapshots unavailable"})
		return
	}
	snap, err := o.store.Reports.Latest(r.Context(), name, int(window.Seconds()), key)
	if errors.Is(err, store.ErrNotFound) {
		snap, err = o.buildReportRollup(r.Context(), name, window, key, reportRollupRequestTimeout)
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "report snapshot not yet available"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Loom-Report-Snapshot-At", snap.SnapshotAt.Format(time.RFC3339Nano))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snap.Payload)
}

func (o *operator) buildReportRollup(ctx context.Context, name string, window time.Duration, key string, timeout time.Duration) (*store.ReportRollupSnapshot, error) {
	spec, ok := o.reportRollupSpecs()[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	now := time.Now().UTC()
	payload, err := spec(key)(buildCtx, now.Add(-window), now)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	snap := &store.ReportRollupSnapshot{ReportName: name, WindowSeconds: int(window.Seconds()), ReportKey: key, SnapshotAt: now, Payload: raw}
	if err := o.store.Reports.Put(buildCtx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

type reportBuilder func(context.Context, time.Time, time.Time) (any, error)
type reportBuilderFactory func(string) reportBuilder

func (o *operator) reportRollupSpecs() map[string]reportBuilderFactory {
	return map[string]reportBuilderFactory{
		"judge_calibration": func(string) reportBuilder {
			return func(ctx context.Context, since, now time.Time) (any, error) {
				return guard.BuildJudgeCalibrationReport(ctx, o.store.Events, o.store.Pipeline, since, now)
			}
		},
		"promotion": func(key string) reportBuilder {
			return func(ctx context.Context, since, now time.Time) (any, error) {
				return guard.BuildPromotionReport(ctx, o.store.Events, key, since, now)
			}
		},
		"config_outcomes": func(string) reportBuilder {
			return func(ctx context.Context, since, now time.Time) (any, error) {
				return guard.BuildConfigOutcomeReport(ctx, o.store.Events, o.store.Pipeline, since, now)
			}
		},
		"overseers":            func(string) reportBuilder { return o.buildOverseersRollup },
		"signature_candidates": func(string) reportBuilder { return o.buildSignatureCandidatesRollup },
		"regressions":          func(string) reportBuilder { return o.buildRegressionsRollup },
	}
}

func newReportRollupWriter(o *operator) *eval.ReportRollupWriter {
	specs := o.reportRollupSpecs()
	return &eval.ReportRollupWriter{Reports: o.store.Reports, Interval: reportRollupInterval, Timeout: 45 * time.Second, Logger: o.logger, Specs: []eval.ReportRollupSpec{
		{Name: "judge_calibration", Window: judgeCalibrationDefaultWindow, Build: specs["judge_calibration"]("")},
		{Name: "promotion", Window: promotionReportDefaultWindow, Key: promotionReportDefaultActor, Build: specs["promotion"](promotionReportDefaultActor)},
		{Name: "config_outcomes", Window: configOutcomesDefaultWindow, Build: specs["config_outcomes"]("")},
		{Name: "overseers", Window: overseerRecentActionsWindow, Build: specs["overseers"]("")},
		{Name: "signature_candidates", Window: signatureCandidatesDefaultWindow, Build: specs["signature_candidates"]("")},
		{Name: "regressions", Window: regressionsDefaultWindow, Build: specs["regressions"]("")},
	}}
}

func (o *operator) buildOverseersRollup(ctx context.Context, since, now time.Time) (any, error) {
	resp := overseersStatusResponse{Agents: make([]overseerAgentView, 0, len(o.overseers)), RecentActions: make(map[string][]*store.Event, len(o.overseers))}
	// The S2 promotion verdict, projected from the persisted dry-run
	// decisions (docs/mill-staff-s2-soak-runbook.md). Before 2026-09-02 no
	// decision was ever recorded, so the verdict could only fail closed and
	// the three dry-run overseers had no path to promotion.
	if now.IsZero() {
		now = time.Now().UTC()
	}
	soak := overseer.EvaluatePersistedS2Soak(ctx, o.store, now)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("overseer soak telemetry: %w", err)
	}
	resp.Soak = &soak
	if pol := o.policy.Current(); pol != nil {
		resp.Enabled = pol.Overseers.Enabled
	}
	names := make([]string, 0, len(o.overseers))
	for name := range o.overseers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := o.overseers[name]
		view := overseerAgentView{AgentStatus: entry.Harness.Status()}
		if entry.Enabled != nil {
			view.Enabled = entry.Enabled()
		}
		if entry.DryRun != nil {
			view.DryRun = entry.DryRun()
		}
		if entry.Suppression != nil {
			view.Suppression = entry.Suppression()
		}
		resp.Agents = append(resp.Agents, view)
		events, err := o.store.Events.ListByActorSince(ctx, "overseer."+name, since, overseerRecentActionsLimit)
		if err != nil {
			return nil, err
		}
		resp.RecentActions[name] = events
	}
	return resp, nil
}

func (o *operator) buildSignatureCandidatesRollup(ctx context.Context, since, now time.Time) (any, error) {
	events, err := o.store.Events.ListByActorSince(ctx, mills.SignatureMinerActor, since, signatureCandidatesScanLimit)
	if err != nil {
		return nil, err
	}
	resp := signatureCandidatesResponse{Window: now.Sub(since).String(), Since: since, Candidates: make([]signatureCandidateView, 0, len(events))}
	for _, ev := range events {
		if ev != nil && ev.Kind == mills.SignatureCandidateEventKind {
			resp.Candidates = append(resp.Candidates, signatureCandidateView{Fingerprint: ev.SubjectID, Phrase: eventPayloadString(ev, "phrase"), MemberCount: eventPayloadInt64(ev, "member_count"), WindowMatchCount: eventPayloadInt64(ev, "window_match_count"), SampleEvidence: eventPayloadStrings(ev, "sample_evidence"), FirstSeen: eventPayloadString(ev, "first_seen"), LastSeen: eventPayloadString(ev, "last_seen"), ProposedAt: ev.OccurredAt})
		}
	}
	resp.Count = len(resp.Candidates)
	return resp, nil
}

func (o *operator) buildRegressionsRollup(ctx context.Context, since, now time.Time) (any, error) {
	events, err := o.store.Events.ListByActorSince(ctx, mills.RegressionAttributionActor, since, regressionsScanLimit)
	if err != nil {
		return nil, err
	}
	resp := regressionsResponse{Window: now.Sub(since).String(), Since: since, Regressions: make([]regressionAttributionView, 0, len(events))}
	for _, ev := range events {
		if ev != nil && ev.Kind == mills.RegressionAttributedEventKind {
			resp.Regressions = append(resp.Regressions, regressionAttributionView{RegressedMRIID: eventPayloadInt64(ev, "regressed_mr_iid"), MergedSHA: eventPayloadString(ev, "merged_sha"), RevertSHA: eventPayloadString(ev, "revert_sha"), RevertTitle: eventPayloadString(ev, "revert_title"), AttributedAt: ev.OccurredAt})
		}
	}
	resp.Count = len(resp.Regressions)
	return resp, nil
}
