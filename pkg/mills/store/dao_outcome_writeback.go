package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrOutcomeNotTerminal = errors.New("outcome writeback requires a merged or escalated item")

// OutcomeWriteback is the durable feature row captured when a backlog item
// reaches a terminal state. Grade is nil when the item has not been graded.
type OutcomeWriteback struct {
	BacklogID     string    `json:"backlog_id"`
	PipelineRunID string    `json:"pipeline_run_id"`
	PlanID        string    `json:"plan_id,omitempty"`
	Outcome       string    `json:"outcome"`
	Attempts      int       `json:"attempts"`
	CostUSD       float64   `json:"cost_usd"`
	DispatchScore float64   `json:"dispatch_score"`
	Grade         *string   `json:"grade,omitempty"`
	WrittenAt     time.Time `json:"written_at"`
}

type OutcomeFeatures struct {
	PlanID          string
	Samples         int
	MergeRate       float64
	AverageAttempts float64
	AverageCostUSD  float64
}

type CalibrationBucket struct {
	Lower                  float64  `json:"lower"`
	Upper                  float64  `json:"upper"`
	Samples                int      `json:"samples"`
	MeanScore              float64  `json:"mean_score"`
	RealizedMergeRate      float64  `json:"realized_merge_rate"`
	RealizedEscalationRate float64  `json:"realized_escalation_rate"`
	RealizedQuality        *float64 `json:"realized_quality"`
	TasteCalibrationError  *float64 `json:"taste_calibration_error"`
}

type OutcomeCalibration struct {
	WindowStart      time.Time           `json:"window_start"`
	WindowEnd        time.Time           `json:"window_end"`
	Samples          int                 `json:"samples"`
	Merged           int                 `json:"merged"`
	Escalated        int                 `json:"escalated"`
	Graded           int                 `json:"graded"`
	OutcomeCoverage  float64             `json:"outcome_coverage"`
	GradeCoverage    float64             `json:"grade_coverage"`
	CalibrationError float64             `json:"calibration_error"`
	Buckets          []CalibrationBucket `json:"buckets"`
}

type OutcomeWritebackDAO struct{ db *sql.DB }

// WriteTerminal joins the terminal backlog item to its latest terminal run.
// The primary-key upsert makes retries safe and also picks up a later grade.
func (d *OutcomeWritebackDAO) WriteTerminal(ctx context.Context, backlogID string, dispatchScore float64) (*OutcomeWriteback, error) {
	if backlogID == "" {
		return nil, errors.New("outcome writeback: backlog id required")
	}
	if math.IsNaN(dispatchScore) || math.IsInf(dispatchScore, 0) {
		return nil, errors.New("outcome writeback: dispatch score must be finite")
	}
	var state, runID, planID, grade string
	var attempts int
	var cost float64
	err := d.db.QueryRowContext(ctx, `SELECT b.state, p.id, COALESCE(b.plan_id,''), COALESCE(b.grade,''), p.attempts, p.cost_usd
		FROM backlog_items b JOIN pipeline_runs p ON p.backlog_id=b.id
		WHERE b.id=? AND p.state IN ('done','escalated')
		ORDER BY COALESCE(p.ended_at,p.started_at) DESC LIMIT 1`, backlogID).Scan(&state, &runID, &planID, &grade, &attempts, &cost)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutcomeNotTerminal
	}
	if err != nil {
		return nil, fmt.Errorf("outcome writeback join: %w", err)
	}
	if state != string(BacklogMerged) && state != string(BacklogEscalated) {
		return nil, ErrOutcomeNotTerminal
	}
	now := time.Now().UTC()
	var gradeArg any
	if grade != "" {
		gradeArg = grade
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO outcome_writebacks
		(backlog_id,pipeline_run_id,plan_id,outcome,attempts,cost_usd,dispatch_score,grade,written_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(backlog_id) DO UPDATE SET
		pipeline_run_id=excluded.pipeline_run_id, plan_id=excluded.plan_id, outcome=excluded.outcome,
		attempts=excluded.attempts, cost_usd=excluded.cost_usd, dispatch_score=excluded.dispatch_score,
		grade=COALESCE(excluded.grade,outcome_writebacks.grade), written_at=excluded.written_at`,
		backlogID, runID, nullStr(planID), state, attempts, cost, dispatchScore, gradeArg, timeRFC3339(now))
	if err != nil {
		return nil, fmt.Errorf("outcome writeback: %w", err)
	}
	return d.Get(ctx, backlogID)
}

func (d *OutcomeWritebackDAO) Get(ctx context.Context, backlogID string) (*OutcomeWriteback, error) {
	var o OutcomeWriteback
	var plan, grade, written sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT backlog_id,pipeline_run_id,plan_id,outcome,attempts,cost_usd,dispatch_score,grade,written_at FROM outcome_writebacks WHERE backlog_id=?`, backlogID).
		Scan(&o.BacklogID, &o.PipelineRunID, &plan, &o.Outcome, &o.Attempts, &o.CostUSD, &o.DispatchScore, &grade, &written)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.PlanID = plan.String
	if grade.Valid {
		o.Grade = &grade.String
	}
	o.WrittenAt, _ = time.Parse(time.RFC3339Nano, written.String)
	return &o, nil
}

