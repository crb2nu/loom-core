# External-incident hold for the merge queue

When two consecutive terminal pipelines for a default branch are durably
classified as `external_dependency_incident`, Mills pauses that project and
branch's merge-queue lane. Candidates remain queued and report the deferral
reason `main_red_external`; they are not evicted or failed.

`merge_queue.main_red_external_hold_minutes` bounds the hold. Unset or non-positive
values use two hours, and values above 24 hours are clamped to 24 hours. At
expiry Mills writes one `mergequeue.main_red_external.expired` audit event and
its escalation marker in the same transaction, preventing repeated reconciliation
or daemon restarts from publishing duplicates. A new incident after recovery gets
its own escalation. Expiry releases the bounded hold; normal queue checks resume.

Any later terminal default-branch result that is successful, unclassified, or
classified as an internal failure clears the hold. Normal FIFO processing then
resumes on the next queue tick. Shift reports show active holds with remaining
time and expired holds as escalated.

## Persistence contract

CI classification producers call `pipeline.DefaultBranchCIRecorder.Record` with
only terminal results for the project's verified default branch. Pass the stable
pipeline identity and its original observation time, including successful and
unclassified outcomes, so intervening pipelines break the incident streak.
The recorder persists classifications before evaluating the latest two results;
re-delivery preserves the original ordering. `PolicyFn` can resolve the live Mills
policy; the configured duration is captured at activation and later observations
do not extend an existing hold.

The queue processor consults persisted holds before driving candidates or
preparing speculative successors. Deferred entries retain their queue state and
carry `detail.defer_reason=main_red_external`; recovery or expiry removes it.
Shift reports distinguish active, expired pending reconciliation, and escalated
holds, and omit recovered holds. Reports currently display the newest uncleared
lane hold.

Migration `043_main_red_external_hold.sql` adds the observation and hold tables.
Migration 038 retains the existing composite stamp identity schema. Store tests
cover a fresh database, upgrade from 042, repeated migration, and hold preservation.
