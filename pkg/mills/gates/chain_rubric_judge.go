package gates

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RubricJudgeHop is a configured vendor. A nil Judge records an unavailable
// configuration without suppressing later vendors.
type RubricJudgeHop struct {
	Vendor, Model string
	Judge         RubricJudge
	Unavailable   string
}

// ChainRubricJudge walks independent vendors. Callbacks keep clients (which
// import gates) out of this package. The chain has no per-call mutable state.
type ChainRubricJudge struct {
	Hops          []RubricJudgeHop
	Classify      func(error) string
	BreakerReason func(vendor string) string
}

func (j *ChainRubricJudge) String() string {
	labels := make([]string, 0, len(j.Hops))
	for _, h := range j.Hops {
		labels = append(labels, h.Vendor+"/"+h.Model)
	}
	return strings.Join(labels, " → ")
}

func (j *ChainRubricJudge) Judge(ctx context.Context, rubric string, in StageInput) (RubricVerdict, error) {
	var skipped []string
	last := errors.New("tiebreaker chain exhausted")
	recovery := false
	for _, h := range j.Hops {
		if err := ctx.Err(); err != nil {
			return RubricVerdict{Skipped: skipped}, err
		}
		reason := h.Unavailable
		if j.BreakerReason != nil {
			if r := j.BreakerReason(h.Vendor); r != "" {
				reason = r
			}
		}
		if reason == "" && h.Judge == nil {
			reason = "not_configured"
		}
		if reason != "" {
			skipped = append(skipped, fmt.Sprintf("[tiebreak %s skipped: %s]", h.Vendor, reason))
			continue
		}
		v, err := h.Judge.Judge(ctx, rubric, in)
		if ctx.Err() != nil {
			return RubricVerdict{Skipped: skipped}, ctx.Err()
		}
		if err == nil {
			v.JudgedBy = "tiebreak:" + h.Vendor + "/" + v.Model
			if v.Model == "" {
				v.Model = h.Model
				v.JudgedBy = "tiebreak:" + h.Vendor + "/" + h.Model
			}
			v.Skipped = append(skipped, v.Skipped...)
			return v, nil
		}
		if errors.Is(err, context.Canceled) {
			return RubricVerdict{Skipped: skipped}, err
		}
		reason = "unknown"
		if j.Classify != nil {
			reason = j.Classify(err)
		}
		if isJudgeUnparseable(err) {
			reason = "parse"
		}
		skipped = append(skipped, fmt.Sprintf("[tiebreak %s skipped: %s]", h.Vendor, reason))
		last = err
		if recovery {
			break
		}
		switch reason {
		case "billing", "auth", "rate_limit", "overloaded", "transport":
		case "parse", "refusal":
			recovery = true
		default:
			return RubricVerdict{Skipped: skipped}, err
		}
	}
	return RubricVerdict{Skipped: skipped}, fmt.Errorf("tiebreaker chain exhausted: %w", last)
}
