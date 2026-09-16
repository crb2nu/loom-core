// Package boltcard composes the operator-facing terminal-work read model.
package boltcard

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type MR struct {
	IID *int64 `json:"iid"`
	URL string `json:"url"`
}

type Diff struct {
	Files   int    `json:"files"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Source  string `json:"source"`
}

type Eval struct {
	MinScore *float64 `json:"min_score"`
	Verdicts []any    `json:"verdicts"`
}

type Escalation struct {
	Class           string `json:"class"`
	Signature       string `json:"signature"`
	WeekOccurrences int    `json:"week_occurrences"`
}

type Grade struct {
	Value string    `json:"value"`
	Note  string    `json:"note"`
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}

type Deploy struct {
	State    string `json:"state"`
	MergeSHA string `json:"merge_sha"`
	BuildSHA string `json:"build_sha"`
	Reason   string `json:"reason"`
}

type Card struct {
	RunID        *string     `json:"run_id"`
	BacklogID    string      `json:"backlog_id"`
	Title        string      `json:"title"`
	SpecHeadline string      `json:"spec_headline"`
	Outcome      string      `json:"outcome"`
	MergedAt     *time.Time  `json:"merged_at"`
	MR           MR          `json:"mr"`
	Diff         Diff        `json:"diff"`
	Eval         Eval        `json:"eval"`
	GatesFailed  []string    `json:"gates_failed"`
	Attempts     int         `json:"attempts"`
	CostUSD      float64     `json:"cost_usd"`
	Template     string      `json:"template"`
	Escalation   *Escalation `json:"escalation"`
	Regression   any         `json:"regression"`
	Stamp        *string     `json:"stamp"`
	Grade        *Grade      `json:"grade"`
	Deploy       *Deploy     `json:"deploy"`
}

// SpecHeadline returns the first non-empty line, truncated rune-safely.
func SpecHeadline(spec string) string {
	for _, line := range strings.Split(strings.ReplaceAll(spec, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > 160 {
			r = r[:160]
		}
		return string(r)
	}
	return ""
}

// DiffFromStages applies the persisted-artifacts then raw-patch precedence.
func DiffFromStages(stages []*store.StageResult) Diff {
	for i := len(stages) - 1; i >= 0; i-- {
		a := stages[i].Artifacts
		if a == nil {
			continue
		}
		added, aok := intValue(a["lines_added"])
		removed, rok := intValue(a["lines_removed"])
		files, fok := fileCount(a["files_changed"])
		if aok || rok || fok {
			return Diff{Files: files, Added: added, Removed: removed, Source: "artifacts"}
		}
	}
	for i := len(stages) - 1; i >= 0; i-- {
		patch, _ := stages[i].Artifacts["diff_patch"].(string)
		if patch == "" {
			continue
		}
		added, removed := gates.CountDiffLines([]byte(patch))
		return Diff{Files: patchFiles(patch), Added: added, Removed: removed, Source: "patch_count"}
	}
	return Diff{}
}

func patchFiles(patch string) int {
	seen := map[string]struct{}{}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			seen[parts[3]] = struct{}{}
		}
	}
	return len(seen)
}

func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	}
	return 0, false
}

func fileCount(v any) (int, bool) {
	if n, ok := intValue(v); ok {
		return n, true
	}
	switch xs := v.(type) {
	case []any:
		return len(xs), true
	case []string:
		return len(xs), true
	}
	return 0, false
}

// EvalFromStages reduces persisted run_evidence/verdicts artifacts.
func EvalFromStages(stages []*store.StageResult) Eval {
	out := Eval{Verdicts: []any{}}
	for _, s := range stages {
		var raw any
		if s.Artifacts != nil {
			raw = s.Artifacts["run_evidence"]
		}
		m, _ := raw.(map[string]any)
		if m == nil {
			m = s.Artifacts
		}
		vs, _ := m["verdicts"].([]any)
		for _, v := range vs {
			out.Verdicts = append(out.Verdicts, v)
			vm, _ := v.(map[string]any)
			score, ok := number(vm["score"])
			if ok && (out.MinScore == nil || score < *out.MinScore) {
				x := score
				out.MinScore = &x
			}
		}
	}
	return out
}

// EvalFromEvidence reduces the verdict list exposed by a run's evidence.
func EvalFromEvidence(evidence map[string]any) Eval {
	out := Eval{Verdicts: []any{}}
	if evidence == nil {
		return out
	}
	switch verdicts := evidence["verdicts"].(type) {
	case []map[string]any:
		for _, verdict := range verdicts {
			out = appendVerdict(out, verdict)
		}
	case []any:
		for _, raw := range verdicts {
			if verdict, ok := raw.(map[string]any); ok {
				out = appendVerdict(out, verdict)
			}
		}
	}
	return out
}

func appendVerdict(out Eval, verdict map[string]any) Eval {
	out.Verdicts = append(out.Verdicts, verdict)
	if score, ok := number(verdict["score"]); ok && (out.MinScore == nil || score < *out.MinScore) {
		x := score
		out.MinScore = &x
	}
	return out
}

func StampFromEvidence(evidence map[string]any) *string {
	if evidence == nil {
		return nil
	}
	provenance, _ := evidence["provenance"].(map[string]any)
	for _, key := range []string{"pattern_slug", "pattern", "slug"} {
		if value, ok := provenance[key].(string); ok && strings.TrimSpace(value) != "" {
			stamp := strings.TrimSpace(value)
			return &stamp
		}
	}
	return nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// FinalFailedGates returns only failures from the final evaluated attempt.
func FinalFailedGates(gatesIn []*store.GateOutcome) []string {
	if len(gatesIn) == 0 {
		return []string{}
	}
	latest := make(map[string]*store.GateOutcome, len(gatesIn))
	for _, g := range gatesIn {
		prior := latest[g.GateName]
		if prior == nil || g.EvaluatedAt.After(prior.EvaluatedAt) || (g.EvaluatedAt.Equal(prior.EvaluatedAt) && g.ID > prior.ID) {
			latest[g.GateName] = g
		}
	}
	seen := map[string]struct{}{}
	for _, g := range latest {
		if g.Outcome == store.GateOutcomeFail {
			seen[g.GateName] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func GradeFromItem(item *store.BacklogItem) *Grade {
	if item == nil || item.Grade == "" || item.GradedAt == nil {
		return nil
	}
	return &Grade{Value: item.Grade, Note: item.GradeNote, Actor: item.GradeActor, At: *item.GradedAt}
}

func StampFromPlanID(planID string) *string {
	const prefix = "plan-stamp-"
	if !strings.HasPrefix(planID, prefix) {
		return nil
	}
	s := strings.TrimPrefix(planID, prefix)
	if i := strings.LastIndex(s, "-"); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return nil
	}
	return &s
}

func StringArtifact(stages []*store.StageResult, key string) string {
	for i := len(stages) - 1; i >= 0; i-- {
		if v, ok := stages[i].Artifacts[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func RegressionFromStages(stages []*store.StageResult) any {
	for i := len(stages) - 1; i >= 0; i-- {
		if v, ok := stages[i].Artifacts["regression_evidence"]; ok {
			return v
		}
	}
	return nil
}

func ValidUTF8(s string) bool { return utf8.ValidString(s) }
func Outcome(state store.PipelineState) string {
	if state == store.PipelineDone {
		return "merged"
	}
	if state == store.PipelineEscalated {
		return "escalated"
	}
	return string(state)
}
func MRURL(base string, iid *int64) string {
	if iid == nil || *iid <= 0 || base == "" {
		return ""
	}
	return fmt.Sprintf("%s/-/merge_requests/%d", strings.TrimRight(base, "/"), *iid)
}
