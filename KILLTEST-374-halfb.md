# Kill test #374 — Half B (mutating attribution)

**Status: NOT RUN. Requires a real GitLab mutation, which this implementation
agent did not perform.**

Half A (read-only observability) passed 2026-07-25 and is recorded in §3.3/§3.4
of `.loom/local/design-374-durable-rebase-sha-transitions.md`. This file is the
operator-runnable procedure for Half B. Slice 1's design-stated completion
criterion is that Half B passes before the ledger ships; the code and its tests
are complete and green against `httptest` fakes, but **no live rebase has been
issued**.

## What Half B proves

That a `PUT .../merge_requests/:iid/rebase` on GitLab 18.4.3 CE at
`gitlab.flexinfer.ai` produces **exactly one** observable head movement whose
`commit_from` equals the pre-rebase head — the predicate
`clients.GitLabClient.ObserveHead` uses to return `attributed` rather than
`ambiguous`.

If it produces MORE than one version row (e.g. an intermediate write), the
classifier's step-4 predicate must become "the last row's `head_commit_sha` ==
successor and every intermediate row chains from R". That is a change to
`classifyHead` in `pkg/mills/clients/gitlab_rebase.go` only — the data model,
the fence, and the rewind are unaffected.

## Preconditions

- `$GITLAB_TOKEN` with `api` scope on the scratch project.
- A **throwaway** project and branch. Do NOT run this against
  `services/loom-core` `main` or any branch with an open Mills run.
- Requests through the public edge need a browser-ish `User-Agent`; Cloudflare
  403s the default `urllib`/Go UA (error 1010).

Set once:

```bash
export GL=https://gitlab.flexinfer.ai/api/v4
export PROJ=<scratch-project-path-urlencoded>     # e.g. sandbox%2Frebase-killtest
export UA='Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36'
h=(-H "PRIVATE-TOKEN: $GITLAB_TOKEN" -H "User-Agent: $UA" -H 'Accept: application/json')
```

## Step 0 — create the throwaway branch + MR

```bash
# branch off main with one commit
curl -sS "${h[@]}" -X POST "$GL/projects/$PROJ/repository/branches?branch=killtest-374&ref=main"
curl -sS "${h[@]}" -X POST "$GL/projects/$PROJ/repository/files/killtest-374.txt" \
  -H 'Content-Type: application/json' \
  -d '{"branch":"killtest-374","content":"kill test 374\n","commit_message":"chore: kill test 374"}'

# advance main by one commit so the branch is genuinely behind (a rebase must
# have something to replay onto, or step 6's no-op is the only outcome)
curl -sS "${h[@]}" -X POST "$GL/projects/$PROJ/repository/files/killtest-374-target.txt" \
  -H 'Content-Type: application/json' \
  -d '{"branch":"main","content":"target moved\n","commit_message":"chore: move target for 374"}'

curl -sS "${h[@]}" -X POST "$GL/projects/$PROJ/merge_requests" \
  -H 'Content-Type: application/json' \
  -d '{"source_branch":"killtest-374","target_branch":"main","title":"kill test 374 (throwaway)"}'
# record the returned iid as $IID
export IID=<iid>
```

## Step 1 — record R and both cursors

```bash
curl -sS "${h[@]}" "$GL/projects/$PROJ/merge_requests/$IID?include_rebase_in_progress=true"
# record: .sha            -> R
curl -sS "${h[@]}" "$GL/projects/$PROJ/merge_requests/$IID/versions?per_page=100"
# record: max .[].id      -> V0   (versions cursor)
curl -sS "${h[@]}" "$GL/projects/$PROJ/events?action=pushed&per_page=100"
# record: max .id among rows whose .push_data.ref == "killtest-374"  -> E0
```

These three reads are exactly what `clients.GitLabClient.ReadHeadCursors` does
(`pkg/mills/clients/gitlab_rebase.go`), and the values map onto the ledger row's
`reviewed_sha`, `provenance_json.versions_cursor_before`, and
`provenance_json.events_cursor_before`.

## Step 2 — issue the rebase

```bash
curl -sS -i "${h[@]}" -X PUT "$GL/projects/$PROJ/merge_requests/$IID/rebase"
```

**Expect:** `HTTP/2 202` with body `{"rebase_in_progress":true}`.

## Step 3 — poll until it settles

```bash
for i in $(seq 1 60); do
  curl -sS "${h[@]}" "$GL/projects/$PROJ/merge_requests/$IID?include_rebase_in_progress=true"
  sleep 2
done
```

Stop when `.rebase_in_progress == false`. Record `.merge_error` (expect `null`)
and the new `.sha` -> **S**. Cap 120s — that is
`LOOM_MILLS_MERGE_REBASE_SETTLE_SECONDS` (default 120), and exhausting it is
what makes `ObserveHead` return `ambiguous`.

## Step 4 — assert exactly one movement on each ledger

```bash
curl -sS "${h[@]}" "$GL/projects/$PROJ/merge_requests/$IID/versions?per_page=100"
curl -sS "${h[@]}" "$GL/projects/$PROJ/events?action=pushed&per_page=100"
```