// Features returns outcome-only historical features by plan for ranked dispatch.
func (d *OutcomeWritebackDAO) Features(ctx context.Context, since time.Time) (map[string]OutcomeFeatures, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT plan_id,COUNT(*),AVG(CASE WHEN outcome='merged' THEN 1.0 ELSE 0 END),AVG(attempts),AVG(cost_usd)
		FROM outcome_writebacks WHERE written_at>=? AND plan_id IS NOT NULL AND trim(plan_id)<>'' GROUP BY plan_id`, timeRFC3339(since.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]OutcomeFeatures{}
	for rows.Next() {
		var f OutcomeFeatures
		if err := rows.Scan(&f.PlanID, &f.Samples, &f.MergeRate, &f.AverageAttempts, &f.AverageCostUSD); err != nil {
			return nil, err
		}
		out[f.PlanID] = f
	}
	return out, rows.Err()
}

func (d *OutcomeWritebackDAO) Calibration(ctx context.Context, start, end time.Time) (OutcomeCalibration, error) {
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return OutcomeCalibration{}, errors.New("outcome calibration: start must be before end")
	}
	r := OutcomeCalibration{WindowStart: start.UTC(), WindowEnd: end.UTC(), Buckets: []CalibrationBucket{}}
	rows, err := d.db.QueryContext(ctx, `SELECT dispatch_score,outcome,COALESCE(grade,'') FROM outcome_writebacks WHERE written_at>=? AND written_at<? ORDER BY written_at`, timeRFC3339(start.UTC()), timeRFC3339(end.UTC()))
	if err != nil {
		return r, err
	}
	defer rows.Close()
	type acc struct {
		n, merged, qualityN int
		scores, qualitySum  float64
	}
	bins := make([]acc, 10)
	for rows.Next() {
		var score float64
		var outcome, grade string
		if err := rows.Scan(&score, &outcome, &grade); err != nil {
			return r, err
		}
		r.Samples++
		if outcome == "merged" {
			r.Merged++
		} else {
			r.Escalated++
		}
		if grade != "" {
			r.Graded++
		}
		probability := clampProbability(score)
		i := int(probability * 10)
		if i == 10 {
			i = 9
		}
		bins[i].n++
		bins[i].scores += probability
		if outcome == "merged" {
			bins[i].merged++
			switch grade {
			case "keep":
				bins[i].qualityN++
				bins[i].qualitySum++
			case "meh":
				bins[i].qualityN++
				bins[i].qualitySum += 0.5
			case "regret":
				bins[i].qualityN++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	if r.Samples > 0 {
		r.OutcomeCoverage = 1
		r.GradeCoverage = float64(r.Graded) / float64(r.Samples)
	}
	for i, b := range bins {
		if b.n == 0 {
			continue
		}
		mean := b.scores / float64(b.n)
		merge := float64(b.merged) / float64(b.n)
		r.CalibrationError += math.Abs(mean-merge) * float64(b.n) / float64(r.Samples)
		bucket := CalibrationBucket{Lower: float64(i) / 10, Upper: float64(i+1) / 10, Samples: b.n, MeanScore: mean, RealizedMergeRate: merge, RealizedEscalationRate: 1 - merge}
		if b.qualityN > 0 {
			quality := b.qualitySum / float64(b.qualityN)
			tasteError := math.Abs(mean - quality)
			bucket.RealizedQuality = &quality
			bucket.TasteCalibrationError = &tasteError
		}
		r.Buckets = append(r.Buckets, bucket)
	}
	return r, nil
}

func clampProbability(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
