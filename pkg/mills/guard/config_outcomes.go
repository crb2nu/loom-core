package guard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	// configOutcomeEventLimit and configOutcomeRunLimit bound the two window
	// scans. Saturating either is an error rather than a truncation: a win rate
	// computed from a partial window ranks configurations against a reality the
	// report never saw, and the whole point of the read is to decide which
	// configuration to keep.
	configOutcomeEventLimit = 10000
	configOutcomeRunLimit   = 10000

	// ConfigChecksumUnknown labels a stamp whose policy_checksum was recorded
	// empty (no PolicyManager wired). It is a distinct bucket, never folded
	// into a real revision: "we don't know" and "revision X" are different
	// answers to the same question.
	ConfigChecksumUnknown = "unknown"
)

// Terminal-outcome labels, shared with the judge calibration report so the two
// learning-loop readouts partition runs identically. "other" covers paused
// runs, runs still in flight, and runs the window's run scan never saw.
const (
	ConfigOutcomeMerged    = JudgeOutcomeMerged
	ConfigOutcomeEscalated = JudgeOutcomeEscalated
	ConfigOutcomeOther     = JudgeOutcomeOther
	// ConfigOutcomeMergedAfterEscalation labels a run whose escalation was
	// superseded because its MR merged (Trustworthy Verdicts S2). It is a
	// win for MergeRate but never folded into plain merged.
	ConfigOutcomeMergedAfterEscalation = mills.RunVerdictClassMergedAfterEscalation
)

// ConfigOutcomeReport answers "which configuration actually ships work?" over a
// window: every run.provenance stamp joined to what its run finally did, what
// it cost, how its own judges graded it, and whether its merge was later
// reverted.
type ConfigOutcomeReport struct {
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// StampedRuns counts the run.provenance stamps in the window — the runs
	// this report can attribute to a configuration at all.
	StampedRuns int `json:"stamped_runs"`
	// UncoveredRuns counts terminal runs in the window carrying no stamp: the
	// blind spot behind every rate below. Recursion subruns are stamped on
	// their parent by design and land here too, so a nonzero count is not by
	// itself a provenance defect.
	UncoveredRuns     int                      `json:"uncovered_runs"`
	Totals            ConfigOutcomeStats       `json:"totals"`
	PerPolicyChecksum []PolicyOutcomeGroup     `json:"per_policy_checksum"`
	PerStageModel     []StageModelOutcomeGroup `json:"per_stage_model"`
	Regressions       ConfigRegressionSummary  `json:"regressions"`
	// ZeroEvidence marks a window that stamped no runs at all. A configuration
	// that never ran has no win rate, so the absence of evidence is a stated
	// finding rather than an empty table read as a clean bill.
	ZeroEvidence bool `json:"zero_evidence"`
}

