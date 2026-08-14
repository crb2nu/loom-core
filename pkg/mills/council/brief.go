package council

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// BriefSources collects the input handles the brief assembler needs. The
// reviewer dispatcher (slice 3.3) and editor (slice 3.4) get a *Brief
// downstream; nothing past Compile is allowed to re-fetch from these
// sources, so a single tick of the council sees a coherent snapshot.
//
// The full spec calls for recent-merges + alerts + Loki errors fetched
// over MCP. Those land in slice 3.7 (triggers) once the council is
// orchestrated; for slice 3.2 the brief is canonical-store + filesystem
// only, which is enough for dryrun + the eval Loop A judge.
type BriefSources struct {
	// Store is the canonical SQLite-backed view. Required.
	Store *store.Store

	// RepoRoot is the absolute path to the loom-core checkout the
	// council should reference (typically the operator's mounted clone
	// or a freshly-allocated worktree). Used to read .loom/00-index.md
	// and ROADMAP.md.
	RepoRoot string

	// MaxBytes caps the rendered markdown size. Zero falls back to
	// briefDefaultMaxBytes (~16k chars ≈ 4k tokens). Sections are
	// rendered in priority order; later sections truncate first.
	MaxBytes int

	// Now is injectable for deterministic test snapshots. Defaults to
	// time.Now.
	Now func() time.Time

	// Signals, when set, supplies recent workspace pain (Loki error
	// clusters) so the council proposes items grounded in real failures
	// instead of synthetic canaries (W3.1 of .loom/126). Optional: a nil
	// source, an error, or an empty result renders an explicit unavailable
	// note so absence of the section cannot be mistaken for a clean workspace.
	Signals WorkspaceSignalSource

	// SignalWindow is how far back Signals reaches. Zero falls back to
	// workspaceSignalDefaultWindow (24h).
	SignalWindow time.Duration

	// FactoryExhaust, when set, supplies the mill's own open self-maintenance
	// issues (quarantined flaky tests, audit-advisory digests) so unattended
	// shifts have machine-filed demand to draw on instead of idling. Optional:
	// a nil source omits the section entirely — that is the policy-off shape,
	// and it is the caller's job to leave this nil when policy disables the
	// source. A source ERROR does not omit the section; see
	// FactoryExhaustUnavailableBody.
	FactoryExhaust FactoryExhaustSource

	// FactoryExhaustLookback is how far back FactoryExhaust reaches. Zero
	// falls back to defaultFactoryExhaustLookback (14d).
	FactoryExhaustLookback time.Duration

	// FactoryExhaustLimit caps how many exhaust items reach the brief. Zero
	// falls back to defaultFactoryExhaustLimit (10).
	FactoryExhaustLimit int

	// ExternalIncidentVerdict is the structured threshold result produced by
	// the external-dependency incident guardrail. When present, Compile puts a
	// prominent deterministic banner first in the brief. Nil means the
	// threshold was not evaluated and omits the banner.
	ExternalIncidentVerdict *PolicyVerdict
}

// WorkspaceSignal is one clustered real-world failure surfaced into the council
// brief — e.g. "logging/auth-service: 142 error lines, sample: ...".
type WorkspaceSignal struct {
	Source             string          // origin of the signal, e.g. "loki"
	Service            string          // namespace/app (or job) the signal is attributed to
	Count              int             // occurrences in the window
	Sample             string          // a representative line, truncated
	IncidentClass      CIIncidentClass // canonical incident class, when deterministically recognized
	ExternalDependency string          // external dependency name for external_dependency_incident signals
}

// WorkspaceSignalSource fetches recent workspace signals for the brief. The
// Loki client (pkg/mills/clients) implements it; tests use a stub. Declared on
// the consumer side so the brief stays decoupled from the concrete client.
type WorkspaceSignalSource interface {
	RecentErrorClusters(ctx context.Context, since time.Time) ([]WorkspaceSignal, error)
}

// workspaceSignalDefaultWindow is how far back the brief reaches for signals.
const workspaceSignalDefaultWindow = 24 * time.Hour

