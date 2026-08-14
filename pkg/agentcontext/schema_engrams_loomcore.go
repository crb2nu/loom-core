// schema_engrams_loomcore.go -- builtin engrams for the repo-native "dobby
// card" patterns (schema_patterns_loomcore.go).
//
// Unlike the greenfield seeds (whose file_ref proofs resolve inside a freshly
// stamped service), these proofs point at CANONICAL EXEMPLARS in loom-core
// itself — the stamped instance for a brownfield card IS this repo, so the
// same paths double as teaching material for the implementer and as
// verifiable proofs once the A2 engram-verify tail gets a reachable checkout.
package agentcontext

// builtinLoomCIEngrams are shared across every dobby card (each card's MR
// must satisfy the changelog-fragment discipline).
func builtinLoomCIEngrams() []builtinEngram {
	return []builtinEngram{
		{
			Family:   "loom-ci",
			Slug:     "changelog-fragment",
			Title:    "Per-MR changelog fragment instead of editing CHANGELOG.md",
			Problem:  "Concurrent MRs editing CHANGELOG.md's [Unreleased] section collide server-side, dropping auto-merge; the docs guardrail also fails code-only diffs.",
			Solution: "Add ONE new file changelog.d/<slug>.<category>.md (category ∈ added|changed|deprecated|removed|fixed|security) whose body is a single `- ` bullet naming the change and the touched files in backticked parens. Never edit CHANGELOG.md; fragments fold in at release. Linted by `go run ./scripts/changelog --check` in guardrails:docs-cli.",
			Proof:    "changelog.d/README.md",
			Language: "markdown",
			Scope:    "workspace",
			Tags:     []string{"pattern:dobby-cards"},
		},
	}
}

// builtinLoomRunbookEngrams back pattern-loom-runbook.
func builtinLoomRunbookEngrams() []builtinEngram {
	return []builtinEngram{
		{
			Family:   "loom-docs",
			Slug:     "runbook-structure",
			Title:    "Mills operator-runbook document structure",
			Problem:  "Operator runbooks drift into inconsistent shapes, and undiscoverable duplicates accumulate (five near-identical CI triage runbooks landed in five days).",
			Solution: "One H1 + an unlabeled 'Use this runbook when …' opener stating scope and the fail-closed stance; signature-major bodies repeat a fixed five-### block per signature (Detection signals → External-dependency classification criteria → Remediation owner and escalation → Safe operator actions → Recovery verification) closed by one '## Incident closure'; phase-major bodies use First Response → Classification (| Signal | Disposition | Operator action | table) → named failure modes → Closeout Note. Commands are fenced with language tags, namespaced, read-only, time-bounded, with <angle-bracket> placeholders.",
			Proof:    "docs/mills-incident-runbook.md",
			Language: "markdown",
			Scope:    "workspace",
			Tags:     []string{"pattern:pattern-loom-runbook"},
		},
	}
}

// builtinLoomOpsEngrams back pattern-operator-read-endpoint.
func builtinLoomOpsEngrams() []builtinEngram {
	return []builtinEngram{
		{
			Family:   "loom-ops",
			Slug:     "open-read-endpoint",
			Title:    "Open read-only operator GET endpoint",
			Problem:  "Operator read endpoints need a uniform contract (encoding, errors, pagination, auth) so HUD polls never 401, never see null-vs-[] ambiguity, and never 503 on a poll.",
			Solution: "GET routes register BARE in httpMux (admit/operate/requireAdmin are mutation wrappers only); success via writeJSON, failure via plain-text http.Error (400 malformed params, 404 on store.ErrNotFound, 500 otherwise); every slice nil-checked so empty encodes as [] never null (with a regression test asserting the literal); limit+since pagination only; the handler serves from the canonical store even when optional subsystems are unwired.",
			Proof:    "cmd/loom-mills-operator/handlers_helpers.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{"pattern:pattern-operator-read-endpoint"},
		},
		{
			Family:   "loom-ops",
			Slug:     "hud-mills-proxy-route",
			Title:    "Explicit HUD proxy route per operator read",
			Problem:  "A /api/mills/* GET with no explicit HUD proxy route silently serves the SPA HTML fallback, and the frontend fails on JSON.parse with a misleading error.",
			Solution: "Every new operator read gets an explicit mux.HandleFunc(\"GET /api/mills/<path>\", mw(d.handleProxyGet)) in internal/hud/domain/mills plus a proxy test asserting the upstream path (and RawQuery when parameterized) is preserved and X-Loom-Admin-Token is stripped.",
			Proof:    "internal/hud/domain/mills/mills.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{"pattern:pattern-operator-read-endpoint", "pattern:pattern-hud-panel"},
		},
	}
}

// builtinLoomObsEngrams back pattern-mills-metric.
func builtinLoomObsEngrams() []builtinEngram {
	return []builtinEngram{
		{
			Family:   "loom-obs",
			Slug:     "bounded-label-metric",
			Title:    "Bounded-label Prometheus metric via promauto",
			Problem:  "Ad-hoc metric labels explode cardinality, and metrics registered outside the shared declaration file drift out of the presence/label/help test tables.",
			Solution: "Declare in pkg/mills/metrics.go via promauto into the default registry, named mills_<subsystem>_<unit> (counters _total, histograms _seconds); every label is a CLOSED taxonomy enumerated in the Help sentence's parentheses; the doc comment carries the motivating incident and what flat-vs-rising means; call sites live outside the declarations file; all three metrics_test.go tables (registered, label arity, gatherable+help) gain a row.",
			Proof:    "pkg/mills/metrics.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{"pattern:pattern-mills-metric"},
		},
	}
}

// builtinLoomHUDEngrams back pattern-hud-panel.
func builtinLoomHUDEngrams() []builtinEngram {
	return []builtinEngram{
		{
			Family:   "loom-hud",
			Slug:     "registry-poller-panel",
			Title:    "HUD panel via panelRegistry + createPoller + PanelShell",
			Problem:  "New HUD panels re-decide wiring (registration, routing, polling, state gating) and re-introduce solved bugs: initial-tick gaps, hidden-tab polling, read+write $effect graph death, 503-as-error.",
			Solution: "Register a lazy literal import thunk in panelRegistry keyed by a globally-unique router sub-view id; poll ONLY via createPoller (visibility-pause, overlap guard, no initial tick — call the fetch explicitly before start()); runes live in .svelte.ts stores (class + singleton) with getJSON mapping 503→disabled and 404→null, normalise() defaulting Go nil slices, errors never blanking the last good snapshot; components wrap state in PanelShell, use $derived for store reads, and untrack() any read+write $effect.",
			Proof:    "internal/hud/frontend/src/lib/utils/poller.ts",
			Language: "typescript",
			Scope:    "workspace",
			Tags:     []string{"pattern:pattern-hud-panel"},
		},
	}
}
