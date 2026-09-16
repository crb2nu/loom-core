package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const ExternalDependencyIncident = "external_dependency_incident"

type MainRedExternalHold struct {
	Project, Branch             string
	ActivatedAt, ExpiresAt      time.Time
	EscalationSentAt, ClearedAt *time.Time
}

func (h MainRedExternalHold) Active(now time.Time) bool {
	return h.ClearedAt == nil && now.Before(h.ExpiresAt)
}

// RecordDefaultBranchPipeline persists a terminal classification before using
// it to change hold state. A successful or differently classified newest
// pipeline is recovery and clears the hold.
func (s *Store) RecordDefaultBranchPipeline(ctx context.Context, project, branch, pipelineID, classification string, observedAt time.Time, holdFor time.Duration) (*MainRedExternalHold, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("main-red hold: store not configured")
	}
	project, branch, pipelineID = strings.TrimSpace(project), strings.TrimSpace(branch), strings.TrimSpace(pipelineID)
	if project == "" || branch == "" || pipelineID == "" {
		return nil, errors.New("main-red hold: project, branch, and pipeline id are required")
	}
	if observedAt.IsZero() || holdFor <= 0 {
		return nil, errors.New("main-red hold: observation time and positive duration are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO default_branch_pipeline_observations(project,branch,pipeline_id,classification,observed_at) VALUES(?,?,?,?,?) ON CONFLICT(project,branch,pipeline_id) DO UPDATE SET classification=excluded.classification`, project, branch, pipelineID, strings.TrimSpace(classification), timeRFC3339(observedAt.UTC()))
	if err != nil {
		return nil, fmt.Errorf("persist default-branch pipeline: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT classification FROM default_branch_pipeline_observations WHERE project=? AND branch=? ORDER BY observed_at DESC, length(pipeline_id) DESC, pipeline_id DESC LIMIT 2`, project, branch)
	if err != nil {
		return nil, err
	}
	var classes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		classes = append(classes, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(classes) == 2 && classes[0] == ExternalDependencyIncident && classes[1] == ExternalDependencyIncident {
		expires := observedAt.UTC().Add(holdFor)
		_, err = tx.ExecContext(ctx, `INSERT INTO main_red_external_holds(project,branch,activated_at,expires_at) VALUES(?,?,?,?) ON CONFLICT(project,branch) DO UPDATE SET activated_at=CASE WHEN cleared_at IS NULL THEN activated_at ELSE excluded.activated_at END, expires_at=CASE WHEN cleared_at IS NULL THEN expires_at ELSE excluded.expires_at END, escalation_sent_at=CASE WHEN cleared_at IS NULL THEN escalation_sent_at ELSE NULL END, cleared_at=NULL`, project, branch, timeRFC3339(observedAt.UTC()), timeRFC3339(expires))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE main_red_external_holds SET cleared_at=? WHERE project=? AND branch=? AND cleared_at IS NULL`, timeRFC3339(observedAt.UTC()), project, branch)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.MainRedExternalHold(ctx, project, branch)
}

func (s *Store) MainRedExternalHold(ctx context.Context, project, branch string) (*MainRedExternalHold, error) {
	var h MainRedExternalHold
	var a, e string
	var sent, cleared sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT project,branch,activated_at,expires_at,escalation_sent_at,cleared_at FROM main_red_external_holds WHERE project=? AND branch=?`, project, branch).Scan(&h.Project, &h.Branch, &a, &e, &sent, &cleared)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if h.ActivatedAt, err = parseTime(a); err != nil {
		return nil, fmt.Errorf("main-red hold: activated_at: %w", err)
	}
	if h.ExpiresAt, err = parseTime(e); err != nil {
		return nil, fmt.Errorf("main-red hold: expires_at: %w", err)
	}
	if h.EscalationSentAt, err = nullableTime(sent); err != nil {
		return nil, fmt.Errorf("main-red hold: escalation_sent_at: %w", err)
	}
	if h.ClearedAt, err = nullableTime(cleared); err != nil {
		return nil, fmt.Errorf("main-red hold: cleared_at: %w", err)
	}
	return &h, nil
}

// CurrentMainRedExternalHold returns the newest uncleared hold for operator
// reporting. Queue reconciliation remains lane-specific.
func (s *Store) CurrentMainRedExternalHold(ctx context.Context) (*MainRedExternalHold, error) {
	var project, branch string
	err := s.db.QueryRowContext(ctx, `SELECT project,branch FROM main_red_external_holds WHERE cleared_at IS NULL ORDER BY activated_at DESC LIMIT 1`).Scan(&project, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.MainRedExternalHold(ctx, project, branch)
}

// ClaimMainRedExternalEscalation atomically wins the one permitted expiry
// escalation. The marker survives process restarts.
func (s *Store) ClaimMainRedExternalEscalation(ctx context.Context, project, branch string, now time.Time) (bool, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var activated, expires string
	err = tx.QueryRowContext(ctx, `UPDATE main_red_external_holds SET escalation_sent_at=? WHERE project=? AND branch=? AND cleared_at IS NULL AND escalation_sent_at IS NULL AND expires_at<=? RETURNING activated_at,expires_at`, timeRFC3339(now.UTC()), project, branch, timeRFC3339(now.UTC())).Scan(&activated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]string{"reason": "main_red_external", "project": project, "branch": branch, "activated_at": activated, "expires_at": expires})
	if err != nil {
		return false, err
	}
	// The durable escalation and marker commit together. Recovery followed by
	// a new incident can emit a new event; reconciles of this episode cannot.
	_, err = tx.ExecContext(ctx, `INSERT INTO events(occurred_at,actor,kind,subject_kind,subject_id,payload_json) VALUES(?, 'mergequeue', 'mergequeue.main_red_external.expired', 'merge_queue_lane', ?, ?)`, timeRFC3339(now.UTC()), project+"→"+branch, string(payload))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