// ConfigOutcomeStats is one configuration's win-rate row. The same shape serves
// the window totals so a group can be read against the window it came from
// without translating between two vocabularies.
type ConfigOutcomeStats struct {
	// Runs counts stamped runs attributed to this configuration; Merged,
	// MergedAfterEscalation, Escalated and Other partition it exactly.
	Runs   int `json:"runs"`
	Merged int `json:"merged"`
	// MergedAfterEscalation counts runs whose escalation verdict was
	// superseded because the work landed (Trustworthy Verdicts S2). Wins for
	// MergeRate, but never folded into Merged — the detour stays visible.
	MergedAfterEscalation int `json:"merged_after_escalation"`
	Escalated             int `json:"escalated"`
	Other                 int `json:"other"`
	// MergeRate is (Merged+MergedAfterEscalation)/Runs — landed is landed —
	// with in-flight runs included in the denominator: a configuration whose
	// runs never finish is not winning.
	MergeRate float64 `json:"merge_rate"`
	// JudgeGradedRuns is the denominator of the two judge figures. Both are
	// averaged per RUN, not per verdict, because judge.verdict is appended once
	// per judge per gate evaluation and a retried gate would otherwise let one
	// run dominate its group.
	JudgeGradedRuns int     `json:"judge_graded_runs"`
	MeanJudgeScore  float64 `json:"mean_judge_score"`
	JudgePassRate   float64 `json:"judge_pass_rate"`
	// CostedRuns is the number of runs whose terminal record carried a cost,
	// and the denominator of MeanCostUSD — in-flight runs contribute no cost
	// and must not dilute the average.
	CostedRuns   int     `json:"costed_runs"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	MeanCostUSD  float64 `json:"mean_cost_usd"`
	// Regressions counts post-merge reverts attributed to merge requests this
	// configuration's runs produced. Only regressions whose MR links back to a
	// run inside the window are attributable; see ConfigRegressionSummary.
	Regressions int `json:"regressions"`
}

// PolicyOutcomeGroup is one policy revision's row.
type PolicyOutcomeGroup struct {
	PolicyChecksum string `json:"policy_checksum"`
	ConfigOutcomeStats
}

// StageModelOutcomeGroup is one (stage, model) pin's row. A run contributes to
// every stage it pinned, so these rows overlap and their Runs do not sum to the
// window total.
type StageModelOutcomeGroup struct {
	Stage string `json:"stage"`
	Model string `json:"model"`
	ConfigOutcomeStats
}

// ConfigRegressionSummary states how much of the window's regression evidence
// the report could attribute to a configuration at all.
type ConfigRegressionSummary struct {
	// Total counts every regression.attributed event in the window.
	Total int `json:"total"`
	// Linked counts the regressions whose regressed MR maps to a stamped run
	// inside the window. Only these appear in the per-group Regressions counts.
	Linked int `json:"linked"`
	// Unlinked counts the rest: a revert can land weeks after the merge, so a
	// regression on work produced before the window is real evidence about a
	// configuration this window cannot name. It is reported, never guessed at.
	Unlinked int `json:"unlinked"`
}

// runProvenance is one decoded run.provenance stamp.
type runProvenance struct {
	runID          string
	policyChecksum string
	stageModels    map[string]string
}

// runOutcome is the joined per-run fact table the groups aggregate over.
type runOutcome struct {
	outcome string
	// costed distinguishes "cost 0.00" from "no terminal record yet".
	costed        bool
	costUSD       float64
	judgeVerdicts int
	judgeScore    float64
	judgePass     float64
	regressions   int
}

// judgeRollup accumulates one run's verdicts before they are averaged into a
// single per-run data point.
type judgeRollup struct {
	verdicts int
	passed   int
	scoreSum float64
}

// statsAgg accumulates one group's cells before the report is sorted.
type statsAgg struct {
	ConfigOutcomeStats
	scoreSum    float64
	passRateSum float64
}

func (a *statsAgg) add(r runOutcome) {
	a.Runs++
	switch r.outcome {
	case ConfigOutcomeMerged:
		a.Merged++
	case ConfigOutcomeMergedAfterEscalation:
		a.MergedAfterEscalation++
	case ConfigOutcomeEscalated:
		a.Escalated++
	default:
		a.Other++
	}
	if r.costed {
		a.CostedRuns++
		a.TotalCostUSD += r.costUSD
	}
	if r.judgeVerdicts > 0 {
		a.JudgeGradedRuns++
		a.scoreSum += r.judgeScore
		a.passRateSum += r.judgePass
	}
	a.Regressions += r.regressions
}

func (a *statsAgg) finish() ConfigOutcomeStats {
	out := a.ConfigOutcomeStats
	out.MergeRate = ratio(out.Merged+out.MergedAfterEscalation, out.Runs)
	if out.JudgeGradedRuns > 0 {
		out.MeanJudgeScore = a.scoreSum / float64(out.JudgeGradedRuns)
		out.JudgePassRate = a.passRateSum / float64(out.JudgeGradedRuns)
	}
	if out.CostedRuns > 0 {
		out.MeanCostUSD = out.TotalCostUSD / float64(out.CostedRuns)
	}
	return out
}

// BuildConfigOutcomeReport joins the run.provenance stamps recorded over
// [since, now] to the terminal outcomes, costs, judge grades and post-merge
// regressions of the runs they describe. Pure over the two read surfaces: no
// store writes, no clock reads.
func BuildConfigOutcomeReport(ctx context.Context, events EventLister, runs RunOutcomeLister, since, now time.Time) (ConfigOutcomeReport, error) {
	if events == nil {
		return ConfigOutcomeReport{}, errors.New("config outcomes: events lister required")
	}
	if runs == nil {
		return ConfigOutcomeReport{}, errors.New("config outcomes: runs lister required")
	}
	if !since.Before(now) {
		return ConfigOutcomeReport{}, fmt.Errorf("config outcomes: window start %s is not before end %s",
			since.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	terminal, err := terminalRunsSince(ctx, runs, since)
	if err != nil {
		return ConfigOutcomeReport{}, err
	}

	// Prefer the kind-filtered scan so the truncation cap below counts the
	// three kinds this report joins, not the whole firehose (see
	// ActorPrefixLister). The in-memory kind switch stays: it also guards the
	// fallback path.
	var raw []*store.Event
	if kl, ok := events.(KindLister); ok {
		raw, err = kl.ListSinceByKinds(ctx, []string{
			mills.RunProvenanceEventKind,
			store.JudgeVerdictEventKind,
			mills.RegressionAttributedEventKind,
			mills.RunVerdictKindGhostSparkMerged,
			mills.GhostSparkClosedEventKind,
		}, since, configOutcomeEventLimit)
	} else {
		raw, err = events.ListSince(ctx, since, configOutcomeEventLimit)
	}
	if err != nil {
		return ConfigOutcomeReport{}, fmt.Errorf("config outcomes: %w", err)
	}
	if len(raw) >= configOutcomeEventLimit {
		return ConfigOutcomeReport{}, fmt.Errorf("config outcomes: window holds at least %d events; narrow the window rather than rank configurations on a truncated sample", configOutcomeEventLimit)
	}
	corrected := correctedRunIDs(raw, since, now)

	var (
		stamps     = make(map[string]runProvenance)
		grades     = make(map[string]*judgeRollup)
		regressed  []int64
		regByRunID = make(map[string]int)
	)
	for _, e := range raw {
		if e == nil {
			continue
		}
		// ListSince bounds the window's start; the end is bounded here so a
		// clock-skewed future event cannot land in a closed window.
		if e.OccurredAt.Before(since) || e.OccurredAt.After(now) {
			continue
		}
		switch e.Kind {
		case mills.RunProvenanceEventKind:
			p, ok := parseRunProvenance(e)
			if !ok {
				continue
			}
			// The stamp is append-once per (subject_kind, run), but the two
			// lanes stamp under different subject kinds; first writer wins so a
			// run can never be counted twice.
			if _, seen := stamps[p.runID]; !seen {
				stamps[p.runID] = p
			}
		case store.JudgeVerdictEventKind:
			v, ok := parseJudgeVerdict(e)
			if !ok {
				continue
			}
			g, ok := grades[v.runID]
			if !ok {
				g = &judgeRollup{}
				grades[v.runID] = g
			}
			g.verdicts++
			g.scoreSum += v.score
			if v.pass {
				g.passed++
			}
		case mills.RegressionAttributedEventKind:
			if iid, ok := regressedMRIID(e); ok {
				regressed = append(regressed, iid)
			}
		}
	}

	rep := ConfigOutcomeReport{
		WindowStart: since.UTC(),
		WindowEnd:   now.UTC(),
		StampedRuns: len(stamps),
	}
	for _, iid := range regressed {
		rep.Regressions.Total++
		runID, ok := terminal.runByMR[iid]
		if !ok {
			rep.Regressions.Unlinked++
			continue
		}
		if _, stamped := stamps[runID]; !stamped {
			rep.Regressions.Unlinked++
			continue
		}
		rep.Regressions.Linked++
		regByRunID[runID]++
	}
	for runID := range terminal.byRun {
		if _, stamped := stamps[runID]; !stamped {
			rep.UncoveredRuns++
		}
	}

	var (
		totals     statsAgg
		byPolicy   = make(map[string]*statsAgg)
		byStageMdl = make(map[string]*statsAgg)
	)
	for runID, p := range stamps {
		joined := runOutcome{outcome: ConfigOutcomeOther, regressions: regByRunID[runID]}
		if row, ok := terminal.byRun[runID]; ok {
			joined.outcome = outcomeLabel(row.State)
			// A corrected escalation (verdict superseded — its MR merged)
			// partitions under its own label, never plain merged.
			if joined.outcome == ConfigOutcomeEscalated {
				if _, wasCorrected := corrected[runID]; wasCorrected {
					joined.outcome = ConfigOutcomeMergedAfterEscalation
				}
			}
			joined.costed = true
			joined.costUSD = row.CostUSD
		}
		if g, ok := grades[runID]; ok && g.verdicts > 0 {
			joined.judgeVerdicts = g.verdicts
			joined.judgeScore = g.scoreSum / float64(g.verdicts)
			joined.judgePass = ratio(g.passed, g.verdicts)
		}
		totals.add(joined)
		aggFor(byPolicy, p.policyChecksum).add(joined)
		for stage, model := range p.stageModels {
			aggFor(byStageMdl, stage+"\x00"+model).add(joined)
		}
	}

	rep.Totals = totals.finish()
	rep.PerPolicyChecksum = make([]PolicyOutcomeGroup, 0, len(byPolicy))
	for _, checksum := range sortedKeys(byPolicy) {
		rep.PerPolicyChecksum = append(rep.PerPolicyChecksum, PolicyOutcomeGroup{
			PolicyChecksum: checksum, ConfigOutcomeStats: byPolicy[checksum].finish(),
		})
	}
	// Keys are "<stage>\x00<model>", so sorting them stage-major needs no
	// second comparator: NUL sorts below every character a stage name holds.
	rep.PerStageModel = make([]StageModelOutcomeGroup, 0, len(byStageMdl))
	for _, key := range sortedKeys(byStageMdl) {
		stage, model := splitStageModelKey(key)
		rep.PerStageModel = append(rep.PerStageModel, StageModelOutcomeGroup{
			Stage: stage, Model: model, ConfigOutcomeStats: byStageMdl[key].finish(),
		})
	}
	rep.ZeroEvidence = rep.StampedRuns == 0
	return rep, nil
}

// terminalRuns is the window's finished runs indexed both ways a config-outcome
// join needs them: by run, and by the merge request each run produced.
type terminalRuns struct {
	byRun   map[string]*store.RunTerminalOutcome
	runByMR map[int64]string
}

// terminalRunsSince reads the window's finished runs. Rows arrive newest-first,
// so when several attempts share one MR the newest attempt wins the MR index —
// it is the run that landed the merge the revert later undid.
func terminalRunsSince(ctx context.Context, runs RunOutcomeLister, since time.Time) (terminalRuns, error) {
	rows, err := runs.ListTerminalOutcomesSince(ctx, since, configOutcomeRunLimit)
	if err != nil {
		return terminalRuns{}, fmt.Errorf("config outcomes: runs: %w", err)
	}
	if len(rows) >= configOutcomeRunLimit {
		return terminalRuns{}, fmt.Errorf("config outcomes: window holds at least %d terminal runs; narrow the window rather than join against a truncated run set", configOutcomeRunLimit)
	}
	out := terminalRuns{
		byRun:   make(map[string]*store.RunTerminalOutcome, len(rows)),
		runByMR: make(map[int64]string, len(rows)),
	}
	for _, r := range rows {
		if r == nil || r.RunID == "" {
			continue
		}
		if _, seen := out.byRun[r.RunID]; !seen {
			out.byRun[r.RunID] = r
		}
		if r.MRIID != nil {
			if _, seen := out.runByMR[*r.MRIID]; !seen {
				out.runByMR[*r.MRIID] = r.RunID
			}
		}
	}
	return out, nil
}

func outcomeLabel(state store.PipelineState) string {
	switch state {
	case store.PipelineDone:
		return ConfigOutcomeMerged
	case store.PipelineEscalated:
		return ConfigOutcomeEscalated
	default:
		return ConfigOutcomeOther
	}
}

// parseRunProvenance decodes a run.provenance payload. A stamp without a run
// cannot be joined to anything and is dropped rather than aggregated under an
// empty key. An empty policy_checksum is recorded as "unknown" — the stamp
// deliberately writes it empty rather than omitting it, and collapsing that
// into a real revision would attribute wins to a configuration nobody ran.
func parseRunProvenance(e *store.Event) (runProvenance, bool) {
	p := runProvenance{
		runID:          payloadString(e.Payload, "run_id"),
		policyChecksum: payloadString(e.Payload, "policy_checksum"),
	}
	if p.runID == "" {
		p.runID = e.SubjectID
	}
	if p.runID == "" {
		return runProvenance{}, false
	}
	if p.policyChecksum == "" {
		p.policyChecksum = ConfigChecksumUnknown
	}
	p.stageModels = payloadStringMap(e.Payload, "stage_models")
	return p, true
}

// regressedMRIID reads the regressed merge request off an attribution event.
// The payload survives a JSON round-trip through the events table, so the iid
// comes back as float64; the subject id is the fallback the dedup key wrote.
func regressedMRIID(e *store.Event) (int64, bool) {
	if n, ok := payloadFloat(e.Payload, "regressed_mr_iid"); ok {
		return int64(n), true
	}
	if iid, err := strconv.ParseInt(e.SubjectID, 10, 64); err == nil {
		return iid, true
	}
	return 0, false
}

// payloadStringMap decodes a map-valued payload field, dropping entries whose
// key or value is empty: an unpinned stage is absent from the stamp, and a
// blank model is not a configuration to rank.
func payloadStringMap(payload map[string]any, key string) map[string]string {
	raw, ok := payload[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok || k == "" || s == "" {
			continue
		}
		out[k] = s
	}
	return out
}

func aggFor(m map[string]*statsAgg, key string) *statsAgg {
	a, ok := m[key]
	if !ok {
		a = &statsAgg{}
		m[key] = a
	}
	return a
}

func splitStageModelKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
