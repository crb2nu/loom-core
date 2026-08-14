Mills: finish the escalation-class refactor (follow-ups to #408).

- The Integrator's declared class now survives to the Escalator and the DB.
  It stamped the class but discarded the result and never synced it onto the
  in-memory run, so `Escalator.Handle` built its `FailureRecord` from a blank
  struct, re-inferred a class from the reason prose, and
  `SetEscalationMetadata`'s `COALESCE` then overwrote the declared class in
  the row with the inferred one. A merge conflict declaring `config` could
  persist as whatever the needles decided — which changes run-budget
  accounting (`config` is discounted, `code` counts) and could mark a run
  auto-requeue-eligible. The Runner's sync is now a shared
  `applyEscalationMetadata` helper used by both paths.
- `maybeAutoRetry` gates on the declared `ErrorClass` instead of
  substring-matching `[class=…]` out of the reason. The prose matcher
  diverged in both directions: a genuinely transient escalation whose reason
  carried no marker never auto-retried, and a `code`-class gate exhaustion
  whose failDetail quoted a nested `[class=transient]` auto-retried against
  its own verdict. The marker remains operator-facing prose only.

Also note a metric semantics shift from the original refactor:
`mills_pipeline_escalation_class_total{class="unclassified"}` effectively
disappears now that every call site declares a class and gate exhaustion
resolves to `code`/`config`. The label set stays bounded by the `ErrorClass`
taxonomy, but dashboards or alerts keyed on `unclassified` will see a step
change to zero.