// workspaceSignalMaxClusters is the maximum number of clusters admitted to a
// brief, even when a source ignores its own limit. It matches the production
// Loki (8) plus GitLab CI (6) adapter caps.
const workspaceSignalMaxClusters = 14

// WorkspaceSignalsUnavailableBody is rendered whenever workspace signals
// cannot be fetched or no source reports a cluster. Fetch failures stay
// deliberately indistinguishable from an empty result: neither is evidence
// that the workspace is clean.
const WorkspaceSignalsUnavailableBody = "_(workspace signals unavailable or empty this tick — no recent Loki/CI cluster was retrieved; this is not evidence that the workspace is clean)_"

// WorkspaceSignalsPartialBody makes a degraded fetch visible while retaining
// clusters returned by healthy sources.
const WorkspaceSignalsPartialBody = "_(workspace signals partially unavailable this tick — the clusters below came from the sources that responded)_"

// briefDefaultMaxBytes is roughly 4k tokens at 4 chars/token. Generous
// for a planning brief; we'll tighten if reviewers consistently hit it.
const briefDefaultMaxBytes = 16 * 1024

// Brief is the assembled prompt header. Markdown is what the editor +
// reviewers consume verbatim; Sections is the structured projection
// the eval Loop A judge can reference without re-parsing the markdown.
type Brief struct {
	Markdown string
	Sections []BriefSection

	// ClassifiedCIFailures is the structured projection of the same recent
	// classified ci_watch failures rendered into Markdown. API summaries can
	// consume this directly instead of scraping the brief text.
	ClassifiedCIFailures []*store.ClassifiedCIFailureSummary

	// IntentsMissing is true when preflightRoadmapIntents found the canonical
	// roadmap-intent store empty. The same condition stamps IntentsMissingMarker
	// into Markdown; this is the structured projection the council runner gates
	// on before it spends anything on reviewers or the editor.
	IntentsMissing bool

	// SourceCounts records what made it into the brief and what was
	// truncated. Surfaced into the council_runs sidecar for forensics.
	SourceCounts BriefSourceCounts
}

// BriefSection is one labelled chunk in the rendered brief. Order in the
// slice matches order in Markdown.
type BriefSection struct {
	Heading string
	Body    string
}

// BriefSourceCounts is the audit footprint of one Compile call.
type BriefSourceCounts struct {
	Intents              int  `json:"intents"`
	IntentsMissing       bool `json:"intents_missing"` // canonical roadmap-intent store was empty at compile time
	BacklogQueued        int  `json:"backlog_queued"`
	BacklogActive        int  `json:"backlog_active"`
	KPISnapshot          bool `json:"kpi_snapshot"`
	IndexBytes           int  `json:"index_bytes"`
	CrossRunFindings     int  `json:"cross_run_findings"`     // number of Loop C eval rows surfaced
	WorkspaceSignals     int  `json:"workspace_signals"`      // number of Loki/CI error clusters surfaced (W3.1)
	ClassifiedCIFailures int  `json:"classified_ci_failures"` // recent classified ci_watch escalations surfaced
	// FactoryExhaustItems is how many open self-maintenance issues reached the
	// brief; FactoryExhaustUnavailable records that the source was configured
	// but errored, which is a different fact from "zero items".
	FactoryExhaustItems       int  `json:"factory_exhaust_items"`
	FactoryExhaustUnavailable bool `json:"factory_exhaust_unavailable"`
	TruncatedTo               int  `json:"truncated_to"` // final byte length after truncation, equal to MaxBytes if we hit the cap
}

// crossRunBriefWindow is how far back Compile reaches for Loop C
// findings to surface in the next council brief. Matches the cross-run
// scheduler's weekly cadence with a small grace overlap so a brief
// compiled Sunday afternoon still sees that morning's findings.
const crossRunBriefWindow = 8 * 24 * time.Hour

