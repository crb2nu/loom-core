- **mills: signature-candidate mining — shadow-evidenced classifier proposals**
  (`pkg/mills/reconciler_signature_mining.go`, `pkg/mills/signature_normalize.go`,
  `pkg/mills/signature_cluster.go`, `pkg/mills/pipeline/known_signature.go`,
  `cmd/loom-mills-operator/handlers_signature_candidates.go`): a periodic reconciler
  sweep observes the factory's own unexplained failures and proposes classifier
  signatures as DATA. Each pass reads the escalations of the last `336h`
  (`PipelineDAO.ListEscalationEvidence` — the run's last non-empty stage log tail),
  drops every run the live classifiers already explain (any of the four
  `escalation_*` markers set, or a `pipeline.KnownFailureSignature` match — the real
  corpus, injected as a func because `pkg/mills` cannot import `pkg/mills/pipeline`),
  normalizes the rest (UUIDs, paths, durations, hex/SHAs, and numbers collapse to
  placeholder tokens), and groups what is left by the longest 3–8-token phrase at
  least three of them share. Deterministic and inspectable by design: no LLM, no
  embeddings, minimum cluster size 3, and a phrase must carry at least two real
  words so `<num> <path> <num>` is never proposed. Every cluster is a first-writer
  event (`signature.candidate`, actor `reconciler.signature_miner`, keyed on the
  normalized-phrase fingerprint) carrying `phrase`, `member_count`, up to 3 raw
  `sample_evidence` snippets (≤300 chars), `first_seen`, `last_seen`, and
  `window_match_count` — how many escalations across the WHOLE window the phrase
  would match if promoted, so over-firing is visible before a human writes the rule.
  Nothing is enforced: no run is reclassified and no retry decision changes;
  promotion stays a reviewed change to the classifier corpus. The sweep is off
  without a classifier predicate, rate-limited by
  `LOOM_MILLS_SIGNATURE_MINING_INTERVAL` (Go duration, default `6h`), bounded by its
  own timeout, and kept out of `TickResult` so a mining failure logs and never
  wedges a tick. `GET /api/mills/signature-candidates?window=<duration>` (default
  `336h`) lists what it proposed. Adds `mills_signature_mining_candidates_total`,
  `mills_signature_mining_texts_scanned_total`, and
  `mills_signature_mining_errors_total{stage}` (`pkg/mills/metrics.go`).
