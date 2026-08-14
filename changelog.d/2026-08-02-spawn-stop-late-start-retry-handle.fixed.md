Stop losing the retryable pod handle when StopSpawn races a late-starting
spawn and cleanup fails. The stop caller's deterministic fallback name
(`spawn-<id>`) could land after the late Start result's authoritative
container ID — via its `RecordStoppingPod` running outside `driversMu`, or
via `RecordStopCleanupFailure` re-recording the fallback — leaving the
durable retry handle pointing at the wrong name and leaking the real
container on substrates where the two differ. The fallback is now recorded
while still holding `driversMu` (so the late handle always wins), cleanup
failures no longer rewrite the handle, and the reconciler retry re-reads
current state instead of trusting a possibly stale snapshot.