// Compile assembles the brief from the configured sources. Sections are
// rendered in priority order:
//  1. Roadmap intents (machine-shaped goals)
//  2. Recent KPIs (most recent 1d snapshot if any)
//  3. Backlog snapshot (queued + active counts; titles for queued)
//  4. .loom/00-index.md excerpt (operator-curated planning thread)
//
// Trailing sections truncate first if MaxBytes is hit, so a tight cap
// preserves the structured signals over the prose tail.
func Compile(ctx context.Context, src BriefSources) (*Brief, error) {
	if src.Store == nil {
		return nil, fmt.Errorf("council: brief requires a Store")
	}
	now := time.Now().UTC()
	if src.Now != nil {
		now = src.Now().UTC()
	}
	maxBytes := src.MaxBytes
	if maxBytes <= 0 {
		maxBytes = briefDefaultMaxBytes
	}

	b := &Brief{}

	if src.ExternalIncidentVerdict != nil {
		b.Sections = append(b.Sections, RenderExternalIncidentBanner(*src.ExternalIncidentVerdict))
	}

	intents, intentsMissing, err := preflightRoadmapIntents(ctx, src.Store.Roadmap)
	if err != nil {
		return nil, err
	}
	intentsBody := renderIntents(intents)
	if intentsMissing {
		intentsBody = IntentsMissingMarker + "\n" + intentsBody
		// Mark the brief itself, not just its Markdown. The runner gates
		// scheduling on this field, so the mark and the block are one
		// decision about one object.
		b.IntentsMissing = true
		b.SourceCounts.IntentsMissing = true
	}
	b.Sections = append(b.Sections, BriefSection{
		Heading: "Roadmap intents",
		Body:    intentsBody,
	})
	b.SourceCounts.Intents = len(intents)

	if snap, err := src.Store.KPI.Latest(ctx, 86400); err == nil {
		b.Sections = append(b.Sections, BriefSection{
			Heading: "Mills KPIs (last 24h snapshot)",
			Body:    renderKPI(snap),
		})
		b.SourceCounts.KPISnapshot = true
	}

	queued, _ := src.Store.Backlog.ListByState(ctx, store.BacklogQueued)
	active, _ := src.Store.Pipeline.CountActive(ctx)
	b.Sections = append(b.Sections, BriefSection{
		Heading: "Backlog snapshot",
		Body:    renderBacklog(queued, active),
	})
	b.SourceCounts.BacklogQueued = len(queued)
	b.SourceCounts.BacklogActive = active

	incidents, err := src.Store.Incidents.ListAggregated(ctx)
	if err != nil {
		return nil, fmt.Errorf("council: list persisted incidents: %w", err)
	}
	b.Sections = append(b.Sections, BriefSection{
		Heading: "Persisted incidents",
		Body:    renderPersistedIncidents(incidents),
	})

	// Workspace signals (W3.1): recent real failures so the council proposes
	// grounded work. Best-effort — source errors and empty results render an
	// explicit absence note; they never fail the brief.
	workspaceSignals := BriefSection{
		Heading: "Workspace signals (recent errors)",
		Body:    WorkspaceSignalsUnavailableBody,
	}
	if src.Signals != nil {
		window := src.SignalWindow
		if window <= 0 {
			window = workspaceSignalDefaultWindow
		}
		if sigs, serr := src.Signals.RecentErrorClusters(ctx, now.Add(-window)); len(sigs) > 0 {
			if len(sigs) > workspaceSignalMaxClusters {
				sigs = sigs[:workspaceSignalMaxClusters]
			}
			sigs = ClassifyExternalWorkspaceSignals(sigs)
			workspaceSignals.Body = renderSignals(sigs, window)
			if serr != nil {
				workspaceSignals.Body = WorkspaceSignalsPartialBody + "\n\n" + workspaceSignals.Body
			}
			b.SourceCounts.WorkspaceSignals = len(sigs)
		}
	}
	b.Sections = append(b.Sections, workspaceSignals)

	if failures, ferr := src.Store.Pipeline.ListRecentClassifiedCIFailures(ctx, now.Add(-workspaceSignalDefaultWindow), 10); ferr == nil && len(failures) > 0 {
		b.ClassifiedCIFailures = append([]*store.ClassifiedCIFailureSummary(nil), failures...)
		b.Sections = append(b.Sections, BriefSection{
			Heading: "Classified CI failures (last 24h)",
			Body:    renderClassifiedCIFailures(failures),
		})
		if body := RenderIncidentPlanningContext(IncidentContextsFromClassifiedCIFailures(failures)); body != "" {
			b.Sections = append(b.Sections, BriefSection{
				Heading: "Incident classification planning context",
				Body:    body,
			})
		}
		b.SourceCounts.ClassifiedCIFailures = len(failures)
	}

	// Factory exhaust: the mill's own machine-filed maintenance demand. Placed
	// above the .loom prose tail because it is structured demand — under a
	// tight MaxBytes we would rather drop the planning-index excerpt than the
	// list of open flakes. Never fails the brief: a source error renders the
	// section as explicitly unavailable rather than omitting it, because an
	// omitted section reads as "the factory is clean".
	if src.FactoryExhaust != nil {
		window := src.FactoryExhaustLookback
		if window <= 0 {
			window = defaultFactoryExhaustLookback
		}
		limit := src.FactoryExhaustLimit
		if limit <= 0 {
			limit = defaultFactoryExhaustLimit
		}
		items, xerr := src.FactoryExhaust.ListFactoryExhaust(ctx, now.Add(-window), limit)
		switch {
		case xerr != nil:
			mills.CouncilFactoryExhaustErrorsTotal.Inc()
			b.Sections = append(b.Sections, BriefSection{
				Heading: FactoryExhaustHeading,
				Body:    FactoryExhaustUnavailableBody,
			})
			b.SourceCounts.FactoryExhaustUnavailable = true
		case len(items) > 0:
			// Bound here as well as at the source: the interface documents the
			// cap, but the brief is the thing with a byte budget, so it does
			// not trust a client that over-returns.
			SortFactoryExhaust(items)
			if len(items) > limit {
				items = items[:limit]
			}
			for _, it := range items {
				mills.CouncilFactoryExhaustItemsTotal.WithLabelValues(string(it.Kind)).Inc()
			}
			b.Sections = append(b.Sections, BriefSection{
				Heading: FactoryExhaustHeading,
				Body:    renderFactoryExhaust(items, window, limit),
			})
			b.SourceCounts.FactoryExhaustItems = len(items)
		}
	}

	if src.RepoRoot != "" {
		if body, n := readBriefFile(filepath.Join(src.RepoRoot, ".loom/00-index.md"), 8*1024); body != "" {
			b.Sections = append(b.Sections, BriefSection{
				Heading: ".loom/00-index.md (active planning thread)",
				Body:    body,
			})
			b.SourceCounts.IndexBytes = n
		}
	}

	// Loop C — surface the most recent cross-run findings so the council
	// can react to flaky gates / stale plans / divergent outcomes
	// without the operator humans having to query the eval table by hand.
	// We fetch the whole crossRunBriefWindow worth and let the renderer
	// drop the section if there are no findings.
	scores, _ := src.Store.Eval.ListSince(ctx, now.Add(-crossRunBriefWindow), 50)
	crossRunFindings := filterCrossRun(scores)
	if len(crossRunFindings) > 0 {
		b.Sections = append(b.Sections, BriefSection{
			Heading: "Cross-run findings (Loop C, last 7 days)",
			Body:    renderCrossRun(crossRunFindings),
		})
		b.SourceCounts.CrossRunFindings = len(crossRunFindings)
	}

	b.Markdown = renderMarkdown(now, b.Sections, maxBytes)
	b.SourceCounts.TruncatedTo = len(b.Markdown)
	return b, nil
}