**PASS requires all of:**

| Assertion | Expected |
|---|---|
| version rows with `id > V0` | exactly **1** |
| that row's `head_commit_sha` | `S` |
| push events with `id > E0` and `push_data.ref == "killtest-374"` | 0 or exactly 1 |
| if 1 push: `push_data.commit_from` / `commit_to` | `R` / `S` |
| `merge_error` | `null` |
| `S` | ≠ `R` |

That is precisely `HeadVerdictAttributed` (step 4 of
`classifyHead`). Zero push events is tolerated by design — the activity feed is
asynchronous and `versions` is the primary witness.

**Expected ledger row** if this ran under a Mills pipeline run `<run_id>`
(`GET /api/mills/pipeline/runs/<run_id>/transitions`):

```json
{"seq": 1, "trigger": "rebase_request", "state": "attributed",
 "reviewed_sha": "<R>", "successor_sha": "<S>", "target_head_sha": "<main tip>",
 "settled_at": "<non-null>",
 "provenance": {"versions_cursor_before": <V0>, "events_cursor_before": <E0>,
                "versions_after": [{"id": <V0+n>, "head": "<S>", ...}],
                "pushes_after": [{"from": "<R>", "to": "<S>", "author": "root", ...}],
                "rebase_poll": {"attempts": <n>, "settled_after_ms": <ms>, "merge_error": ""},
                "classifier": "attributed",
                "reason": "exactly one movement; commit_from == reviewed_sha"}}
```

## Step 5 — negative variant (concurrent push ⇒ `ambiguous`)

Repeat steps 1–4, but push one commit to `killtest-374` in the ~2s window
immediately before step 2:

```bash
curl -sS "${h[@]}" -X POST "$GL/projects/$PROJ/repository/files/race.txt" \
  -H 'Content-Type: application/json' \
  -d '{"branch":"killtest-374","content":"race\n","commit_message":"chore: race the rebase"}' &
sleep 1; curl -sS -i "${h[@]}" -X PUT "$GL/projects/$PROJ/merge_requests/$IID/rebase"
```

**PASS requires:** step 4 now shows **two or more** version rows past the
cursor. The classifier must return `ambiguous`, never `attributed`.

**Expected ledger row:** `state: "ambiguous"`, `reason` beginning
`"2 movements observed; a rebase Mills requested produces exactly one"`.

## Step 6 — no-op variant (`noop`)

Repeat on a branch that is already on the target head (e.g. rebase twice in a
row with no intervening pushes).

**PASS requires:** `rebase_in_progress` settles `false`, `merge_error` is
`null`, and `.sha` is **unchanged**.

**Expected ledger row:** `state: "noop"`, `successor_sha == reviewed_sha`,
`reason: "mr head is unchanged (successor == reviewed sha)"`. A `noop` is
excluded from both `MaxSettledSeq` and `CountSettled`, so it must NOT
invalidate a CI authorization or spend the transition budget — verify the run's
merge is not fenced off afterwards.

## Cleanup

```bash
curl -sS "${h[@]}" -X PUT "$GL/projects/$PROJ/merge_requests/$IID" \
  -H 'Content-Type: application/json' -d '{"state_event":"close"}'
curl -sS "${h[@]}" -X DELETE "$GL/projects/$PROJ/repository/branches/killtest-374"
```

## Recording the result

Update the **Status** line of §2 in
`.loom/local/design-374-durable-rebase-sha-transitions.md` with
`passed YYYY-MM-DD` (or `FAILED`) plus the pasted commands and outputs for
steps 2–6.

## What is already proven without a live mutation

Every classifier branch above is pinned by `httptest`-fake tests in
`pkg/mills/clients/gitlab_rebase_test.go`:

| Half B assertion | Test |
|---|---|
| one movement ⇒ `attributed` | `TestObserveHead_OneMovementAttributed` |
| version row without a push event ⇒ `attributed` | `TestObserveHead_VersionWithoutPushEventAttributed` |
| two movements ⇒ `ambiguous` | `TestObserveHead_TwoMovementsAmbiguous` |
| `commit_from != reviewed` ⇒ `ambiguous` | `TestObserveHead_PushFromUnreviewedSHAAmbiguous` |
| unchanged head ⇒ `noop` | `TestObserveHead_UnchangedHeadIsNoop` |
| moved head, no witness ⇒ `ambiguous` | `TestObserveHead_MovedHeadWithoutWitnessIsAmbiguous` |
| settle deadline ⇒ `ambiguous` | `TestObserveHead_SettleDeadlineIsAmbiguous` |
| `merge_error` ⇒ `failed` | `TestObserveHead_MergeErrorIsFailed` |
| `include_rebase_in_progress=true` always sent | asserted inside `newObserveStub` |
| the observation path issues no mutations | `TestObserveHead_IssuesNoMutations` |

What the fakes cannot prove is the **vendor's** behaviour: that GitLab writes
exactly one version row per rebase. That is the only thing Half B adds, and it
is why it is a live test.
