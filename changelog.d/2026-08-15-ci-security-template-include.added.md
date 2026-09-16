- **CI includes the fleet security template, adding SAST and secret detection**
  (`.gitlab-ci.yml`): the pipeline hand-rolled `security:gosec` and
  `security:govulncheck` but included no `platform/gitops` security template, so
  `platform/gitops/scripts/ci/ci_audit.py` flagged this repo — one of the three
  exemplars — `no-security-scanning`, and the repo had no committed-secret
  scanning of any kind. `/ci/templates/security.yml` is now included alongside
  the existing cache template. It is additive: `secret_detection` is new
  coverage, `semgrep-sast` brings a rule set gosec has no equivalent for, the
  template's own `gosec-sast` analyzer is retired upstream
  (`rules: - when: never`) so nothing duplicates `security:gosec`, and
  `govulncheck`'s module vulnerability database has no template counterpart, so
  `security:govulncheck` stays. Two adaptations are applied to the templates'
  hidden base jobs (`.sast-analyzer`, `.secret-analyzer`) rather than to
  concrete analyzer names, so they survive GitLab adding, renaming or retiring
  an analyzer: the analyzers pick up `before_script: *clone_repo`, because
  `GIT_STRATEGY` is `"none"` repo-wide and without their own fetch they would
  scan an empty working directory and report a clean result having read nothing;
  and `needs: []`, because security scanning is downstream of nothing. Both keep
  the template's `allow_failure: true` — their images resolve through
  `$CI_TEMPLATE_REGISTRY_HOST`, the one endpoint in this pipeline with no
  in-cluster mirror. Verified against the GitLab 18.4 CI Lint API with
  `include_merged_yml`: the merged config is valid with no warnings, the stage
  list is unchanged, no existing job's stage, `needs:` or variables changed, and
  exactly two analyzers instantiate on a branch pipeline (`semgrep-sast` and
  `secret_detection`) — the rest are gated off by `when: never` or by `exists:`
  globs this repo has no files for.
- **The `.gitlab-ci.yml` stage set records why it departs from the canonical
  order**: `prepare` exists to seed the shared Go module cache once, `deploy` is
  this repo's publish stage under an older name, and `build` precedes `test`
  because `test:integration` and `test:benchmark` `needs: build:binaries` while
  the release cross-compiles sit in `test` so they can `needs: test:unit`.
  Swapping the two stage names inverts both edges at once, so canonicalising the
  order is a re-plumbing of the needs graph rather than a rename — the comment
  exists so a conformance sweep does not attempt it blind.