// filterCrossRun selects only Loop C eval scores from a mixed result.
func filterCrossRun(scores []*store.EvalScore) []*store.EvalScore {
	out := make([]*store.EvalScore, 0, len(scores))
	for _, s := range scores {
		if s.SubjectKind == store.EvalSubjectCrossRun {
			out = append(out, s)
		}
	}
	return out
}

// renderCrossRun formats Loop C findings. Score < 1.0 means the rubric
// flagged something — we surface the rubric, score, and notes line so
// the council brief reads at a glance.
func renderCrossRun(scores []*store.EvalScore) string {
	var b strings.Builder
	for _, s := range scores {
		notes := s.Notes
		if notes == "" {
			notes = "_(no details)_"
		}
		fmt.Fprintf(&b, "- **%s** (score `%.2f`, %s): %s\n",
			s.Rubric, s.Score, s.SubjectID, notes)
	}
	return b.String()
}

// ----- renderers -----

func renderIntents(intents []*store.RoadmapIntent) string {
	if len(intents) == 0 {
		return "_(no roadmap intents in canonical store; run the extractor first)_"
	}
	var b strings.Builder
	for _, i := range intents {
		fmt.Fprintf(&b, "- **P%d %s** — %s\n", i.Priority, i.Theme, i.Summary)
	}
	return b.String()
}

