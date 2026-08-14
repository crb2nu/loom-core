package guard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// RunOutcomeLister is the narrowest runs read a ground-truth join needs: the
// terminal state of every run started in a window. *store.PipelineDAO
// satisfies it.
type RunOutcomeLister interface {
	ListTerminalOutcomesSince(ctx context.Context, since time.Time, limit int) ([]*store.RunTerminalOutcome, error)
}

const (
	// judgeCalibrationEventLimit and judgeCalibrationRunLimit bound the two
	// window scans. Saturating either is an error rather than a truncation:
	// a calibration read that silently drops verdicts, or that loses the runs
	// they belong to, reports a pass rate against a partial reality.
	judgeCalibrationEventLimit = 10000
	judgeCalibrationRunLimit   = 10000

	// Score bucket edges. Three buckets is what a judge's score distribution
	// can actually support: confident fail, contested middle, confident pass.
	judgeBucketLowMax = 0.5
	judgeBucketMidMax = 0.8
)

// Terminal-outcome labels a verdict is attributed to. "other" covers runs
// still in flight, runs paused for a human, and runs the window's run scan
// never saw — all cases where the factory has not yet told us whether the
// judge was right. They are never folded into merged or escalated.
const (
	JudgeOutcomeMerged    = "merged"
	JudgeOutcomeEscalated = "escalated"
	JudgeOutcomeOther     = "other"
)

// Bucket labels, rendered once so the report and its readers cannot disagree
// about where 0.8 lands.
const (
	JudgeBucketLow  = "<0.5"
	JudgeBucketMid  = "0.5-0.8"
	JudgeBucketHigh = ">=0.8"
)

// JudgeCalibrationReport answers "did the factory's own grades track what the
// work actually did?" over a window: every judge verdict joined to the
// terminal outcome of the run it graded.
type JudgeCalibrationReport struct {
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// TotalVerdicts counts every judge.verdict event in the window, including
	// tiebreaker second opinions and verdicts whose run has not finished.
	TotalVerdicts int `json:"total_verdicts"`
	// JoinedVerdicts counts the verdicts whose run reached a terminal state
	// in the window — the only ones that carry calibration signal.
	JoinedVerdicts int `json:"joined_verdicts"`
	// CorrectedVerdicts counts the joined verdicts whose run's escalation was
	// later superseded (verdict merged_after_escalation — the MR landed).
	// Those verdicts are counted on the MERGED side above; these two fields
	// keep the correction visible instead of silently folded.
	CorrectedVerdicts int `json:"corrected_verdicts"`
	// CorrectedRuns counts the distinct corrected runs behind those verdicts.
	CorrectedRuns int          `json:"corrected_runs"`
	PerGate       []JudgeGate  `json:"per_gate"`
	Buckets       []string     `json:"buckets"`
	Outcomes      []string     `json:"outcomes"`
	Models        []JudgeModel `json:"models"`
	// ZeroEvidence marks a window that recorded no verdicts at all. A judge
	// that never ran is trivially well-calibrated, so the absence of evidence
	// is a stated finding rather than an empty table read as a clean bill.
	ZeroEvidence bool `json:"zero_evidence"`
}

// JudgeGate is one gate's calibration row.
type JudgeGate struct {
	Gate     string `json:"gate"`
	Verdicts int    `json:"verdicts"`
	Passed   int    `json:"passed"`
	// PassRate is Passed/Verdicts over ALL verdicts, joined or not: how
	// often this gate says yes.
	PassRate float64 `json:"pass_rate"`
	// MergedVerdicts / EscalatedVerdicts / OtherVerdicts partition Verdicts
	// by what the graded run finally did — per the run's current VERDICT, so
	// a corrected escalation counts under merged.
	MergedVerdicts    int `json:"merged_verdicts"`
	EscalatedVerdicts int `json:"escalated_verdicts"`
	OtherVerdicts     int `json:"other_verdicts"`
	// CorrectedVerdicts is the slice of MergedVerdicts that got there via a
	// superseded escalation (merged_after_escalation).
	CorrectedVerdicts int `json:"corrected_verdicts"`
	// MeanScoreMerged and MeanScoreEscalated are the calibration signal: a
	// judge that discriminates scores merged work above escalated work. Both
	// are 0 when their count is 0 — read the counts first.
	MeanScoreMerged    float64            `json:"mean_score_merged"`
	MeanScoreEscalated float64            `json:"mean_score_escalated"`
	Histogram          []JudgeScoreBucket `json:"histogram"`
}

