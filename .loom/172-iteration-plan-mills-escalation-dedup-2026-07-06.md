# Iteration Plan — Mills escalation dedup (one open issue per backlog item)

> RALPH slice, 2026-07-06. Branch `fix/mills-escalation-dedup`.
> Closes part of **DEBT-073 / #167 criterion e** (escalation dedup + auto-close).

## Context

#167's root problem: "67 of 100 open issues are bot-filed `mills-escalation`
incidents." Class a (ci_watch timeout) shipped in the prior RALPH slice (!965).
The escalation-lifecycle half (criterion e) was untouched: `Escalator.Handle`
(`escalate.go`) unconditionally called `CreateIssue`, so a backlog item that
kept failing filed a **new** issue every run — the pileup mechanism itself.

`followup.go:73` already flagged dedup as a known TODO; `ListIssues` already
exists on the client. Missing: a stable dedup key, a "find existing open
escalation" lookup, and a "comment on it" write.

## Scope

**In (dedup on create):**
- `EscalationDedupMarker(backlogID)` — stable invisible HTML-comment marker, embedded in every escalation issue body (`escalate.go`).
- Optional `DedupIssueClient` interface (`FindOpenEscalation` + `CommentIssue`); `Escalator.publishIssue` type-asserts for it and reuses an open issue (appends a recurrence note) instead of filing a duplicate.
- Concrete `GitLabClient.FindOpenEscalation` (marker match over newest ≤100 open `mills-escalation` issues) + `CommentIssue` (`POST /issues/:iid/notes`).
- Metric `mills_escalation_issues_deduped_total`.
- Unit tests (escalator dedup paths + client REST) + CHANGELOG.

**Out (follow-ups on #167):**
- **Auto-close** on a later successful run for the same item (separate slice; touches the success path, needs a `CloseIssue`/set-state method).
- **Class-granular** dedup (this slice dedups per backlog item only).
- One-time **bulk triage** of the stale ~60 markerless escalation issues (ops).

## Acceptance criteria

- [x] An open escalation issue exists for the item → escalator comments (recurrence note) and reuses its URL; **no** `CreateIssue`.
- [x] No open issue → fresh `CreateIssue`, and the new body carries `EscalationDedupMarker(item.ID)`.
- [x] Lookup error → **fail open** to a fresh create (escalation never dropped).
- [x] Existing-issue URL still flows to the agent handoff.
- [ ] `go build`/`vet`/`gofmt`/`golangci-lint`/`go test ./pkg/mills/...` clean.
- [ ] MR merged to `main` (pipeline-success gate).

## Risk notes

- **Contained blast radius**: the change is back-compat (activates only when the
  wired issue client implements `DedupIssueClient` — the concrete
  `*clients.GitLabClient` does, so it activates on deploy) and **fail-open**:
  worst case on any dedup-path error is "creates a duplicate like today" — no
  regression. No destructive op (no close/delete) in this slice.
- Wrong-issue match is guarded by matching a unique per-item marker containing
  the exact backlog id (backtick/HTML-comment delimited).
- `code`-path escalations pre-dating this ship lack the marker; dedup only
  applies to issues created after it ships (the markerless backlog is the
  bulk-triage follow-up).

## Test plan

- `pkg/mills/pipeline`: `TestEscalator_DedupReusesOpenIssue`, `…CreatesWhenNoOpenIssue`, `…FailsOpenOnLookupError`.
- `pkg/mills/clients`: `TestFindOpenEscalation_{MatchesMarker,NoMatch,EmptyBacklog,PropagatesListError}`, `TestCommentIssue_{PostsNote,RejectsZeroIID}`.
- Regression: full `go test ./pkg/mills/...` (only the known macOS fsnotify flake fails, unrelated).

## Riskiest assumption + kill-test

Internal refactor of the escalation-create seam + two new REST calls; no new
external-system behavior claim beyond the standard GitLab issues/notes API
(exercised by the client stub tests). Load-bearing internal assumption — the
operator wires the concrete `*clients.GitLabClient` as `Escalator.Issue`, so the
`DedupIssueClient` type-assertion fires in prod — verified at
`cmd/loom-mills-operator/main.go:439` (`NewEscalator(st, gitlabClient, …)`).
**Status**: passed 2026-07-06 (unit tests + wiring inspection). Live end-to-end
(a real second escalation deduping onto the first) is provable only via an
operator canary run, same as class a.
