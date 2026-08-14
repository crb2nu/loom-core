Retry every job automatically when CI infrastructure kills it, not just the Go
jobs. Between 2026-08-01 22:39Z and 2026-08-02 12:47Z, 119 jobs in this project
died with `runner_system_failure` — all of them the same apiserver→kubelet 502
on the node hosting the job pod, which the Kubernetes executor's attach strategy
cannot survive. Jobs that inherited `.go-template`'s retry stanza re-ran within
seconds; `build:frontend`, `fmt`, and `test:enterprise-smoke` had no stanza, so
they sat red for hours until a human pressed retry and they passed on unchanged
code. A top-level `default: retry:` now covers `runner_system_failure`,
`stuck_or_timeout_failure`, `api_failure`, `scheduler_failure`, and
`unknown_failure` for every job. `script_failure` stays excluded — a genuinely
red gate must stay red. Per-job stanzas that were narrower than the new default
were removed or widened, because a job-level `retry:` replaces the default
wholesale instead of merging with it.
