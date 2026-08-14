// schema_patterns_loomcore.go -- the repo-native "dobby card" catalog:
// parameterized patterns for the recurring BROWNFIELD work classes observed in
// real Mills demand (docs/FACTORY_MODEL.md §5 kill-test, 2026-08-01: the
// greenfield catalog covered ~5% of the last 20 real terminal items, while
// these four classes covered ~35–40%).
//
// Unlike the greenfield seeds, these cards stamp INTO loom-core's existing
// tree: slice files are concrete repo paths (no target_dir), pins encode the
// repo's own conventions (extracted from canonical exemplar commits, cited
// per pattern), and engram file_ref proofs point at the in-repo exemplars.
// All four passed their kill-tests on 2026-08-01 (context-free follower +
// independent gauge — the !911 method) and ship approved at v0.2, whose pins
// close the boundary gaps the follower runs surfaced; the J2 auto-harvest
// keeps feeding their provenance from live merged stamps.
package agentcontext

// loomRunbookPattern is pattern-loom-runbook v0.1: an operator runbook page
// under docs/runbooks/. Mined from docs/mills-incident-runbook.md,
// docs/runbooks/infra-incidents.md, and the five-duplicate-runbooks incident
// (CHANGELOG 2026-07-31) — which is why this card makes the dedup search and
// the MILLS_RUNBOOK.md index link MANDATORY when today's practice has neither.
func loomRunbookPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-loom-runbook",
		Slug:        "loom-runbook",
		Name:        "Mills operator runbook page",
		Makes:       "operator runbook document (docs/runbooks/)",
		Description: "A single operator runbook following the repo's fixed structure (H1 + 'Use this runbook when…' opener; signature-major five-### blocks or phase-major classification table), with a mandatory pre-write dedup search and a MILLS_RUNBOOK.md index link — the two steps whose absence let five near-identical triage runbooks accumulate in five days.",
		Version:     "0.2",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "topic", Type: "string", Required: true, Description: "What the runbook covers.", Example: "ClickHouse merge failures during eval ingest"},
			{Name: "doc_slug", Type: "string", Required: true, Description: "Kebab-case filename slug (also the changelog fragment slug).", Example: "clickhouse-merge-failures"},
			{Name: "shape", Type: "enum", Enum: []string{"signature-major", "phase-major"}, Default: "signature-major", Description: "signature-major: one ## per error signature with the fixed five-### block. phase-major: First Response → Classification table → named failure modes → Closeout Note."},
			{Name: "signatures", Type: "list", Description: "For signature-major: [{name, detection, owner, safe_actions, verification}]. Each becomes one ## section.", Example: `[{"name":"Code: 432 rate limit","detection":"loki: …","owner":"LiteLLM owner","safe_actions":"…","verification":"…"}]`},
			{Name: "related_docs", Type: "list", Description: "Sibling docs to cite in prose / the References list.", Example: `["docs/mills-escalation-and-dependency-failures.md"]`},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "dedup_first", Value: "BEFORE writing, search docs/ and docs/runbooks/ for runbooks covering overlapping signatures; if an existing doc covers half or more of the topic, EXTEND it instead of creating a sibling; record the search and the docs considered in the MR description (the five-duplicate incident is the reason this axis exists)"},
			{Axis: "indexing", Value: "the new doc MUST be linked from docs/MILLS_RUNBOOK.md's see-also section in the same MR (create a '## See also' section immediately before '## Sources' when none exists) — peer prose links alone are how duplicates went undiscovered"},
			{Axis: "opening", Value: "exactly one H1 in Title Case, then an unlabeled orientation paragraph beginning 'Use this runbook when …' stating scope and the fail-closed/classification stance; no front-matter, badges, dates, or owner headers"},
			{Axis: "structure", Value: "signature-major: one ## per signature, each repeating verbatim the five-### sequence Detection signals → External-dependency classification criteria → Remediation owner and escalation → Safe operator actions → Recovery verification, closed by a single '## Incident closure'. phase-major: ## First Response → ## Classification (table with header | Signal | Disposition | Operator action |) → named failure-mode sections → ## Closeout Note"},
			{Axis: "boundary_rule", Value: "state explicitly: a branch-owned change to the failing surface is a repository defect, never an external incident"},
			{Axis: "commands", Value: "every fenced block carries a language tag (bash/sh/json/text/yaml); kubectl commands are always namespaced, read-only, and time-bounded (--since=30m); psql has no --since — bound it with a preceding SET statement_timeout in the same session and keep queries read-only; placeholders use <angle-brackets>; hand-verify escape correctness of shell/psql meta-commands — nothing in CI lints fenced blocks (the '\\du' incident)"},
			{Axis: "redaction", Value: "never include passwords, DSNs, API keys, key lengths, or Secret values — not even placeholders that invite pasting them"},
			{Axis: "operator_actions", Value: "escalation steps, safe operator actions, and recovery verification are numbered lists, never prose (detection signals and classification criteria stay descriptive); the closeout/evidence template is a ```text block of 'Key: <value>' lines with pipe-separated enums"},
			{Axis: "format", Value: "wrap at 80 columns (table rows exempt); target length 40-200 lines; imperative voice with explicit negative rules ('Do not …')"},
			{Axis: "delivery", Value: "docs-only MRs pass the docs guardrail unconditionally; this card still adds a changelog fragment; commit subject 'docs: <lowercase imperative>'"},
		},
		Gauge: &PatternGauge{
			Description: "Structural lint of the produced doc plus the changelog gate; no prose linter exists in CI, so the gauge is the only structural check.",
			Commands:    []string{"go run ./scripts/changelog --check"},
			Assertions: []string{
				"docs/runbooks/<doc_slug>.md exists with exactly one H1 and an opener paragraph beginning 'Use this runbook'",
				"every fenced code block carries a language tag",
				"no line exceeds 80 columns except markdown table rows",
				"docs/MILLS_RUNBOOK.md links the new doc",
				"the MR description records the dedup search and names the overlapping docs considered",
				"signature-major docs repeat the five-### block verbatim per signature and end with '## Incident closure'",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "author {{doc_slug}} runbook",
				Goal:               "Write the operator runbook for {{topic}} at docs/runbooks/{{doc_slug}}.md in the {{shape}} shape, covering: {{signatures}}. FIRST run the dedup search pinned by the pattern; link the doc from docs/MILLS_RUNBOOK.md; cite {{related_docs}}. Satisfy every pin and the gauge.",
				Files:              []string{"docs/runbooks/{{doc_slug}}.md", "docs/MILLS_RUNBOOK.md", "changelog.d/{{doc_slug}}.added.md"},
				AcceptanceCriteria: "Gauge passes 100%; the doc follows the pinned structure; the dedup search is recorded; no secrets or unbounded commands appear.",
				Engrams:            engramURIs(builtinLoomRunbookEngrams()),
			},
		},
		DeployContract: "merged MR + the doc reachable from docs/MILLS_RUNBOOK.md's see-also",
		Engrams:        append(engramURIs(builtinLoomRunbookEngrams()), engramURIs(builtinLoomCIEngrams())...),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-08-01",
			Notes:      "Mined from observed demand 2026-08-01 (docs/FACTORY_MODEL.md §5). Kill-test PASSED same day: context-free follower produced a structurally perfect 151-line signature-major runbook (independent gauge 8/8: one H1, opener, verbatim five-### blocks, 0 over-80 lines, indexed, dedup search recorded over 8 candidate docs), zero architecture-class gaps. v0.2 pins the boundary gaps it surfaced: psql bounding via statement_timeout (no --since flag), see-also section creation, which sections are numbered.",
		},
		Tags: []string{"docs", "runbook", "mills", "brownfield", "dobby-card"},
	}
}

