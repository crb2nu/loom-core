# Excelize advisory correction

Owner: loom-core maintainers. Review deadline: **2026-10-01 UTC**.

`GO-2026-6452` was published on 2026-09-16 with an unbounded affected range, although its references include the fix and v2.11.0 release. The [upstream release](https://github.com/qax-os/excelize/releases/tag/v2.11.0) and [GitHub advisory](https://github.com/advisories/GHSA-fx5j-qcqg-grpf) identify the in-memory fix in 2.11.0. That release still panics on negative shared-string indexes when strings spill to disk ([related advisory](https://github.com/qax-os/excelize/security/advisories/GHSA-48hm-4h8j-58fg)).

We pin **v2.11.1-0.20260728235842-f98df08a8f6a**, the upstream merge of [PR #2366](https://github.com/qax-os/excelize/pull/2366), which also fixes the disk-backed path. The regression test `TestXLSXSharedStringIndexBounds` exercises normal and forced disk-backed reads through GetCellValue, GetRows, and the row iterator, with valid and negative indexes. It reproduces a panic on v2.11.0. GetRows and the iterator can discard invalid-cell errors upstream; they must return no referenced string and must not panic.

Both CI systems retain the unmodified scanner JSON. A small filter removes only findings for GO-2026-6452, its reviewed 2026-09-16T18:00:43Z metadata, the exact Excelize module, and that exact fixed pseudo-version. Other versions, modules, updated advisory metadata, and other findings are preserved. The native `govulncheck -mode=convert` command still enforces the package-level gate. Scanner errors, invalid/incomplete reports, and an expired correction fail the job. JSON mode alone is never accepted as a passing gate.

Remove the correction once the Go database records the fixed range. If it remains incorrect at the deadline, re-check upstream and renew only with explicit evidence; do not extend the expiry automatically. Any later dependency version requires a fresh review.