func renderKPI(snap *store.KPISnapshot) string {
	if snap == nil {
		return "_(no KPI snapshot yet)_"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "_Snapshot at %s_\n\n", snap.SnapshotAt.Format(time.RFC3339))
	for k, v := range snap.Metrics {
		fmt.Fprintf(&b, "- `%s`: %v\n", k, v)
	}
	return b.String()
}

func renderBacklog(queued []*store.BacklogItem, active int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Queued: **%d** | Active pipeline runs: **%d**\n", len(queued), active)
	if len(queued) == 0 {
		return b.String()
	}
	b.WriteString("\nQueued items:\n")
	for _, item := range queued {
		fmt.Fprintf(&b, "- `%s` %s — %s\n", item.ID, item.Priority, item.Title)
	}
	return b.String()
}

func renderPersistedIncidents(incidents []store.IncidentSummary) string {
	if len(incidents) == 0 {
		return "_(no persisted incidents)_"
	}
	var b strings.Builder
	b.WriteString("Use persisted incident history as planning context. For class `external_dependency_incident`, do not propose outside-system remediation; emit no proposal unless the follow-up changes repository-owned guardrails, classifiers, telemetry, docs, config, retry policy, or runbooks.\n\n")
	for _, incident := range incidents {
		fmt.Fprintf(&b, "- **%s** class=`%s` disposition=`%s` occurrence_count=`%d` source=`%s` dependency=`%s` shape=`%s` retryable=`%t`\n",
			incident.Summary, incident.Class, incident.Disposition, incident.Occurrences,
			incident.Source, incident.Dependency, incident.Shape, incident.Retryable)
	}
	return b.String()
}

// renderSignals formats workspace signals (W3.1) — the top recent error
// clusters — so the council can propose grounded fixes. One line per cluster:
// service, occurrence count, source, and a representative sample.
func renderSignals(sigs []WorkspaceSignal, window time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Top error clusters in the last %s (from workspace logs — propose fixes for these over synthetic work):\n\n", window.Round(time.Hour))
	for _, s := range sigs {
		class := ""
		if s.IncidentClass != "" {
			class = fmt.Sprintf(" class=`%s`", s.IncidentClass)
		}
		dependency := ""
		if s.ExternalDependency != "" {
			dependency = fmt.Sprintf(" external=`%s`", s.ExternalDependency)
		}
		fmt.Fprintf(&b, "- **%s** — %d occurrence(s) [%s]%s%s: `%s`\n", s.Service, s.Count, s.Source, class, dependency, s.Sample)
	}
	return b.String()
}