// millsMetricPattern is pattern-mills-metric v0.1: the loom-core half of the
// metric+alert+dashboard class. The gitops half (PrometheusRule + dashboard
// row) is NOT stampable — platform/gitops is outside the Mills repo registry
// and protected-path-listed — so the card emits it as a documented follow-up
// block, matching the repo's own ordering convention (absent-series alert
// guards until the operator image rolls).
func millsMetricPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-mills-metric",
		Slug:        "mills-metric",
		Name:        "Mills Prometheus metric",
		Makes:       "bounded-label Prometheus metric + call site + tests + docs",
		Description: "A new mills_* metric declared in pkg/mills/metrics.go with a closed label taxonomy, its call site, mandatory rows in all three metrics_test.go tables (26 existing metrics have drifted out of them — this card makes the update non-optional), a docs/MILLS.md KPI row, and a generated gitops follow-up block for the alert + dashboard half that Mills cannot execute.",
		Version:     "0.2",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "metric_name", Type: "string", Required: true, Description: "Full metric name: mills_<subsystem>_<unit>; counters end _total, histograms _seconds, gauges bare.", Example: "mills_takeup_pattern_harvests_total"},
			{Name: "metric_slug", Type: "string", Required: true, Description: "Kebab slug for the changelog fragment.", Example: "takeup-pattern-harvests-metric"},
			{Name: "collector", Type: "enum", Required: true, Enum: []string{"counter", "counter_vec", "gauge", "gauge_vec", "histogram"}, Description: "promauto collector kind."},
			{Name: "labels", Type: "list", Description: "For *_vec: [{name, values:[{value, when}]}] — every label value set must be CLOSED, and every value must name the call-site event that emits it (kill-test gap: an unmapped value forces the implementer to invent the mapping).", Example: `[{"name":"outcome","values":[{"value":"recorded","when":"instance recorded against the taste gate"},{"value":"error","when":"record call failed"}]}]`},
			{Name: "help", Type: "string", Required: true, Description: "One sentence, sentence case, ends with a period, enumerates label values in parentheses.", Example: "Pattern taste-gate harvests from merged stamped plans, by outcome (recorded/unmatched/error)."},
			{Name: "call_site", Type: "string", Required: true, Description: "Repo-relative file where the Inc/Observe/Set lands.", Example: "pkg/mills/takeup/reconciler.go"},
			{Name: "rationale", Type: "string", Required: true, Description: "The incident/question motivating the metric — becomes the Go doc comment's why.", Example: "the 2026-08-01 withheld-notes storm was invisible without a per-outcome counter"},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "go", Kind: "toolchain", Required: true, Check: "go version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "registration", Value: "promauto.New* into the DEFAULT registry only, as a package-level var in pkg/mills/metrics.go under the subsystem's `----- <Subsystem> metrics -----` banner comment (append a new banner section at the end of the file when the subsystem has none); never MustRegister, never a custom registerer, never a declaration outside metrics.go"},
			{Axis: "naming", Value: "mills_<subsystem>_<unit>; counters end _total, histograms _seconds, gauges bare"},
			{Axis: "labels", Value: "every label is a closed taxonomy; the doc comment reasons about cardinality; label values appear at call sites only from the enumerated set"},
			{Axis: "help", Value: "one sentence, sentence case, ends with a period, enumerates label values in parentheses — TestGatherableExposesMillsMetrics enforces non-empty Help for every mills_* metric"},
			{Axis: "doc_comment", Value: "the Go doc comment carries the WHY: the motivating incident and what a flat vs rising series means"},
			{Axis: "buckets", Value: "histogram buckets are hand-tuned per subsystem (sub-second 0.001…5, tick loops 0.1…120, stage wall-clock 1…7200); never prometheus.DefBuckets"},
			{Axis: "call_sites", Value: "metrics.go declares only; Inc/Observe/Set calls live in the owning package"},
			{Axis: "tests", Value: "ALL THREE metrics_test.go tables gain a row: TestMetricsRegistered (nil check), TestCounterVecLabelsAccept (label arity with canonical values), TestGatherableExposesMillsMetrics (name in want map). No exceptions — the existing 26-metric drift is the failure mode"},
			{Axis: "serving", Value: "/metrics on :9090 via promhttp exposes the default registry automatically — touch NO server plumbing"},
			{Axis: "docs", Value: "docs/MILLS.md 'KPIs the dashboards track' table gains a row: | metric{labels} | what it answers |"},
			{Axis: "gitops_followup", Value: "the alert (k3s/mills/prometheus-alerts.yaml or k3s/monitoring/prometheus-rules-mills.yaml) and dashboard row (services-loom-mills-dashboard.yaml, JSON version bump) are OUT of this MR — platform/gitops is protected and not Mills-executable. Emit a follow-up block in the MR description with the drafted alert name/expr/for/severity, the absent-series guard (`X == 0 and Y > 0`) so it stays inert until the image rolls, and the dashboard row-id + version-bump instruction"},
			{Axis: "delivery", Value: "changelog.d/<metric_slug>.added.md fragment; never edit CHANGELOG.md"},
		},
		Gauge: &PatternGauge{
			Description: "The three metric test tables plus the changelog gate; deterministic, no cluster required.",
			Commands: []string{
				"go build ./pkg/mills/...",
				"go test ./pkg/mills -run 'TestMetricsRegistered|TestCounterVecLabelsAccept|TestGatherableExposesMillsMetrics'",
				"go run ./scripts/changelog --check",
			},
			Assertions: []string{
				"the metric appears in all three metrics_test.go tables",
				"Help ends with a period and enumerates every label value",
				"the call site uses only enumerated label values",
				"the MR description contains the gitops follow-up block with an absent-series-guarded alert draft",
				"docs/MILLS.md KPI table has the new row",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "add {{metric_name}}",
				Goal:               "Declare {{metric_name}} ({{collector}}, labels: {{labels}}) in pkg/mills/metrics.go with Help '{{help}}' and a doc comment explaining: {{rationale}}. Wire the call site in {{call_site}}. Extend all three metrics_test.go tables. Add the docs/MILLS.md KPI row and the changelog fragment. Emit the gitops follow-up block in the MR description. Satisfy every pin and the gauge.",
				Files:              []string{"pkg/mills/metrics.go", "pkg/mills/metrics_test.go", "{{call_site}}", "docs/MILLS.md", "changelog.d/{{metric_slug}}.added.md"},
				AcceptanceCriteria: "Gauge passes 100%; all three test tables extended; label space closed; gitops follow-up block present in the MR description.",
				Engrams:            engramURIs(builtinLoomObsEngrams()),
			},
		},
		DeployContract: "merged MR + green CI; the metric appears at :9090/metrics on the next operator roll; gitops follow-up lands separately after the roll",
		Engrams:        append(engramURIs(builtinLoomObsEngrams()), engramURIs(builtinLoomCIEngrams())...),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-08-01",
			Notes:      "Mined from observed demand 2026-08-01 (docs/FACTORY_MODEL.md §5). Kill-test PASSED same day: context-free follower landed the metric + call sites + all three test tables + docs row + absent-series-guarded gitops follow-up block, gauge green, diff exactly the declared files. v0.2 pins the gaps it surfaced: label values must name their triggering call-site event; banner-section creation when the subsystem has none.",
		},
		Tags: []string{"go", "observability", "prometheus", "mills", "brownfield", "dobby-card"},
	}
}

