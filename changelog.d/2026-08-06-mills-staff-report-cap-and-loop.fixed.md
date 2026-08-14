Un-break the Mill Staff evidence reports on a busy mill, and stop the panel
hammering the operator. The promotion, judge-calibration, and config-outcome
reports capped their window scan at 10k events *before* filtering to the
actor prefix / event kinds they aggregate, so a two-week window of pipeline
bookkeeping tripped the fail-closed guard while the relevant rows numbered a
few hundred — every tile showed a 500. EventDAO gains prefix- and
kind-filtered window scans (riding the existing occurred_at index) and the
guard builders prefer them, so the cap now counts the report's own events;
plain EventLister fakes keep the unfiltered fail-closed fallback. Separately,
the Mill Staff store's refresh() read its report slots synchronously inside
the panel's mount $effect and wrote them on completion, re-triggering the
effect every round trip — one open tab refetched all six reports every ~1.5s
instead of every 60s. The reads are now untracked, with a regression test
that pins refresh-inside-a-tracking-effect to exactly one fetch per report.
Report tiles also surface the operator's own error body ("narrow the
window…") instead of a bare "Endpoint unreachable — url: 500".