func renderClassifiedCIFailures(failures []*store.ClassifiedCIFailureSummary) string {
	var b strings.Builder
	b.WriteString("Recent escalated `ci_watch` failures with persisted classification metadata:\n\n")
	for _, f := range failures {
		if f == nil {
			continue
		}
		class := canonicalClassifiedCIFailureClass(f)
		retryable := "unknown"
		if f.Retryable != nil {
			retryable = fmt.Sprintf("%t", *f.Retryable)
		}
		freeRetry := "unknown"
		if f.FreeRetry != nil {
			freeRetry = fmt.Sprintf("%t", *f.FreeRetry)
		}
		terminal := "unknown"
		if f.Terminal != nil {
			terminal = fmt.Sprintf("%t", *f.Terminal)
		}
		dependency := ""
		if f.ExternalDependency != "" || f.ExternalDependencyID != "" {
			dependency = fmt.Sprintf(" external=`%s`", firstNonEmptyString(f.ExternalDependency, f.ExternalDependencyID))
		}
		classifier := ""
		if f.Classifier != "" {
			classifier = fmt.Sprintf(" classifier=`%s`", f.Classifier)
		}
		title := f.BacklogTitle
		if title == "" {
			title = f.BacklogID
		}
		fmt.Fprintf(&b, "- `%s` `%s` — class=`%s` retryable=`%s` free_retry=`%s` terminal=`%s`%s%s: %s\n",
			f.RunID, f.BacklogID, class, retryable, freeRetry, terminal, classifier, dependency, title)
	}
	return b.String()
}

// canonicalClassifiedCIFailureClass maps a persisted ci_watch escalation onto
// the closed CIIncidentClass taxonomy. The store records the pipeline's own
// failure vocabulary (pipeline.ErrorClass and the store's normalized failure
// class), which is a different, coarser taxonomy — returning those raw strings
// produced classes like "code" and "infra" that no CIIncidentClass constant
// matches, so every consumer comparing against the taxonomy silently missed.
func canonicalClassifiedCIFailureClass(f *store.ClassifiedCIFailureSummary) CIIncidentClass {
	if f == nil {
		return CIIncidentUnclassified
	}
	if f.ExternalDependency != "" || f.ExternalDependencyID != "" {
		return CIIncidentExternalDependency
	}
	if class := incidentClassForFailureClass(f.FailureClass); class != "" {
		return class
	}
	if class := incidentClassForFailureClass(f.EscalationClass); class != "" {
		return class
	}
	return CIIncidentUnclassified
}

// incidentClassForFailureClass translates one failure-class spelling into the
// incident taxonomy. Both vocabularies are accepted: the store normalizes to
// "infrastructure"/"configuration" while pipeline.ErrorClass emits the short
// "infra"/"config" forms, and both reach these columns. An unrecognized value
// returns the empty class so the caller can fall through to the next source
// rather than mislabelling the incident.
func incidentClassForFailureClass(class string) CIIncidentClass {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "code":
		return CIIncidentRepositoryRegression
	case "config", "configuration":
		return CIIncidentCIConfiguration
	case "infra", "infrastructure":
		return CIIncidentRunnerInfrastructure
	case "transient", "transient_quota":
		return CIIncidentFlakeOrTransient
	default:
		return ""
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// renderMarkdown stitches sections together with H2 headings, prefixed
// with a deterministic "compiled at" timestamp so reviewers see when the
// snapshot was taken. Truncation happens at section boundaries — we'd
// rather drop a section entirely than render half of one.
func renderMarkdown(now time.Time, sections []BriefSection, maxBytes int) string {
	header := fmt.Sprintf("# Council Brief — compiled %s\n\n", now.Format(time.RFC3339))
	out := strings.Builder{}
	out.WriteString(header)
	for _, s := range sections {
		chunk := fmt.Sprintf("\n## %s\n\n%s\n", s.Heading, strings.TrimRight(s.Body, "\n"))
		if out.Len()+len(chunk) > maxBytes {
			fmt.Fprintf(&out, "\n_(brief truncated at %d bytes — section %q dropped to fit)_\n", maxBytes, s.Heading)
			break
		}
		out.WriteString(chunk)
	}
	return out.String()
}

// readBriefFile reads up to maxBytes from path. Returns ("", 0) on any
// error so the brief stays well-formed if the file is missing.
func readBriefFile(path string, maxBytes int) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return "", 0
	}
	return string(body), len(body)
}
