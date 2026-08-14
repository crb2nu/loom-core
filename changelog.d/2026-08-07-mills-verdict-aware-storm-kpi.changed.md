Storm detection and the escalation KPIs read run verdicts (Trustworthy
Verdicts S3). The foreman's `escalation_storm` rule now discounts
escalations whose verdict was superseded — their MRs merged, so they are
resolved incidents, and counting them kept storms alive on corrected
history; the anomaly's evidence carries `raw_count` and `superseded`
alongside the acted-on `count` so the discount is never silent. The KPI
snapshot gains `pipeline_escalated_active` (net of superseded) and
`pipeline_escalated_superseded`; the raw `pipeline_escalated_runs` gauge
keeps its historical meaning for existing dashboards. Item-state-driven
policy paths (relaunch candidates, auto-requeue) already exclude corrected
work because ghost-spark closure transitions the backlog item itself.
