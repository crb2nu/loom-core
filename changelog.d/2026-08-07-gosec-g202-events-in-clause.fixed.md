Unblock main's security gate: gosec G202 flagged the events DAO's
kind-filtered window scan (added in the Mill Staff report fix) for building
its `IN (?,?,…)` placeholder list by string concatenation. The query binds
every value as a parameter — the concatenated string is only `"?"` repeats —
so this suppresses the finding with the same justified annotation the
workflow DAO's identical pattern already carries. The security job runs only
on main, so the original MR's branch pipeline was green while every main
pipeline (and therefore every deploy) failed from 03:34Z.
