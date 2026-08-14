# RALPH slice — Audit-advisory follow-up digest (2026-07-20)

Owner: claude-code. Executes `190-roadmap-core-refinement-2026-07-18.md`
**Wave 2 item 1, second half**: "fold audit-advisory issues (#339–#355 class)
into a digest instead of one issue each."

## Review (state verified 2026-07-20)

- Roadmap-190 **Wave 1 fully shipped** since it was written: mill-floor views
  S1–S6, H1 spawn-timeout classification (`28779df3`), H2 judge empty-envelope
  at gates (`d9a73ecd`), S5 backend, escalation dedup-by-class (`c7032980`).
- Wave 2 item 1 **first half shipped**: ghost-spark auto-close (`5e82f409`),
  per-stage spawn agent (`f60cbc57`), HUD requeue action (`167220ac`).
- **Remaining gap (this slice):** live GitLab (`services/loom-core`, project 47)
  showed audit follow-up issues #339, #340, #342, #343, #345, #353, #354, #355 —
  all "_Opened automatically by the Loom Mills audit subsystem (advisory)_", one
  per council-artifact / pipeline-merge, nearly all survival 0.45 / warn,
  advisory-only (never block). ~1 every few hours = pure triage noise.
  `pkg/mills/audit/followup.go` opened one issue per sub-0.60-survival finding.

## Align — slice definition

Fold advisory audit findings into a **rolling per-UTC-day digest** issue
(one issue/day + a comment per finding), mirroring the escalation dedup idiom
(marker in `pkg/mills/pipeline` + optional capability interface + fail-open).

**Scope in**
- `pkg/mills/pipeline/audit_digest.go` — `AuditDigestMarker(period)` + `AuditDigestLabel`.
- `pkg/mills/audit/followup.go` — `DigestIssuer` capability interface; digest
  branch in `OnRecorded`; `recordToDigest` + `entry`/`digestBody`/`digestTitle`/`digestLabels`.
- `pkg/mills/clients/gitlab.go` — `FindOpenAuditDigest` (ListIssues + marker match).
- `cmd/loom-mills-operator/main.go` — compile-time guard `var _ audit.DigestIssuer = (*clients.GitLabClient)(nil)`.
- Tests + changelog fragment.

**Scope out** — policy/config change; `main.go` wiring (auto-detected via
capability); bulk-closing the EXISTING stale advisory issues (ops sweep, separate);
per-severity special-casing (critical still folds); HUD.

**Acceptance criteria** — all met:
1. N sub-threshold findings on the same UTC day → 1 `CreateIssue` + (N−1) `CommentIssue`. ✅
2. Digest body carries `mills-audit-digest:period=YYYY-MM-DD` marker + `audit-digest` label; each entry preserves subject/survival/severity/rubric/auditor-pool/findings. ✅
3. A finding on a new UTC day opens a fresh digest. ✅
4. Legacy Issuer (CreateIssue-only) behaviour unchanged (`TestFollowup_DoubleFireCreatesTwoIssues` still opens two). Above-threshold still no-op. Fail-open on every GitLab error. ✅
5. `FindOpenAuditDigest` matches by marker via `ListIssues`. ✅
6. `gofmt` clean; `go build`/`go test` green for audit+pipeline+clients+operator. ✅

**Risk notes**
- Find-or-create race across two operators — same risk the escalation dedup
  path accepts; a single operator drains the audit queue serially. Fail-open.
- Auto-enable via capability detection changes production behaviour on deploy
  with **no gitops/policy change** — intended, strictly less noise, advisory-only,
  and precedented by the escalation `DedupIssueClient` branch.

## Prove

- `gofmt -l` clean on all touched files.
- `GOWORK=off CGO_ENABLED=0 go build ./pkg/mills/... ./cmd/loom-mills-operator/...` → exit 0
  (operator guard compiling proves `*clients.GitLabClient` satisfies `DigestIssuer`).
- `go test ./pkg/mills/audit/... ./pkg/mills/pipeline/... ./pkg/mills/clients/...` → ok.
  6 new digest tests + 2 marker tests + all pre-existing followup tests pass.

## Handoff / follow-ups

- **Ops (separate, non-code):** bulk-close the existing stale advisory issues
  (#339,#340,#342,#343,#345,#353,#354,#355 …) now that new ones digest. A
  one-time sweep or a reconciler pass could fold+close them; not in this slice.
- **Optional:** promote `critical`-severity advisory findings (e.g. #340 survival
  0.00) back to their own issue if the digest buries them — deferred; everything
  is advisory in v2.0.
- Next RALPH candidates from roadmap-190 Wave 2: raise Mills concurrency under
  the S4 supervisor; dynamic workflows (Starlark) next slice; Pattern Loom A2
  engram verification.
