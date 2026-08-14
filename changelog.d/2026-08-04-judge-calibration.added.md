- **Mills: judge verdicts persist, and a calibration report grades the graders**
  (`pkg/mills/gates/llm_judge.go`, `pkg/mills/pipeline/runner.go`,
  `pkg/mills/guard/judge_calibration.go`,
  `cmd/loom-mills-operator/handlers_judge_calibration.go`): the LLM gates'
  numeric scores were discarded on every pass and survived a fail only as prose
  inside `gate_outcomes.reasons_json`, so judge quality could never be measured
  against what the graded work actually did. `gates.Outcome` now carries the
  primary (and, on a dissent, tiebreaker) `Judgement`, the runner appends it as
  a best-effort `judge.verdict` event, and
  `GET /api/mills/judge-calibration?window=336h` joins those verdicts to their
  runs' terminal states — per gate: pass rate, mean score over merged vs
  escalated runs, and a score × outcome histogram. Runs still in flight are
  reported as `other`, never guessed.