// operatorReadEndpointPattern is pattern-operator-read-endpoint v0.1: a
// read-only GET on the loom-mills-operator, end-to-end through the HUD proxy.
// Mined from handlers_pipeline.go / handlers_overseers.go and the proxy-route
// comments; the []-never-null and SPA-fallback pins encode this repo's two
// most-repeated endpoint regressions.
func operatorReadEndpointPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-operator-read-endpoint",
		Slug:        "operator-read-endpoint",
		Name:        "Operator read-only endpoint",
		Makes:       "GET /api/mills/* endpoint (operator handler + HUD proxy, end-to-end)",
		Description: "A read-only operator GET serving store data: bare-registered handler with writeJSON/[]-never-null discipline, handler tests driven through httpMux, the mandatory HUD proxy route + test, optional DAO query, docs table row, and changelog fragment.",
		Version:     "0.2",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "topic", Type: "string", Required: true, Description: "Handler-file topic: handlers_<topic>.go (reuse an existing topic file when one fits).", Example: "escalations"},
			{Name: "route_path", Type: "string", Required: true, Description: "Path under /api/mills/.", Example: "escalations/relaunch-candidates"},
			{Name: "endpoint_slug", Type: "string", Required: true, Description: "Kebab slug for the changelog fragment.", Example: "relaunch-candidates-endpoint"},
			{Name: "projection", Type: "string", Required: true, Description: "What the endpoint returns: the DAO call (existing or new), the projected fields, AND which timestamp the `since` param windows (kill-test gap: an unnamed since-dimension forces a guess).", Example: "escalated items whose latest run has EscalationRetryable=true, projected as {ID, Title, EscalationClass, EndedAt}; since windows the latest run's ended_at"},
			{Name: "response_shape", Type: "enum", Enum: []string{"bare-array", "envelope"}, Default: "bare-array", Description: "bare-array for a single collection; envelope ({\"x\": …, \"y\": …}) for composites a drawer renders in one request."},
			{Name: "limit_default", Type: "string", Default: "50", Description: "Default page size const."},
			{Name: "limit_max", Type: "string", Default: "200", Description: "Max page size const."},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "go", Kind: "toolchain", Required: true, Check: "go version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "auth", Value: "GET routes register BARE in httpMux — admit() gates new work, operate() gates control-while-closed, requireAdmin() gates naked admin mutations; none of the three ever wraps a read. Each new route line carries a comment naming why it is an open read"},
			{Axis: "encoding", Value: "success via writeJSON(w, http.StatusOK, body) from handlers_helpers.go; failures via plain-text http.Error: 400 for malformed query/path params, 404 via errors.Is(err, store.ErrNotFound), 500 with err.Error()"},
			{Axis: "empty_lists", Value: "[] never null: nil-check EVERY slice before encoding (if xs == nil { xs = []T{} }), including nested ones, plus a regression test asserting the JSON literal is [] — this is the repo's hardest-won endpoint pin"},
			{Axis: "dto", Value: "when the projection matches a store row's fields, serialize a tagless store row directly (PascalCase, no mapping layer); an unexported DTO above the handler exists ONLY for computed fields the store row lacks — the consumer mirrors the ACTUAL serialization either way"},
			{Axis: "pagination", Value: "limit + since query params only, no cursors; per-file <topic>DefaultLimit/<topic>MaxLimit consts; an over-max limit CAPS to the max (parseLimit precedent) while a malformed or non-positive limit 400s; since is RFC3339 with a documented default window"},
			{Axis: "hud_proxy", Value: "an explicit mux.HandleFunc(\"GET /api/mills/<path>\", mw(d.handleProxyGet)) in internal/hud/domain/mills/mills.go is MANDATORY — a missing route silently serves the SPA HTML fallback and the frontend dies on JSON.parse; plus a proxy_test.go case asserting the upstream path (and RawQuery when parameterized) is preserved and X-Loom-Admin-Token is stripped"},
			{Axis: "testing", Value: "handler tests drive op.httpMux().ServeHTTP with the newTestOperator(t) fixture — never the handler func directly — and include a read-only assertion (POST/PUT/DELETE on the route return non-200)"},
			{Axis: "resilience", Value: "serve from the canonical store even when optional subsystems (dispatcher, reconcilers) are unwired: guard nils, log-and-continue on partial reads — the HUD never sees a 503 on a poll"},
			{Axis: "dao", Value: "when a new query is needed: prefer hanging it on the topic's EXISTING DAO struct (a brand-new DAO struct requires store.go wiring, which is outside the declared file set) in pkg/mills/store/dao_<topic>.go with a shared column const, Get→ErrNotFound / ListBy<Dimension>(ctx, …, limit) naming, errors wrapped '<topic> <op>: %w', times via timeRFC3339/parseTime, ordering documented in the doc comment; a new table is a numbered migration; tests use newTestStore(t). Declared DAO files may go untouched when an existing query suffices"},
			{Axis: "docs", Value: "docs/MILLS.md 'REST + MCP surface' table gains the route row"},
			{Axis: "delivery", Value: "changelog.d/<endpoint_slug>.added.md fragment; never edit CHANGELOG.md"},
		},
		Gauge: &PatternGauge{
			Description: "Build + the three test packages the endpoint touches; deterministic, no cluster.",
			Commands: []string{
				"go build ./...",
				"go test ./cmd/loom-mills-operator/ ./internal/hud/domain/mills/ ./pkg/mills/store/",
				"go run ./scripts/changelog --check",
			},
			Assertions: []string{
				"GET returns 200 with the declared shape and [] (never null) when empty",
				"POST/PUT/DELETE on the route return non-200",
				"the HUD proxy forwards the exact path (and query) and strips X-Loom-Admin-Token",
				"the route appears in docs/MILLS.md's surface table",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "add GET /api/mills/{{route_path}}",
				Goal:               "Serve {{projection}} as a {{response_shape}} at GET /api/mills/{{route_path}}: handler in cmd/loom-mills-operator/handlers_{{topic}}.go, bare route registration in server.go, HUD proxy route + test, limit default {{limit_default}} / max {{limit_max}}, docs row, changelog fragment. Use an existing DAO query when one fits; the declared dao_{{topic}}.go files may go untouched. Satisfy every pin and the gauge.",
				Files:              []string{"cmd/loom-mills-operator/handlers_{{topic}}.go", "cmd/loom-mills-operator/handlers_{{topic}}_test.go", "cmd/loom-mills-operator/server.go", "internal/hud/domain/mills/mills.go", "internal/hud/domain/mills/proxy_test.go", "pkg/mills/store/dao_{{topic}}.go", "pkg/mills/store/dao_{{topic}}_test.go", "docs/MILLS.md", "changelog.d/{{endpoint_slug}}.added.md"},
				AcceptanceCriteria: "Gauge passes 100%; []-never-null regression test present; proxy test present; read-only test present.",
				Engrams:            engramURIs(builtinLoomOpsEngrams()),
			},
		},
		DeployContract: "merged MR + green CI; the route serves via the HUD proxy on the next operator+HUD roll",
		Engrams:        append(engramURIs(builtinLoomOpsEngrams()), engramURIs(builtinLoomCIEngrams())...),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-08-01",
			Notes:      "Mined from observed demand 2026-08-01 (docs/FACTORY_MODEL.md §5). Kill-test PASSED same day ON THE REAL relaunch-candidates demand item: context-free follower delivered DAO query + handler + bare route + proxy route + 9 tests (incl. []-literal regression + read-only sweep), all packages green, diff exactly the declared files — a shippable diff, mirroring how !874 earned the go-rest approval. v0.2 pins the gaps it surfaced: since-dimension named in projection; over-max limit caps; tagless-store-row precedence; extend-existing-DAO preference.",
		},
		Tags: []string{"go", "rest", "operator", "hud", "mills", "brownfield", "dobby-card"},
	}
}

