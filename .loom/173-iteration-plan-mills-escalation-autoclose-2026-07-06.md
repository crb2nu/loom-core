# Iteration Plan — Mills escalation auto-close on later success

> RALPH slice, 2026-07-06. Branch `fix/mills-escalation-autoclose`.
> Closes **DEBT-073 / #167 criterion e** (auto-close half). Pairs with the
> dedup half (!966) to fully close criterion e's code work.

## Context

Criterion e = "escalation issues deduped … AND auto-closed when a later run for
the same item succeeds." Dedup shipped in !966 (one open issue per item, marker
+ `DedupIssueClient`). This slice adds the auto-close half: reap that issue when
the item finally goes green so stale escalations don't linger open.

## Scope

**In:**
- `ClosableIssueClient` optional interface (`CloseIssue`) + concrete `GitLabClient.CloseIssue` (`PUT /issues/:iid` `state_event=close`).
- `EscalationResolver` optional interface (`ResolveOnSuccess`) + `*Escalator.ResolveOnSuccess` — reuse `FindOpenEscalation`, comment a resolution note, close.
- Wire into `Runner.markDone` (success terminal) via a nil-safe type-assert; best-effort.
- Metric `mills_escalation_issues_auto_closed_total`.
- Unit tests + CHANGELOG.

**Out (follow-ups on #167):**
- Class-granular dedup (marker could carry the class).
- One-time **bulk triage** of the stale ~60 markerless issues (ops).
- Final verification: canary run → zero false-positive escalations, open count < 10.

## Acceptance criteria

- [x] Successful run for an item with an open escalation → resolution comment + issue closed.
- [x] No open escalation → no close (item never escalated).
- [x] Issue client without dedup/close capability → safe no-op (nil error).
- [x] Resolution comment is soft (comment failure still closes).
- [x] Resolve failure never fails the run (`markDone` logs and returns nil).
- [ ] `go build`/`vet`/`gofmt`/`golangci-lint`/`go test ./pkg/mills/...` clean.
- [ ] MR merged to `main` (pipeline-success gate).

## Risk notes

- **Contained + best-effort**: hooked at `markDone` AFTER the run is already
  persisted `done`; the resolver is called under a type-assert (`EscalationResolver`)
  and its error is only logged. It can never fail or roll back a successful merge.
- **Non-destructive beyond intent**: only closes the issue matched by the item's
  own dedup marker (`FindOpenEscalation`), the same lookup dedup uses — it won't
  close an unrelated issue.
- Nil-safe: `markDone`'s assert skips a nil `Escalator`; `ResolveOnSuccess`
  additionally guards `e == nil` / `e.Issue == nil` (typed-nil interface case).

## Test plan

- `pkg/mills/pipeline`: `TestEscalator_ResolveOnSuccess{ClosesOpenIssue,NoOpWhenNoOpenIssue,NoOpWithoutDedupClient,ClosesDespiteCommentError}`.
- `pkg/mills/clients`: `TestCloseIssue_{PutsStateEventClose,RejectsZeroIID}`.
- Regression: full `go test ./pkg/mills/...` (only the known macOS fsnotify flake fails, unrelated).

## Riskiest assumption + kill-test

Internal wiring of an existing seam + one new REST call (`PUT issue state_event=close`,
exercised by the client stub test). Load-bearing internal assumption — the runner's
`markDone` runs on every successful pipeline and `r.Escalator` is the concrete
`*Escalator` (satisfies `EscalationResolver`) — verified by the runner tests +
`cmd/loom-mills-operator/main.go` wiring. **Status**: passed 2026-07-06 (unit tests +
wiring inspection). Live end-to-end (a real escalation closing after a later green
run) is provable only via an operator canary run.