// JudgeScoreBucket is one score bucket split by terminal outcome. Scores
// clustering in the same bucket regardless of outcome mean the gate is
// grading something other than whether the work shipped.
type JudgeScoreBucket struct {
	Bucket    string `json:"bucket"`
	Merged    int    `json:"merged"`
	Escalated int    `json:"escalated"`
	Other     int    `json:"other"`
}

// JudgeModel counts verdicts per judge backend so a model swap mid-window is
// visible rather than averaged away.
type JudgeModel struct {
	Model    string `json:"model"`
	Role     string `json:"role"`
	Verdicts int    `json:"verdicts"`
}

// judgeGateAgg accumulates one gate's cells before the report is sorted.
type judgeGateAgg struct {
	verdicts            int
	passed              int
	corrected           int
	byOutcome           map[string]int
	scoreSumMerged      float64
	scoreSumEscalated   float64
	countMerged         int
	countEscalated      int
	bucketOutcomeCounts map[string]map[string]int
}

func newJudgeGateAgg() *judgeGateAgg {
	return &judgeGateAgg{
		byOutcome:           make(map[string]int, 3),
		bucketOutcomeCounts: make(map[string]map[string]int, 3),
	}
}

// BuildJudgeCalibrationReport joins the judge verdicts recorded over
// [since, now] to the terminal outcomes of the runs they graded. Pure over the
// two read surfaces: no store writes, no clock reads.
func BuildJudgeCalibrationReport(ctx context.Context, events EventLister, runs RunOutcomeLister, since, now time.Time) (JudgeCalibrationReport, error) {
	if events == nil {
		return JudgeCalibrationReport{}, errors.New("judge calibration: events lister required")
	}
	if runs == nil {
		return JudgeCalibrationReport{}, errors.New("judge calibration: runs lister required")
	}
	if !since.Before(now) {
		return JudgeCalibrationReport{}, fmt.Errorf("judge calibration: window start %s is not before end %s",
			since.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	outcomeByRun, err := terminalOutcomesByRun(ctx, runs, since)
	if err != nil {
		return JudgeCalibrationReport{}, err
	}

	// Prefer the kind-filtered scan so the truncation cap below counts judge
	// verdicts, not the whole firehose (see ActorPrefixLister). The in-memory
	// kind check stays: it also guards the fallback path. Verdict-correction
	// kinds ride the same scan (Trustworthy Verdicts S2): a corrected
	// escalation joins the merged side of the calibration signal below.
	var raw []*store.Event
	if kl, ok := events.(KindLister); ok {
		raw, err = kl.ListSinceByKinds(ctx, []string{
			store.JudgeVerdictEventKind,
			mills.RunVerdictKindGhostSparkMerged,
			mills.GhostSparkClosedEventKind,
		}, since, judgeCalibrationEventLimit)
	} else {
		raw, err = events.ListSince(ctx, since, judgeCalibrationEventLimit)
	}
	if err != nil {
		return JudgeCalibrationReport{}, fmt.Errorf("judge calibration: %w", err)
	}
	if len(raw) >= judgeCalibrationEventLimit {
		return JudgeCalibrationReport{}, fmt.Errorf("judge calibration: window holds at least %d events; narrow the window rather than calibrate on a truncated sample", judgeCalibrationEventLimit)
	}
	corrected := correctedRunIDs(raw, since, now)

	byGate := make(map[string]*judgeGateAgg)
	byModel := make(map[string]*JudgeModel)
	correctedRuns := make(map[string]struct{})
	rep := JudgeCalibrationReport{
		WindowStart: since.UTC(),
		WindowEnd:   now.UTC(),
		Buckets:     []string{JudgeBucketLow, JudgeBucketMid, JudgeBucketHigh},
		Outcomes:    []string{JudgeOutcomeMerged, JudgeOutcomeEscalated, JudgeOutcomeOther},
	}
	for _, e := range raw {
		if e == nil || e.Kind != store.JudgeVerdictEventKind {
			continue
		}
		// ListSince bounds the window's start; the end is bounded here so a
		// clock-skewed future verdict cannot land in a closed window.
		if e.OccurredAt.Before(since) || e.OccurredAt.After(now) {
			continue
		}
		v, ok := parseJudgeVerdict(e)
		if !ok {
			continue
		}
		outcome := JudgeOutcomeOther
		if state, joined := outcomeByRun[v.runID]; joined {
			outcome = state
		}
		// A corrected escalation (verdict superseded — its MR merged) joins
		// the MERGED side: that is the ground truth the judge graded. The
		// correction is counted explicitly below, never silent.
		wasCorrected := false
		if outcome == JudgeOutcomeEscalated {
			if _, ok := corrected[v.runID]; ok {
				outcome = JudgeOutcomeMerged
				wasCorrected = true
			}
		}

		agg, ok := byGate[v.gate]
		if !ok {
			agg = newJudgeGateAgg()
			byGate[v.gate] = agg
		}
		agg.verdicts++
		if v.pass {
			agg.passed++
		}
		if wasCorrected {
			agg.corrected++
			rep.CorrectedVerdicts++
			correctedRuns[v.runID] = struct{}{}
		}
		agg.byOutcome[outcome]++
		bucket := bucketForScore(v.score)
		cells, ok := agg.bucketOutcomeCounts[bucket]
		if !ok {
			cells = make(map[string]int, 3)
			agg.bucketOutcomeCounts[bucket] = cells
		}
		cells[outcome]++
		switch outcome {
		case JudgeOutcomeMerged:
			agg.scoreSumMerged += v.score
			agg.countMerged++
		case JudgeOutcomeEscalated:
			agg.scoreSumEscalated += v.score
			agg.countEscalated++
		}

		rep.TotalVerdicts++
		if outcome != JudgeOutcomeOther {
			rep.JoinedVerdicts++
		}
		key := v.model + "\x00" + v.role
		m, ok := byModel[key]
		if !ok {
			m = &JudgeModel{Model: v.model, Role: v.role}
			byModel[key] = m
		}
		m.Verdicts++
	}

	rep.PerGate = make([]JudgeGate, 0, len(byGate))
	for _, gate := range sortedKeys(byGate) {
		agg := byGate[gate]
		row := JudgeGate{
			Gate:              gate,
			Verdicts:          agg.verdicts,
			Passed:            agg.passed,
			PassRate:          ratio(agg.passed, agg.verdicts),
			MergedVerdicts:    agg.byOutcome[JudgeOutcomeMerged],
			EscalatedVerdicts: agg.byOutcome[JudgeOutcomeEscalated],
			OtherVerdicts:     agg.byOutcome[JudgeOutcomeOther],
			CorrectedVerdicts: agg.corrected,
			Histogram:         make([]JudgeScoreBucket, 0, len(rep.Buckets)),
		}
		if agg.countMerged > 0 {
			row.MeanScoreMerged = agg.scoreSumMerged / float64(agg.countMerged)
		}
		if agg.countEscalated > 0 {
			row.MeanScoreEscalated = agg.scoreSumEscalated / float64(agg.countEscalated)
		}
		// Every bucket is emitted, including empty ones: the shape of the
		// distribution is the finding, and a missing row reads as missing
		// data rather than as zero.
		for _, bucket := range rep.Buckets {
			cells := agg.bucketOutcomeCounts[bucket]
			row.Histogram = append(row.Histogram, JudgeScoreBucket{
				Bucket:    bucket,
				Merged:    cells[JudgeOutcomeMerged],
				Escalated: cells[JudgeOutcomeEscalated],
				Other:     cells[JudgeOutcomeOther],
			})
		}
		rep.PerGate = append(rep.PerGate, row)
	}

	rep.Models = make([]JudgeModel, 0, len(byModel))
	for _, key := range sortedKeys(byModel) {
		rep.Models = append(rep.Models, *byModel[key])
	}
	rep.CorrectedRuns = len(correctedRuns)
	rep.ZeroEvidence = rep.TotalVerdicts == 0
	return rep, nil
}

// terminalOutcomesByRun reads the window's finished runs into a run → outcome
// label map. Paused runs land in "other": parked for a human is neither the
// judge being vindicated nor being wrong.
func terminalOutcomesByRun(ctx context.Context, runs RunOutcomeLister, since time.Time) (map[string]string, error) {
	rows, err := runs.ListTerminalOutcomesSince(ctx, since, judgeCalibrationRunLimit)
	if err != nil {
		return nil, fmt.Errorf("judge calibration: runs: %w", err)
	}
	if len(rows) >= judgeCalibrationRunLimit {
		return nil, fmt.Errorf("judge calibration: window holds at least %d terminal runs; narrow the window rather than join against a truncated run set", judgeCalibrationRunLimit)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r == nil || r.RunID == "" {
			continue
		}
		switch r.State {
		case store.PipelineDone:
			out[r.RunID] = JudgeOutcomeMerged
		case store.PipelineEscalated:
			out[r.RunID] = JudgeOutcomeEscalated
		default:
			out[r.RunID] = JudgeOutcomeOther
		}
	}
	return out, nil
}

// judgeVerdict is one decoded judge.verdict payload.
type judgeVerdict struct {
	runID string
	gate  string
	model string
	role  string
	score float64
	pass  bool
}

// parseJudgeVerdict decodes an event payload. A verdict missing its run or
// gate cannot be joined or grouped and is dropped rather than bucketed under
// an empty key; ok=false says so.
func parseJudgeVerdict(e *store.Event) (judgeVerdict, bool) {
	v := judgeVerdict{
		runID: payloadString(e.Payload, "run_id"),
		gate:  payloadString(e.Payload, "gate"),
		model: payloadString(e.Payload, "judge_model"),
		role:  payloadString(e.Payload, "role"),
	}
	if v.runID == "" {
		v.runID = e.SubjectID
	}
	if v.runID == "" || v.gate == "" {
		return judgeVerdict{}, false
	}
	if v.model == "" {
		v.model = "unknown"
	}
	if v.role == "" {
		v.role = "primary"
	}
	score, ok := payloadFloat(e.Payload, "score")
	if !ok {
		return judgeVerdict{}, false
	}
	v.score = score
	v.pass, _ = e.Payload["pass"].(bool)
	return v, true
}

// judgeScoreBucket maps a score onto its bucket label. Bounds are
// [0, 0.5) / [0.5, 0.8) / [0.8, ∞) so a verdict scored exactly at the default
// 0.8 threshold counts as a confident pass.
func bucketForScore(score float64) string {
	switch {
	case score < judgeBucketLowMax:
		return JudgeBucketLow
	case score < judgeBucketMidMax:
		return JudgeBucketMid
	default:
		return JudgeBucketHigh
	}
}

func payloadString(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}

// payloadFloat reads a numeric payload field. Payloads survive a JSON
// round-trip through the events table, so an int written by the runner comes
// back as float64 — both spellings are accepted.
func payloadFloat(payload map[string]any, key string) (float64, bool) {
	switch n := payload[key].(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}
