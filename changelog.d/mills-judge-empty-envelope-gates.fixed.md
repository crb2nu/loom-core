- **Mills no longer burns the code-class retry budget when the LLM judge
  returns an ungradeable score envelope at the post_review gates**
  (`pkg/mills/pipeline/runner.go`, `pkg/mcperror/external_incidents.go`): the
  `spec_conformance` / `pr_self_review` gates could escalate finished work
  (run self-rated 0.91, all stages green, $2 spent) purely because the judge
  model returned an empty response (`raw=""`) that survived the client's own
  boosted-retry recovery (issue #348). The runner's gate-fail path treated
  that identically to a real content failure — rewinding to the (already
  successful, costly) `pr_self_review` stage, respawning the agent, and
  burning `maxAttempts` before escalating as `class=code`. The gate-fail
  branch now recognizes a judge empty/unparseable envelope, retries with a
  bounded set of FREE re-judges (a fresh judge call that re-invokes the
  client's larger-budget recovery, no agent respawn, no attempt-budget spend),
  and — if the verdict is still ungradeable — escalates as an external
  model-provider dependency incident (`class=config`, `retryable=false`) via
  the existing failure taxonomy instead of as code. Remediation: wait for the
  judge provider to recover, or raise `FLEXINFER_JUDGE_MAX_TOKENS` for
  reasoning models, then requeue.