// hudPanelPattern is pattern-hud-panel v0.1: a full new HUD panel wired
// through panelRegistry + router + store + proxy. Mined from the Overseers
// panel commit (8 files) and the mill-floor spec's ten house rules; strips
// inside an existing panel are deliberately NOT this card (they're small
// enough to ride a plain slice).
func hudPanelPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-hud-panel",
		Slug:        "hud-panel",
		Name:        "HUD panel",
		Makes:       "HUD panel (Svelte 5 + registry + router + polling store + proxy route)",
		Description: "A new lazily-loaded HUD panel: panelRegistry thunk + router sub-view, a rune-store polling a /api/mills read through createPoller, PanelShell state gating, token-driven styling, colocated tests, and the mandatory Go proxy route + test.",
		Version:     "0.2",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "panel_name", Type: "string", Required: true, Description: "Component base name (PascalCase, no 'Panel' suffix).", Example: "Relaunch"},
			{Name: "panel_slug", Type: "string", Required: true, Description: "Kebab slug for the changelog fragment.", Example: "hud-relaunch-panel"},
			{Name: "subview_id", Type: "string", Required: true, Description: "Globally-unique router sub-view id (prefix mills-* when the bare id collides with a top-level view).", Example: "mills-relaunch"},
			{Name: "subview_label", Type: "string", Required: true, Description: "Tab label.", Example: "Relaunch"},
			{Name: "subview_key", Type: "string", Required: true, Description: "Single-char hotkey, unique within the view, never 'o' or 'r' (app-level globals). The stamp cannot see the router, so verify uniqueness at implementation time — on a collision take the nearest free letter and document the deviation in a router comment (kill-test precedent).", Example: "u"},
			{Name: "subview_group", Type: "string", Required: true, Description: "Router group; same-group entries must be contiguous.", Example: "Governance"},
			{Name: "data_route", Type: "string", Required: true, Description: "The /api/mills/... GET the store polls (stamp pattern-operator-read-endpoint first when it doesn't exist).", Example: "/api/mills/escalations/relaunch-candidates"},
			{Name: "store_domain", Type: "string", Required: true, Description: "Store filename fragment: mills_<store_domain>.svelte.ts.", Example: "relaunch"},
			{Name: "poll_interval_ms", Type: "string", Default: "15000", Description: "Poll cadence (Mills default 15s; archive-backed views 60s)."},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "registration", Value: "panelRegistry gains a lazy LITERAL import thunk keyed by the sub-view id (statically analyzable for Vite code-splitting); router gains {id, label, key, group}; the router parity tests (panelLoaders entry per sub-view, globally-unique ids, unique keys, no reserved o/r) must pass untouched"},
			{Axis: "runes", Value: "rune files are .svelte.ts only: store is a class + singleton export (export const mills<X>Store); clock-dependent or unit-test-worthy helpers live in plain utils/*.ts taking (data, now = new Date()) — a store-local normalise() suffices when none are needed; components use $derived for every store read, $state for local UI only, and ONE $effect owning the poller lifecycle with a cleanup return"},
			{Axis: "effect_safety", Value: "any $effect that reads AND writes the same $state wraps the read in untrack() — the alternative kills the component's entire effect graph, not just that effect"},
			{Axis: "polling", Value: "createPoller is the only polling primitive (visibility-pause, overlap guard, coalesced refresh); it NEVER fires an initial tick — call the fetch explicitly before start(); store-owned startPolling(ms)/stopPolling() shape"},
			{Axis: "fetch_boundary", Value: "getJSON maps 503→disabled (calm empty state — dev machines without an operator URL must not see red), 404→null, other non-ok→error; a normalise() defaults Go nil slices/maps (x ?? [], including inner arrays); an error NEVER blanks the last good snapshot; TS interfaces mirror the wire casing exactly (PascalCase for untagged Go structs), no mapping layer"},
			{Axis: "state_gating", Value: "the panel wraps in shared/PanelShell (loading/error/empty/emptyTone rendered mutually exclusively) and reuses shared MetricCard/Badge/DataTable/ErrorBanner — never hand-rolled state UI"},
			{Axis: "proxy", Value: "the polled route needs an explicit GET entry in internal/hud/domain/mills/mills.go + a proxy_test.go case (path preserved, X-Loom-Admin-Token stripped) — a missing route serves the SPA HTML fallback and the store dies on JSON.parse; when the route pre-exists, add only the missing proxy_test case"},
			{Axis: "styling", Value: "design tokens only (var(--space-*), var(--text-*), var(--fg-*), semantic colors; color-mix for tints, --*-rgb for canvas/SVG); verify a --mills-* alias exists before relying on it; child-component internals styled via wrapper-scoped :global(); every animation has a prefers-reduced-motion static fallback"},
			{Axis: "testing", Value: "helpers get colocated *.test.ts in the node vitest project; DOM tests only as *.dom.test.ts; the store test stubs globalThis.fetch and restores it in afterEach; zero new svelte-check warnings"},
			{Axis: "artifacts", Value: "internal/hud/frontend/dist/ is GITIGNORED — commit source only and restore dist/.gitkeep after local builds; CI rebuilds dist and downstream jobs assert dist/index.html"},
			{Axis: "size", Value: "the panel component stays under 300 lines; extract cards beyond that (docs/HUD_PANEL_DECOMP.md)"},
			{Axis: "delivery", Value: "changelog.d/<panel_slug>.added.md fragment; never edit CHANGELOG.md"},
		},
		Gauge: &PatternGauge{
			Description: "The frontend gate chain (cheapest first) plus the Go proxy tests.",
			Commands: []string{
				"pnpm --dir internal/hud/frontend check",
				"pnpm --dir internal/hud/frontend test",
				"pnpm --dir internal/hud/frontend build",
				"go test ./internal/hud/domain/mills/",
			},
			Assertions: []string{
				"router parity tests pass with the new sub-view (registry entry present, ids/keys unique)",
				"the panel renders loading/error/empty/disabled exclusively through PanelShell",
				"the store maps 503 to disabled and keeps the last good snapshot on error",
				"the proxy test asserts path preservation and admin-token stripping for the data route",
				"dist/.gitkeep survives; no dist/ content is committed",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "add {{panel_name}} panel",
				Goal:               "Build the {{panel_name}}Panel HUD view (sub-view {{subview_id}}, label {{subview_label}}, key {{subview_key}}, group {{subview_group}}) polling {{data_route}} every {{poll_interval_ms}}ms through a mills_{{store_domain}} store. Wire registry + router + proxy route + tests. Satisfy every pin and the gauge.",
				Files:              []string{"internal/hud/frontend/src/lib/components/mills/{{panel_name}}Panel.svelte", "internal/hud/frontend/src/lib/stores/mills_{{store_domain}}.svelte.ts", "internal/hud/frontend/src/lib/stores/mills_{{store_domain}}.test.ts", "internal/hud/frontend/src/lib/panelRegistry.ts", "internal/hud/frontend/src/lib/stores/router.svelte.ts", "internal/hud/domain/mills/mills.go", "internal/hud/domain/mills/proxy_test.go", "changelog.d/{{panel_slug}}.added.md"},
				AcceptanceCriteria: "Gauge passes 100%; router parity tests untouched and green; no new svelte-check warnings; PanelShell gating in place.",
				Engrams:            engramURIs(builtinLoomHUDEngrams()),
			},
		},
		DeployContract: "merged MR + green CI; the panel serves at #mills/<subview_id> on the next HUD roll",
		Engrams:        append(engramURIs(builtinLoomHUDEngrams()), engramURIs(builtinLoomCIEngrams())...),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-08-01",
			Notes:      "Mined from observed demand 2026-08-01 (docs/FACTORY_MODEL.md §5). Kill-test PASSED same day: context-free follower built panel + store + registry/router + proxy test, code-split chunk emitted, router parity 13/13, zero new svelte-check diagnostics, error-keeps-snapshot and 503→disabled proven by store tests. Its best find was a MATERIALS gap: the stamped subview_key collided with an existing hotkey — v0.2 pins the resolve-at-implementation rule the follower established.",
		},
		Tags: []string{"typescript", "svelte", "hud", "frontend", "mills", "brownfield", "dobby-card"},
	}
}
