package council

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fixedTime returns a deterministic clock for snapshot stability.
func fixedTime() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) }

func seedBriefStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	st := newCouncilTestStore(t)
	ctx := context.Background()

	// Seed two intents at different priorities.
	for _, intent := range []*store.RoadmapIntent{
		{Theme: "Tier 1", Priority: 1, Summary: "Onboarding polish",
			LastSeenInRoadmapSHA: "sha-1"},
		{Theme: "Tier 2", Priority: 2, Summary: "Remote MCP transport",
			LastSeenInRoadmapSHA: "sha-1"},
	} {
		if err := st.Roadmap.Upsert(ctx, intent); err != nil {
			t.Fatalf("seed intent: %v", err)
		}
	}

	// Two queued backlog items so renderBacklog has content.
	for _, item := range []*store.BacklogItem{
		{ID: "MILLS-A", Title: "first", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "council"},
		{ID: "MILLS-B", Title: "second", State: store.BacklogQueued, Priority: store.P1, CreatedBy: "council"},
	} {
		if err := st.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed backlog: %v", err)
		}
	}

	// One KPI snapshot so renderKPI has content.
	if err := st.KPI.RecordSnapshot(ctx, &store.KPISnapshot{
		WindowSeconds: 86400,
		Metrics:       map[string]any{"cost_per_merged": 1.42},
	}); err != nil {
		t.Fatalf("seed kpi: %v", err)
	}

	// Repo root with an index file the brief should pull in.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".loom"), 0o755); err != nil {
		t.Fatalf("mkdir loom: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".loom/00-index.md"),
		[]byte("# Index\n\nActive planning slice: mills\n"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return st, repo
}

func TestCompile_ContainsAllSections(t *testing.T) {
	st, repo := seedBriefStore(t)
	b, err := Compile(context.Background(), BriefSources{
		Store: st, RepoRoot: repo, Now: fixedTime,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(b.Markdown, IntentsMissingMarker) {
		t.Errorf("populated intent store stamped missing marker:\n%s", b.Markdown)
	}
	if b.IntentsMissing {
		t.Error("populated intent store marked the brief struct IntentsMissing")
	}
	if b.SourceCounts.IntentsMissing {
		t.Error("populated intent store set SourceCounts.IntentsMissing")
	}

	// All four expected section headings present in deterministic order.
	wantHeadings := []string{
		"## Roadmap intents",
		"## Mills KPIs (last 24h snapshot)",
		"## Backlog snapshot",
		"## .loom/00-index.md (active planning thread)",
	}
	prev := -1
	for _, h := range wantHeadings {
		idx := strings.Index(b.Markdown, h)
		if idx < 0 {
			t.Errorf("brief missing heading %q\n%s", h, b.Markdown)
		}
		if idx <= prev {
			t.Errorf("heading %q out of order: idx=%d prev=%d", h, idx, prev)
		}
		prev = idx
	}

	// Section bodies must include the seeded values.
	for _, want := range []string{
		"Onboarding polish",
		"Remote MCP transport",
		"MILLS-A",
		"MILLS-B",
		"cost_per_merged",
		"Active planning slice",
	} {
		if !strings.Contains(b.Markdown, want) {
			t.Errorf("brief missing %q", want)
		}
	}

	// SourceCounts records what was sourced.
	if b.SourceCounts.Intents != 2 {
		t.Errorf("intents count: %d", b.SourceCounts.Intents)
	}
	if b.SourceCounts.BacklogQueued != 2 {
		t.Errorf("backlog queued: %d", b.SourceCounts.BacklogQueued)
	}
	if !b.SourceCounts.KPISnapshot {
		t.Errorf("KPI snapshot flag should be true")
	}
	if b.SourceCounts.IndexBytes == 0 {
		t.Errorf("index bytes should be > 0")
	}
}

func TestCompile_DeterministicForFixedInputs(t *testing.T) {
	st, repo := seedBriefStore(t)
	src := BriefSources{Store: st, RepoRoot: repo, Now: fixedTime}

	b1, err := Compile(context.Background(), src)
	if err != nil {
		t.Fatalf("compile 1: %v", err)
	}
	b2, err := Compile(context.Background(), src)
	if err != nil {
		t.Fatalf("compile 2: %v", err)
	}

	// Roadmap-intents body uses RoadmapDAO.List which orders by
	// (priority asc, theme asc) — deterministic. KPI map iteration is
	// non-deterministic in general, but with a single key the rendered
	// output stays stable. Backlog rendering is by query order
	// (newest-first by updated_at). All fixed inputs => identical bytes.
	if b1.Markdown != b2.Markdown {
		t.Errorf("brief markdown is non-deterministic; diff at first byte that differs")
	}
}

func TestCompile_TruncatesAtMaxBytes(t *testing.T) {
	st, repo := seedBriefStore(t)
	b, err := Compile(context.Background(), BriefSources{
		Store: st, RepoRoot: repo, MaxBytes: 200, Now: fixedTime,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(b.Markdown, "brief truncated at 200 bytes") {
		t.Errorf("expected truncation marker; got:\n%s", b.Markdown)
	}
	if len(b.Markdown) > 600 {
		// We allow the trailing _truncated_ note to push past MaxBytes.
		// 200 + ~150-byte note + safety margin → 600 is generous.
		t.Errorf("truncated brief is unexpectedly large: %d bytes", len(b.Markdown))
	}
}

func TestCompile_EmptyStoreStillRenders(t *testing.T) {
	st := newCouncilTestStore(t)
	b, err := Compile(context.Background(), BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(b.Markdown, "no roadmap intents") {
		t.Errorf("expected empty-state placeholder for intents:\n%s", b.Markdown)
	}
	if got := strings.Count(b.Markdown, IntentsMissingMarker); got != 1 {
		t.Errorf("missing marker count = %d, want 1:\n%s", got, b.Markdown)
	}
	if !strings.Contains(b.Markdown, "Backlog snapshot") {
		t.Errorf("backlog section should always render even when empty")
	}
	for _, want := range []string{"## Persisted incidents", "_(no persisted incidents)_"} {
		if !strings.Contains(b.Markdown, want) {
			t.Errorf("empty incident store missing %q:\n%s", want, b.Markdown)
		}
	}
}

func TestCompile_SurfacesAggregatedPersistedIncidents(t *testing.T) {
	st := newCouncilTestStore(t)
	ctx := context.Background()
	records := []*store.IncidentRecord{
		{ID: "INC-gitlab-2", Class: store.IncidentClassExternalDependency,
			Source: "gitlab-ci", Dependency: "gitlab", Shape: "service-unavailable",
			Summary: "GitLab CI service is unavailable", Evidence: "status 503 again",
			Retryable: true, OccurredAt: fixedTime().Add(-time.Minute)},
		{ID: "INC-model-1", Class: store.IncidentClassExternalDependency,
			Source: "model-api", Dependency: "model-provider", Shape: "rate-limit",
			Summary: "Model provider rate limit was exceeded", Evidence: "status 429",
			Retryable: true, OccurredAt: fixedTime().Add(-2 * time.Minute)},
		{ID: "INC-gitlab-1", Class: store.IncidentClassExternalDependency,
			Source: "gitlab-ci", Dependency: "gitlab", Shape: "service-unavailable",
			Summary: "GitLab CI service is unavailable", Evidence: "status 503",
			Retryable: true, OccurredAt: fixedTime().Add(-3 * time.Minute)},
	}
	for _, record := range records {
		inserted, err := st.Incidents.Put(ctx, record)
		if err != nil || !inserted {
			t.Fatalf("seed incident %s = (%t, %v), want (true, nil)", record.ID, inserted, err)
		}
	}

	b, err := Compile(ctx, BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, want := range []string{
		"## Persisted incidents",
		"class=`external_dependency_incident`",
		"disposition=`wait_for_dependency_recovery`",
		"occurrence_count=`2`",
		"occurrence_count=`1`",
		"do not propose outside-system remediation",
		"emit no proposal unless the follow-up changes repository-owned",
	} {
		if !strings.Contains(b.Markdown, want) {
			t.Fatalf("brief missing %q:\n%s", want, b.Markdown)
		}
	}
	if got := strings.Count(b.Markdown, "GitLab CI service is unavailable"); got != 1 {
		t.Fatalf("aggregated GitLab summary count = %d, want 1:\n%s", got, b.Markdown)
	}
	gitlab := strings.Index(b.Markdown, "GitLab CI service is unavailable")
	model := strings.Index(b.Markdown, "Model provider rate limit was exceeded")
	if gitlab < 0 || model < 0 || gitlab >= model {
		t.Fatalf("persisted incident ordering is not deterministic by identity:\n%s", b.Markdown)
	}
}

func TestCompile_RequiresStore(t *testing.T) {
	if _, err := Compile(context.Background(), BriefSources{}); err == nil {
		t.Error("expected error when Store is nil")
	}
}

// TestCompile_SurfacesCrossRunFindings confirms Loop C eval scores
// recorded in the last 7 days appear in the brief markdown so the next
// council run can see flaky-gate / stale-plan / divergent-outcome
// signals without re-running the queries.
func TestCompile_SurfacesCrossRunFindings(t *testing.T) {
	st := newCouncilTestStore(t)
	now := fixedTime()
	if err := st.Eval.RecordScore(context.Background(), &store.EvalScore{
		SubjectKind: store.EvalSubjectCrossRun,
		SubjectID:   "2026-04-19..2026-04-26",
		Rubric:      "loop_c_stale_plans",
		Score:       0.5,
		JudgedBy:    "loop_c_cross_run",
		EvaluatedAt: now.Add(-2 * time.Hour),
		Notes:       "1 backlog item stale: stale-001",
	}); err != nil {
		t.Fatalf("seed score: %v", err)
	}
	b, err := Compile(context.Background(), BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(b.Markdown, "Cross-run findings") {
		t.Errorf("brief missing Cross-run findings section:\n%s", b.Markdown)
	}
	if !strings.Contains(b.Markdown, "loop_c_stale_plans") {
		t.Errorf("brief missing rubric name:\n%s", b.Markdown)
	}
	if b.SourceCounts.CrossRunFindings != 1 {
		t.Errorf("CrossRunFindings = %d, want 1", b.SourceCounts.CrossRunFindings)
	}
}

// TestCompile_OmitsCrossRunSectionWhenEmpty asserts the section is
// dropped entirely when no Loop C scores landed in the window — the
// brief stays compact for routine runs.
func TestCompile_OmitsCrossRunSectionWhenEmpty(t *testing.T) {
	st := newCouncilTestStore(t)
	b, err := Compile(context.Background(), BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(b.Markdown, "Cross-run findings") {
		t.Errorf("empty Loop C should drop the section, got:\n%s", b.Markdown)
	}
}

func TestCompile_SurfacesClassifiedCIFailures(t *testing.T) {
	st := newCouncilTestStore(t)
	ctx := context.Background()
	now := fixedTime()
	item := &store.BacklogItem{
		ID: "MILLS-CI-AUTH", Title: "GitLab auth outage follow-up",
		State: store.BacklogEscalated, Priority: store.P1, CreatedBy: "test",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	retryable := false
	if err := st.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-CI-AUTH", BacklogID: item.ID, Template: "t",
		State: store.PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
		StartedAt: now.Add(-time.Hour), EscalationClass: "config",
		FailureClass: "configuration", ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency: "gitlab", EscalationRetryable: &retryable,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}

	b, err := Compile(ctx, BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, want := range []string{
		"Classified CI failures",
		"PIPE-CI-AUTH",
		"class=`external_dependency_incident`",
		"retryable=`false`",
		"free_retry=`false`",
		"terminal=`true`",
		"classifier=`mills-failure-classifier`",
		"external=`gitlab`",
		"GitLab auth outage follow-up",
	} {
		if !strings.Contains(b.Markdown, want) {
			t.Fatalf("brief missing %q:\n%s", want, b.Markdown)
		}
	}
	if b.SourceCounts.ClassifiedCIFailures != 1 {
		t.Fatalf("ClassifiedCIFailures = %d, want 1", b.SourceCounts.ClassifiedCIFailures)
	}
	if len(b.ClassifiedCIFailures) != 1 || b.ClassifiedCIFailures[0].RunID != "PIPE-CI-AUTH" {
		t.Fatalf("structured classified failures = %+v, want PIPE-CI-AUTH", b.ClassifiedCIFailures)
	}
	body, err := json.Marshal(b.SourceCounts)
	if err != nil {
		t.Fatalf("marshal source counts: %v", err)
	}
	if !strings.Contains(string(body), `"classified_ci_failures":1`) {
		t.Fatalf("source count JSON missing classified_ci_failures: %s", body)
	}
	ciBody, err := json.Marshal(b.ClassifiedCIFailures[0])
	if err != nil {
		t.Fatalf("marshal classified failure: %v", err)
	}
	for _, want := range []string{
		`"run_id":"PIPE-CI-AUTH"`,
		`"classifier":"mills-failure-classifier"`,
		`"failure_class":"configuration"`,
		`"external_dependency":"gitlab"`,
		`"retryable":false`,
		`"free_retry":false`,
		`"terminal":true`,
	} {
		if !strings.Contains(string(ciBody), want) {
			t.Fatalf("classified failure JSON missing %q: %s", want, ciBody)
		}
	}
}

// An empty canonical intent store must mark the Brief STRUCT, not only its
// Markdown. The council runner gates scheduling on brief.IntentsMissing, so a
// marker that exists only in the rendered text would leave the guardrail
// unenforceable.
func TestCompile_EmptyIntentStoreMarksBriefStruct(t *testing.T) {
	st := newCouncilTestStore(t)
	b, err := Compile(context.Background(), BriefSources{Store: st, Now: fixedTime})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !b.IntentsMissing {
		t.Error("empty intent store did not set Brief.IntentsMissing")
	}
	if !strings.Contains(b.Markdown, IntentsMissingMarker) {
		t.Errorf("empty intent store did not stamp the marker:\n%s", b.Markdown)
	}
	if b.SourceCounts.Intents != 0 {
		t.Errorf("SourceCounts.Intents = %d, want 0", b.SourceCounts.Intents)
	}
	if !b.SourceCounts.IntentsMissing {
		t.Error("empty intent store did not set SourceCounts.IntentsMissing")
	}
}

// The wire projection the operator API serves (brief_sources) must carry the
// flag so HUD/CLI can explain a blocked run without a new API field.
func TestBriefSourceCountsSerializesIntentsMissing(t *testing.T) {
	raw, err := json.Marshal(BriefSourceCounts{IntentsMissing: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"intents_missing":true`) {
		t.Errorf("brief_sources JSON = %s, want intents_missing:true", raw)
	}
}
