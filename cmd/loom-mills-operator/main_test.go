package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/store"
)

const validPolicy = `
version: 1
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 5 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 5, max_concurrent_runs: 2 }
council:
  schedule_cron: "0 5 * * *"
  ensemble:
    editor: { name: editor, model: qwen3-8b, backend: flexinfer }
    reviewers:
      - { name: architecture, model: qwen3-8b, backend: flexinfer, lens: architecture }
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
pipeline:
  default_template: mills-default-pipeline
  retry: { max_attempts: 3, cooldown_seconds: 60 }
human_handoff:
  on_escalation_create_handoff: true
  on_escalation_create_issue: true
`

// newTestOperator wires a fully-functional operator backed by a temp-dir
// SQLite + a temp YAML policy. Only the HTTP listeners are not started; the
// caller drives the muxes through httptest.
func newTestOperator(t *testing.T) (*operator, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mills.db")
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(validPolicy), 0o644); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	st, err := store.Open(context.Background(), store.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), policyPath,
		mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		_ = st.Close()
		t.Fatalf("policy manager: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	op := newOperator(st, pm, mills.NewBudget(pm, mills.NewStoreBudgetReader(st)), logger)

	cleanup := func() {
		_ = pm.Close()
		_ = st.Close()
	}
	return op, cleanup
}

func TestHealthz_OK(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.metricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body: %q", rec.Body.String())
	}
}

func TestReadyz_503BeforeReady(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.metricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pre-ready: got %d want 503", rec.Code)
	}

	op.markReady()
	rec = httptest.NewRecorder()
	op.metricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("post-ready: got %d want 200", rec.Code)
	}
}

func TestStagePromptForIncludesBacklogContext(t *testing.T) {
	prompt := stagePromptFor("implement", nil)(pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item: &store.BacklogItem{
			ID:      "MILLS-CANARY-TEST",
			Title:   "canary",
			Labels:  []string{"mills-canary"},
			SpecDoc: "Update only testdata/mills-canary/heartbeat.md",
			Success: store.SuccessCriteria{
				Tests:       []string{"go test ./cmd/loom -run Mills"},
				ManualCheck: "fixture-only MR",
			},
			Slices: []store.Slice{{
				Name:  "heartbeat",
				Files: []string{"testdata/mills-canary/heartbeat.md"},
			}},
		},
	})
	for _, want := range []string{
		"Backlog context:",
		"mills-canary",
		"Update only testdata/mills-canary/heartbeat.md",
		"go test ./cmd/loom -run Mills",
		"files=testdata/mills-canary/heartbeat.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestImplementPromptForAddsDocsGuardrailDiscipline(t *testing.T) {
	jc := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item: &store.BacklogItem{
			ID:    "MILLS-2026-06-30-001",
			Title: "Implement reliability guardrails",
			Slices: []store.Slice{{
				Name:  "guardrail-model",
				Files: []string{"pkg/mills/policy.go"},
			}},
		},
	}
	prompt := implementPromptFor(nil)(jc)

	// The base implement prompt is preserved...
	for _, want := range []string{
		"Implement backlog item MILLS-2026-06-30-001",
		"git push -u origin HEAD",
		"Backlog context:",
		"files=pkg/mills/policy.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("implement prompt dropped base content %q:\n%s", want, prompt)
		}
	}
	// ...and the docs-guardrail discipline is appended so the spawn adds a
	// per-MR changelog FRAGMENT (not a colliding CHANGELOG.md edit) instead of
	// failing guardrails:docs-cli at ci_watch.
	for _, want := range []string{
		"guardrails:docs-cli",
		"changelog.d/<slug>.<category>.md",
		"Do NOT edit CHANGELOG.md directly",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("implement prompt missing docs-guardrail instruction %q:\n%s", want, prompt)
		}
	}
	// The plain stage builder must NOT carry it (the discipline is the wrapper's job).
	if strings.Contains(stagePromptFor("implement", nil)(jc), "guardrails:docs-cli") {
		t.Error("base stagePromptFor(implement) should not include the docs guardrail; only implementPromptFor should")
	}
}

func TestImplementPromptForUsesFixtureCanaryFastPath(t *testing.T) {
	jc := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item: &store.BacklogItem{
			ID:     "MILLS-CANARY-AUTOPILOT-TEST",
			Title:  "Mills canary: update heartbeat fixture",
			Labels: []string{"mills-canary", "safe-fixture"},
			Success: store.SuccessCriteria{
				Tests: []string{"go test ./cmd/loom -run Mills"},
			},
			Slices: []store.Slice{{
				Name:  "heartbeat-fixture",
				Files: []string{"testdata/mills-canary/heartbeat.md"},
			}},
		},
	}
	prompt := implementPromptFor(nil)(jc)
	for _, want := range []string{
		"CANARY FAST PATH",
		"Do NOT run the required tests",
		"dedicated tests stage",
		"Do not update CHANGELOG.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("canary implement prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "DOCS GUARDRAIL") {
		t.Errorf("fixture canary must not receive code-change docs discipline:\n%s", prompt)
	}
}

func TestImplementPromptForRequiresBothFixtureCanaryLabels(t *testing.T) {
	for _, labels := range [][]string{{"mills-canary"}, {"safe-fixture"}, {"mills-canary", "docs"}} {
		prompt := implementPromptFor(nil)(pipeline.JobContext{
			Stage: pipeline.Stage{ID: "implement"},
			Item:  &store.BacklogItem{ID: "ITEM", Labels: labels},
		})
		if !strings.Contains(prompt, "DOCS GUARDRAIL") {
			t.Errorf("labels %v unexpectedly enabled canary fast path", labels)
		}
	}
}

func TestPrSelfReviewPromptForUsesFixtureCanaryFastPath(t *testing.T) {
	jc := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "pr_self_review"},
		Item: &store.BacklogItem{
			ID:     "MILLS-CANARY-REVIEW-TEST",
			Title:  "Mills canary: update heartbeat fixture",
			Labels: []string{"mills-canary", "safe-fixture"},
			Success: store.SuccessCriteria{
				Tests: []string{"go test ./cmd/loom -run Mills"},
			},
		},
	}
	prompt := prSelfReviewPromptFor(nil)(jc)
	for _, want := range []string{
		"CANARY REVIEW FAST PATH",
		"dedicated tests stage has already passed",
		"Do NOT run tests, builds, linters",
		"Review only the existing cumulative branch diff",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("canary self-review prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestPrSelfReviewPromptForScopesVerification(t *testing.T) {
	prompt := prSelfReviewPromptFor(nil)(pipeline.JobContext{
		Stage: pipeline.Stage{ID: "pr_self_review"},
		Item: &store.BacklogItem{
			ID:    "ITEM",
			Title: "review prompt scope",
		},
	})
	for _, want := range []string{
		"cumulative branch diff",
		"backlog specification",
		"tests-stage and CI outcomes already recorded",
		"Do NOT re-run builds or full test suites",
		"verification belongs to the dedicated tests stage and CI",
		"cheap, targeted static check",
		"single-package test only when the diff clearly makes it necessary",
		"state the reason before running it",
		"pr_self_review_v1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("self-review prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestPrSelfReviewPromptForRequiresBothFixtureCanaryLabels(t *testing.T) {
	for _, labels := range [][]string{{"mills-canary"}, {"safe-fixture"}, {"mills-canary", "docs"}} {
		prompt := prSelfReviewPromptFor(nil)(pipeline.JobContext{
			Stage: pipeline.Stage{ID: "pr_self_review"},
			Item:  &store.BacklogItem{ID: "ITEM", Labels: labels},
		})
		if strings.Contains(prompt, "CANARY REVIEW FAST PATH") {
			t.Errorf("labels %v unexpectedly enabled canary self-review fast path", labels)
		}
	}
}

func TestStagePromptForResearchIsNotBlank(t *testing.T) {
	prompt := stagePromptFor("research", nil)(pipeline.JobContext{
		Stage: pipeline.Stage{ID: "research"},
		Item: &store.BacklogItem{
			ID:     "MILLS-CANARY-TEST",
			Title:  "canary",
			Labels: []string{"mills-canary", "safe-fixture"},
			Success: store.SuccessCriteria{
				Tests: []string{"go test ./cmd/loom -run Mills"},
			},
		},
	})
	for _, want := range []string{
		"Research backlog item MILLS-CANARY-TEST",
		"Backlog context:",
		"mills-canary",
		"go test ./cmd/loom -run Mills",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("research prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestMetrics_Endpoint(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.metricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "go_") {
		t.Errorf("expected Go runtime metrics in /metrics output; got %d bytes", len(body))
	}
}

func TestStatus_FullResponds(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.markReady()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	// Slice 2.4 wires real values: queue_depth and active_pipeline_runs
	// are now ints (zero on a fresh DB), and the slice tag advances.
	for _, want := range []string{
		`"db_ok":true`, `"policy_enabled":true`,
		`"autonomy_ready":false`,
		`"queue_depth":0`, `"active_pipeline_runs":0`,
		`"slice":"2.4-rest-surface"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("response missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestCapabilities_FailClosedWhenPolicyEnabledAndStubsRemain(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("test-token")
	t.Cleanup(func() { setAdminToken("") })

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".loom"), 0o755); err != nil {
		t.Fatalf("mkdir .loom: %v", err)
	}
	w := newCapabilityWiring(Config{
		DBPath:            filepath.Join(repo, "mills.db"),
		PolicyPath:        filepath.Join(repo, "policy.yaml"),
		RepoRoot:          repo,
		FlexInferProxyURL: "http://flexinfer.test",
		GitLabAPIURL:      "https://gitlab.example/api/v4",
		GitLabToken:       "token",
		GitLabProject:     "services/loom-core",
		HUDBaseURL:        "http://hud.test",
		HUDToken:          "hud-token",
	})
	w.FlexInferConfigured = true
	w.FlexInferReady = true
	w.GitLabConfigured = true
	w.GitLabReady = true
	w.HUDSpawnConfigured = true
	w.HUDSpawnReady = true
	w.MCPHubConfigured = true
	w.MCPHubSessionReady = true
	w.CouncilConfigured = true
	w.CouncilUsesFakeAgents = true
	w.BranchContractReady = true
	for stage := range w.DispatcherRealStages {
		w.DispatcherRealStages[stage] = true
	}
	w.DispatcherRealStages["implement"] = false
	op.setCapabilities(w)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got capabilityReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got.AutonomyReady {
		t.Fatalf("autonomy_ready = true, want false")
	}
	body := rec.Body.String()
	for _, want := range []string{
		`dispatcher_write_stages: 8/9 write stages use real workers; stubbed stages: implement`,
		`council_participants: council uses FakeReviewer/FakeEditor/FakeLLMJudge participants`,
	} {
		if !strings.Contains(strings.ReplaceAll(body, `"`, ``), want) {
			t.Errorf("response missing blocker %q: %s", want, body)
		}
	}
}

func TestBuildCouncilRunner_UsesRealParticipantsWhenFlexInferReady(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.test"})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	r, usesFake := buildCouncilRunner(op.store, op.policy, op.budget, t.TempDir(), flex, nil, flex, "", runner.DefaultStageBudgets(), discardLogger())
	if r == nil {
		t.Fatal("runner nil")
	}
	if usesFake {
		t.Fatal("usesFake = true, want false with FlexInfer client")
	}
}

func TestBuildCouncilRunner_FakeFallbackWhenFlexInferMissing(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	r, usesFake := buildCouncilRunner(op.store, op.policy, op.budget, t.TempDir(), nil, nil, nil, "", runner.DefaultStageBudgets(), discardLogger())
	if r == nil {
		t.Fatal("runner nil")
	}
	if !usesFake {
		t.Fatal("usesFake = false, want true without FlexInfer client")
	}
}

func TestRunOperatorSessionMaintainerRetriesAndFlipsCapability(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	w := newCapabilityWiring(Config{})
	w.MCPHubConfigured = true
	op.setCapabilities(w)

	caller := &fakeAgentContextCaller{
		errs:   []error{errors.New("backend unavailable"), nil},
		bodies: []string{"", `{"session_id":"session-recovered"}`},
	}
	ref := &operatorSessionRef{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runOperatorSessionMaintainer(ctx, caller, ref, op, discardLogger(), time.Millisecond)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for ref.SessionID() != "session-recovered" {
		select {
		case <-deadline:
			t.Fatal("session retry did not recover")
		case <-tick.C:
		}
	}
	cancel()
	<-done

	row := findCapabilityRow(op.capabilityReport(context.Background()).Capabilities, "mcp_hub_session")
	if row.Status != "green" {
		t.Fatalf("mcp_hub_session capability did not flip green: %+v", row)
	}
}

func TestRunOperatorSessionMaintainerProbesExistingSession(t *testing.T) {
	ref := &operatorSessionRef{}
	ref.Set("session-existing")
	caller := &recordingAgentContextCaller{calls: make(chan string, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runOperatorSessionMaintainer(ctx, caller, ref, nil, discardLogger(), time.Hour)
		close(done)
	}()

	select {
	case tool := <-caller.calls:
		if tool != "agent_session_list" {
			t.Fatalf("health probe tool = %q, want agent_session_list", tool)
		}
	case <-time.After(time.Second):
		t.Fatal("existing operator session was not health-probed")
	}
	cancel()
	<-done
}

func TestWorkflowAdmissionAllowed(t *testing.T) {
	tests := []struct {
		name                                string
		canary, policy, workflows, autonomy bool
		want                                bool
	}{
		{name: "production ready", policy: true, workflows: true, autonomy: true, want: true},
		{name: "production capability failure", policy: true, workflows: true, autonomy: false},
		{name: "crash canary window", workflows: true, want: true},
		{name: "runtime disabled", policy: true, autonomy: true},
		{name: "canary admission transaction", canary: true, workflows: true, autonomy: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workflowAdmissionAllowed(tt.canary, tt.policy, tt.workflows, tt.autonomy); got != tt.want {
				t.Fatalf("workflowAdmissionAllowed() = %t, want %t", got, tt.want)
			}
		})
	}
}

type recordingAgentContextCaller struct {
	calls chan string
}

func (f *recordingAgentContextCaller) CallTool(_ context.Context, serverName, toolName string, args map[string]any) (string, error) {
	if serverName != clients.AgentContextServerName {
		return "", errors.New("unexpected health probe server")
	}
	if toolName == "agent_session_list" && (args["light"] != true || args["limit"] != 1) {
		return "", errors.New("unexpected health probe args")
	}
	f.calls <- toolName
	return `{}`, nil
}

func TestCapabilities_MCPHubLiveFailureBlocksAutonomy(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	w := newCapabilityWiring(Config{})
	w.MCPHubConfigured = true
	w.MCPHubSessionReady = true
	w.MCPHubLiveHealth = func() (bool, string) {
		return false, "agent_context unavailable after 4 consecutive transport failures"
	}
	op.setCapabilities(w)

	report := op.capabilityReport(context.Background())
	row := findCapabilityRow(report.Capabilities, "mcp_hub_session")
	if row.Status != "red" || !strings.Contains(row.Message, "4 consecutive transport failures") {
		t.Fatalf("mcp_hub_session row = %+v, want live failure", row)
	}
	if report.AutonomyReady {
		t.Fatal("autonomy stayed ready while the live MCP hub dependency was failing")
	}
}

type fakeAgentContextCaller struct {
	errs   []error
	bodies []string
	calls  int
}

func (f *fakeAgentContextCaller) CallTool(_ context.Context, serverName, toolName string, _ map[string]any) (string, error) {
	if serverName != clients.AgentContextServerName {
		return "", errors.New("unexpected server")
	}
	if toolName != "agent_session_start" {
		return "", errors.New("unexpected tool")
	}
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return "", f.errs[i]
	}
	if i < len(f.bodies) {
		return f.bodies[i], nil
	}
	return `{"session_id":"session-default"}`, nil
}

func TestCapabilities_RepoRootRequiresWritableLoomDir(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	repo := t.TempDir()
	op.setCapabilities(newCapabilityWiring(Config{RepoRoot: repo}))
	report := op.capabilityReport(context.Background())
	repoRow := findCapabilityRow(report.Capabilities, "repo_root")
	if repoRow.Status != "red" || !strings.Contains(repoRow.Message, "git checkout metadata is missing") {
		t.Fatalf("missing .git row = %+v", repoRow)
	}

	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	report = op.capabilityReport(context.Background())
	repoRow = findCapabilityRow(report.Capabilities, "repo_root")
	if repoRow.Status != "red" || !strings.Contains(repoRow.Message, ".loom directory is missing") {
		t.Fatalf("missing .loom row = %+v", repoRow)
	}

	if err := os.Mkdir(filepath.Join(repo, ".loom"), 0o755); err != nil {
		t.Fatalf("mkdir .loom: %v", err)
	}
	report = op.capabilityReport(context.Background())
	repoRow = findCapabilityRow(report.Capabilities, "repo_root")
	if repoRow.Status != "green" || repoRow.Mode != "real" {
		t.Fatalf("writable .loom row = %+v", repoRow)
	}
}

func TestCapabilities_KPIWriterReadyIsGreen(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	w := newCapabilityWiring(Config{})
	w.KPIWriterReady = true
	w.KPIWriterSource = "pkg/mills/kpi_writer.go"
	op.setCapabilities(w)

	report := op.capabilityReport(context.Background())
	row := findCapabilityRow(report.Capabilities, "kpi_writer")
	if row.Status != "green" || row.Mode != "real" {
		t.Fatalf("kpi row = %+v, want green real", row)
	}
	if row.Source != "pkg/mills/kpi_writer.go" {
		t.Fatalf("kpi source = %q", row.Source)
	}
}

func findCapabilityRow(rows []capabilityRow, id string) capabilityRow {
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	return capabilityRow{}
}

func TestUnknownPath_Returns404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	// /api/mills/council/runs is now wired (slice 2.4) — pick a definitely
	// unknown path under /api/mills/ instead. Returns 404 from the
	// catch-all, not 501 (which is reserved for action stubs whose
	// implementation lands in a later slice).
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/no-such-endpoint", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"empty db path", func(c *Config) { c.DBPath = "" }, "db-path is required"},
		{"empty policy path", func(c *Config) { c.PolicyPath = "" }, "policy-path is required"},
		{"both listeners disabled", func(c *Config) { c.HTTPAddr = ""; c.MetricsAddr = "" }, "at least one of"},
		{"db-path missing dir", func(c *Config) { c.DBPath = "mills.db" }, "must include a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v does not contain %q", err, tc.want)
			}
		})
	}
}

func TestConfig_ApplyEnv(t *testing.T) {
	t.Setenv("LOOM_MILLS_DB_PATH", "/tmp/x.db")
	t.Setenv("LOOM_MILLS_POLICY_PATH", "/tmp/policy.yaml")
	t.Setenv("LOOM_MILLS_HTTP_ADDR", ":1234")
	t.Setenv("LOOM_MILLS_METRICS_ADDR", ":5678")
	t.Setenv("LOOM_MILLS_DEBUG", "true")
	t.Setenv("LOOM_MILLS_ENABLED", "false")

	c := DefaultConfig()
	c.ApplyEnv()
	if c.DBPath != "/tmp/x.db" || c.PolicyPath != "/tmp/policy.yaml" {
		t.Errorf("paths: %+v", c)
	}
	if c.HTTPAddr != ":1234" || c.MetricsAddr != ":5678" {
		t.Errorf("addrs: %+v", c)
	}
	if !c.Debug {
		t.Errorf("debug not set")
	}
	if c.EnableReconciler == nil || *c.EnableReconciler {
		t.Errorf("expected reconciler disabled, got %+v", c.EnableReconciler)
	}
}

func TestConfig_ApplyEnv_CouncilStageBudgets(t *testing.T) {
	t.Setenv("LOOM_MILLS_COUNCIL_OVERALL_TIMEOUT", "30m")
	t.Setenv("LOOM_MILLS_COUNCIL_EDITOR_TIMEOUT", "4m")
	t.Setenv("LOOM_MILLS_COUNCIL_ASYNC_CONCURRENCY", "3")
	t.Setenv("LOOM_MILLS_COUNCIL_JUDGE_TIMEOUT", "not-a-duration")

	c := DefaultConfig()
	c.ApplyEnv()

	if c.CouncilStages.Overall != 30*time.Minute {
		t.Errorf("overall = %s, want 30m", c.CouncilStages.Overall)
	}
	if c.CouncilStages.Editor != 4*time.Minute {
		t.Errorf("editor = %s, want 4m", c.CouncilStages.Editor)
	}
	if c.CouncilAsyncConcurrency != 3 {
		t.Errorf("async concurrency = %d, want 3", c.CouncilAsyncConcurrency)
	}
	// An unparseable knob must fall back to the runner default, never to 0
	// (which would silently unbound the stage).
	if want := runner.DefaultStageBudgets().Judge; c.CouncilStages.Judge != want {
		t.Errorf("judge = %s, want default %s on an invalid value", c.CouncilStages.Judge, want)
	}
	if want := runner.DefaultStageBudgets().Debate; c.CouncilStages.Debate != want {
		t.Errorf("debate = %s, want default %s", c.CouncilStages.Debate, want)
	}
}

// TestRunListener_GracefulShutdown verifies that runListener stops cleanly
// when its context cancels — the lifecycle contract every long-lived listener
// in the operator must satisfy.
func TestRunListener_GracefulShutdown(t *testing.T) {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()

	var lc net.ListenConfig
	listener, err := lc.Listen(probeCtx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	op, cleanup := newTestOperator(t)
	defer cleanup()
	srv := httpServer(addr, op.metricsMux())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runListener(ctx, "test", srv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// Probe /healthz to confirm the server is alive before cancelling.
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+addr+"/healthz", nil)
		if err != nil {
			t.Fatalf("build probe: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runListener returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("runListener did not exit within 5s")
	}
}

// Regression for the 2026-07-01 plan-linked retry wedge: a gate-fail retry
// spawns a FRESH agent in a fresh clone, while the plan store still shows the
// slice advanced by the discarded attempt — so without an explicit retry
// block the agent skips the work and fails nonempty_diff. The implement
// prompt must name the failed gate and order a redo when RetryContext rides
// on the JobContext, and must stay unchanged on a first attempt.
func TestImplementPromptForAddsRetryDiscipline(t *testing.T) {
	jc := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item: &store.BacklogItem{
			ID:     "PSL-PATTERN-STAMP-1",
			Title:  "stamp go-rest-service",
			PlanID: "plan-pattern-stamp-1",
		},
	}
	// First attempt: no retry block.
	first := implementPromptFor(nil)(jc)
	if strings.Contains(first, "RETRY CONTEXT") {
		t.Errorf("first-attempt prompt must not carry the retry discipline:\n%s", first)
	}

	jc.RetryContext = &pipeline.StageRetryContext{
		Attempt:      2,
		GateStage:    "post_implement_gate",
		FirstFailure: "scope: file docs/x.md outside slice scope",
		LastFailure:  "scope: file docs/x.md outside slice scope",
	}
	retry := implementPromptFor(nil)(jc)
	for _, want := range []string{
		"RETRY CONTEXT (implement attempt 2)",
		"FAILED the post_implement_gate gate",
		"scope: file docs/x.md outside slice scope",
		"STALE",
		"agent_plan_slice_update",
		"non-empty committed diff",
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry prompt missing %q:\n%s", want, retry)
		}
	}
	// The base prompt content is preserved alongside the retry block.
	for _, want := range []string{
		"Implement backlog item PSL-PATTERN-STAMP-1",
		"guardrails:docs-cli",
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry prompt dropped base content %q", want)
		}
	}

	// A retry whose latest failure differs from the first (the do-nothing
	// retry's empty diff) surfaces BOTH.
	jc.RetryContext.Attempt = 3
	jc.RetryContext.LastFailure = "nonempty_diff: no files changed by implement stage"
	retry = implementPromptFor(nil)(jc)
	for _, want := range []string{
		"scope: file docs/x.md outside slice scope",
		"most recent failure: nonempty_diff: no files changed by implement stage",
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("attempt-3 retry prompt missing %q:\n%s", want, retry)
		}
	}
}

// mrDiffTransport serves the MR-diffs endpoint for the merged-diff
// loader test and 404s everything else.
type mrDiffTransport struct {
	entries []map[string]any
}

type pipelineProjectResolverFunc func(context.Context, string) (string, error)

func (f pipelineProjectResolverFunc) AuthorizedProject(ctx context.Context, runID string) (string, error) {
	return f(ctx, runID)
}

type pathRecordingMRStateTransport struct {
	paths []string
}

func (rt *pathRecordingMRStateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.paths = append(rt.paths, req.URL.Path)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"state":"opened"}`)),
		Header:     make(http.Header),
	}, nil
}

func TestTakeupMRStateClientUsesConfiguredProject(t *testing.T) {
	gl, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL: "https://gitlab.example/api/v4", Token: "tok", Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rt := &pathRecordingMRStateTransport{}
	gl.SetTransport(rt)
	state, err := takeupMRStateClient(gl, "services/flexdeck").MRState(context.Background(), 247)
	if err != nil || state != "opened" {
		t.Fatalf("MRState = %q, %v", state, err)
	}
	if len(rt.paths) != 1 || !strings.Contains(rt.paths[0], "/projects/services/flexdeck/merge_requests/247") {
		t.Fatalf("MRState paths = %v, want configured foreign project", rt.paths)
	}
}

func (rt *mrDiffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/merge_requests/") && strings.HasSuffix(req.URL.Path, "/diffs") {
		buf, _ := json.Marshal(rt.entries)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(buf))), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"not found"}`)), Header: make(http.Header)}, nil
}

// The pipeline_merge audit artifact must contain the REAL unified diff —
// the v2.0 metadata-only stub made every audit score 0.00 "diff
// unreadable" and open a critical advisory issue (#225/#227/#233/#234).
func TestGitlabMergedDiffLoader(t *testing.T) {
	gl, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:  "https://gitlab.example/api/v4",
		Token:   "tok",
		Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	gl.SetTransport(&mrDiffTransport{entries: []map[string]any{
		{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-x\n+y\n"},
	}})
	loader := gitlabMergedDiffLoader(gl, pipelineProjectResolverFunc(func(context.Context, string) (string, error) {
		return "services/loom-core", nil
	}))

	mriid := int64(42)
	run := &store.PipelineRun{ID: "PIPE-X-1", MRIID: &mriid, CostUSD: 1.5, Attempts: 2}
	item := &store.BacklogItem{ID: "BL-X", Title: "do the thing", Slices: []store.Slice{{Name: "s1", Files: []string{"a.go"}}}}

	out, err := loader(context.Background(), run, item)
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	for _, want := range []string{
		"# Pipeline merge PIPE-X-1",
		"Backlog: BL-X — do the thing",
		"## Unified diff",
		"diff --git a/a.go b/a.go",
		"@@ -1 +1 @@",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("artifact missing %q:\n%s", want, out)
		}
	}

	// No MR iid → "" so the trigger skips instead of auditing metadata.
	if out, err := loader(context.Background(), &store.PipelineRun{ID: "PIPE-NO-MR"}, item); err != nil || out != "" {
		t.Errorf("no-MRIID loader = (%q, %v), want empty skip", out, err)
	}

	// Empty diff from GitLab → "" skip, not a metadata-only artifact.
	gl.SetTransport(&mrDiffTransport{entries: []map[string]any{}})
	if out, err := loader(context.Background(), run, item); err != nil || out != "" {
		t.Errorf("empty-diff loader = (%q, %v), want empty skip", out, err)
	}

	// nil client → "" skip.
	if out, err := gitlabMergedDiffLoader(nil, nil)(context.Background(), run, item); err != nil || out != "" {
		t.Errorf("nil-client loader = (%q, %v), want empty skip", out, err)
	}
}

// pathRecordingDiffTransport serves the MR-diffs endpoint and records every
// request path so the cross-repo test can assert WHICH project was queried.
type pathRecordingDiffTransport struct {
	mrDiffTransport
	paths []string
}

func (rt *pathRecordingDiffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.paths = append(rt.paths, req.URL.Path)
	return rt.mrDiffTransport.RoundTrip(req)
}

// A merged MR's project comes from durable successful-stage provenance, not a
// backlog target that may be edited before the post-merge audit runs.
func TestGitlabMergedDiffLoader_UsesPersistedProjectAfterItemReroute(t *testing.T) {
	gl, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:  "https://gitlab.example/api/v4",
		Token:   "tok",
		Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rt := &pathRecordingDiffTransport{mrDiffTransport: mrDiffTransport{entries: []map[string]any{
		{"old_path": "README.md", "new_path": "README.md", "diff": "@@ -1 +1 @@\n-a\n+b\n"},
	}}}
	gl.SetTransport(rt)
	loader := gitlabMergedDiffLoader(gl, pipelineProjectResolverFunc(func(_ context.Context, runID string) (string, error) {
		if runID == "PIPE-HOME" {
			return "services/loom-core", nil
		}
		return "services/flexdeck", nil
	}))

	mriid := int64(247)
	run := &store.PipelineRun{ID: "PIPE-XREPO-1", MRIID: &mriid}
	item := &store.BacklogItem{ID: "BL-XREPO", Title: "flexdeck readme marker", TargetProject: "services/rerouted"}

	if _, err := loader(context.Background(), run, item); err != nil {
		t.Fatalf("loader: %v", err)
	}
	if len(rt.paths) == 0 {
		t.Fatal("no MR-diff request recorded")
	}
	for _, p := range rt.paths {
		if !strings.Contains(p, "/projects/services/flexdeck/") {
			t.Errorf("diff request went to %q, want durable project services/flexdeck", p)
		}
	}

	// A durable home-project run keeps querying home even if the item is rerouted.
	rt.paths = nil
	homeRun := &store.PipelineRun{ID: "PIPE-HOME", MRIID: &mriid}
	home := &store.BacklogItem{ID: "BL-HOME", Title: "home item", TargetProject: "services/flexdeck"}
	if _, err := loader(context.Background(), homeRun, home); err != nil {
		t.Fatalf("home loader: %v", err)
	}
	for _, p := range rt.paths {
		if !strings.Contains(p, "/projects/services/loom-core/") {
			t.Errorf("home diff request went to %q, want services/loom-core", p)
		}
	}
}

// litellmPolicy mirrors validPolicy but adds a frontier reviewer lens routed
// through the LiteLLM gateway (backend "litellm", e.g. or/kimi-k3).
const litellmPolicy = `
version: 1
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 5 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 5, max_concurrent_runs: 2 }
council:
  schedule_cron: "0 5 * * *"
  ensemble:
    editor: { name: editor, model: qwen3-8b, backend: flexinfer }
    reviewers:
      - { name: architecture, model: qwen3-8b, backend: flexinfer, lens: architecture }
      - { name: frontier, model: or/kimi-k3, backend: litellm, lens: frontier }
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
pipeline:
  default_template: mills-default-pipeline
  retry: { max_attempts: 3, cooldown_seconds: 60 }
human_handoff:
  on_escalation_create_handoff: true
  on_escalation_create_issue: true
`

func newLiteLLMPolicyManager(t *testing.T) *mills.PolicyManager {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(litellmPolicy), 0o644); err != nil {
		t.Fatalf("seed litellm policy: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), policyPath,
		mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy manager: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	return pm
}

// TestBuildCouncilRunner_LiteLLMLensBinding pins the wave-3 wiring: a reviewer
// lens with backend "litellm" binds to the LiteLLM client (not the FlexInfer
// proxy — its models are not federated there), while flexinfer lenses keep the
// flex client.
func TestBuildCouncilRunner_LiteLLMLensBinding(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	pm := newLiteLLMPolicyManager(t)

	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.test"})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	lite, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://litellm.test", Token: "k"})
	if err != nil {
		t.Fatalf("litellm client: %v", err)
	}
	r, usesFake := buildCouncilRunner(op.store, pm, op.budget, t.TempDir(), flex, lite, flex, "", runner.DefaultStageBudgets(), discardLogger())
	if r == nil || usesFake {
		t.Fatalf("runner nil=%v usesFake=%v; want real participants", r == nil, usesFake)
	}
	disp := r.Reviewers
	frontier, ok := disp.Reviewers["frontier"].(*clients.FlexInferCouncilReviewer)
	if !ok {
		t.Fatalf("frontier lens reviewer type: %T", disp.Reviewers["frontier"])
	}
	if frontier.Client != lite {
		t.Error("frontier lens must bind the litellm client")
	}
	arch, ok := disp.Reviewers["architecture"].(*clients.FlexInferCouncilReviewer)
	if !ok {
		t.Fatalf("architecture lens reviewer type: %T", disp.Reviewers["architecture"])
	}
	if arch.Client != flex {
		t.Error("architecture lens must keep the flexinfer client")
	}
}

// TestBuildCouncilRunner_LiteLLMLensWithoutGatewayFallsBack pins the visible-
// misconfiguration contract: a litellm lens with no gateway configured gets the
// fake reviewer (note in council output) instead of silently 404ing against
// the FlexInfer proxy.
func TestBuildCouncilRunner_LiteLLMLensWithoutGatewayFallsBack(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	pm := newLiteLLMPolicyManager(t)

	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.test"})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	r, _ := buildCouncilRunner(op.store, pm, op.budget, t.TempDir(), flex, nil, flex, "", runner.DefaultStageBudgets(), discardLogger())
	if r == nil {
		t.Fatal("runner nil")
	}
	disp := r.Reviewers
	if _, ok := disp.Reviewers["frontier"].(*council.FakeReviewer); !ok {
		t.Fatalf("frontier lens without gateway should be a FakeReviewer, got %T", disp.Reviewers["frontier"])
	}
	if _, ok := disp.Reviewers["architecture"].(*clients.FlexInferCouncilReviewer); !ok {
		t.Fatalf("architecture lens should stay real, got %T", disp.Reviewers["architecture"])
	}
}

// --- MILLS_JUDGE_BACKEND / MILLS_WEAVER_BACKEND selection ---

func newBackendTestFlexClient(t *testing.T) *clients.FlexInferClient {
	t.Helper()
	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.test"})
	if err != nil {
		t.Fatalf("flex client: %v", err)
	}
	return flex
}

// TestResolveMillsJudgeClient_DefaultKeepsFlexInfer pins the zero-behavior-
// change default: an unset (or "flexinfer") MILLS_JUDGE_BACKEND returns the
// FlexInfer client verbatim and an empty council-judge model (preserving the
// legacy weaver-model resolution).
func TestResolveMillsJudgeClient_DefaultKeepsFlexInfer(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	for _, backend := range []string{"", "flexinfer", "FlexInfer"} {
		cfg := Config{JudgeBackend: backend, LiteLLMProxyURL: "http://litellm.test", FlexInferJudgeModel: "or/kimi-k3"}
		got, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
		if got != flex {
			t.Errorf("backend %q: judge client = %p, want flex %p", backend, got, flex)
		}
		if model != "" {
			t.Errorf("backend %q: council judge model = %q, want empty", backend, model)
		}
	}
}

// TestResolveMillsJudgeClient_LiteLLMBindsGateway pins the litellm path: a
// litellm backend with a gateway + explicit model binds a distinct gateway
// client carrying that model, and returns it as the council judge model.
func TestResolveMillsJudgeClient_LiteLLMBindsGateway(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	cfg := Config{
		JudgeBackend:        "litellm",
		LiteLLMProxyURL:     "http://litellm.test",
		LiteLLMToken:        "k",
		FlexInferJudgeModel: "or/kimi-k3",
	}
	got, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
	if got == nil || got == flex {
		t.Fatalf("judge client = %p, want a distinct litellm client (flex %p)", got, flex)
	}
	if got.JudgeModel() != "or/kimi-k3" {
		t.Errorf("litellm judge model = %q, want or/kimi-k3", got.JudgeModel())
	}
	if model != "or/kimi-k3" {
		t.Errorf("council judge model = %q, want or/kimi-k3", model)
	}
}

// TestResolveMillsJudgeClient_LiteLLMMisconfiguredFallsBackLoud pins item 3:
// a litellm selection missing its gateway URL or its explicit model degrades
// to the FlexInfer judge (loud log) rather than binding an unroutable client.
func TestResolveMillsJudgeClient_LiteLLMMisconfiguredFallsBackLoud(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	cases := map[string]Config{
		"no gateway": {JudgeBackend: "litellm", FlexInferJudgeModel: "or/kimi-k3"},
		"no model":   {JudgeBackend: "litellm", LiteLLMProxyURL: "http://litellm.test"},
	}
	for name, cfg := range cases {
		got, model := resolveMillsJudgeClient(cfg, flex, discardLogger())
		if got != flex {
			t.Errorf("%s: judge client = %p, want flex fallback %p", name, got, flex)
		}
		if model != "" {
			t.Errorf("%s: council judge model = %q, want empty on fallback", name, model)
		}
	}
}

func TestResolveMillsWeaverClient_DefaultKeepsFlexInfer(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	for _, backend := range []string{"", "flexinfer"} {
		cfg := Config{WeaverBackend: backend, LiteLLMProxyURL: "http://litellm.test", FlexInferWeaverModel: "or/kimi-k3"}
		if got := resolveMillsWeaverClient(cfg, flex, discardLogger()); got != flex {
			t.Errorf("backend %q: weaver client = %p, want flex %p", backend, got, flex)
		}
	}
}

func TestResolveMillsWeaverClient_LiteLLMBindsGateway(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	cfg := Config{
		WeaverBackend:        "litellm",
		LiteLLMProxyURL:      "http://litellm.test",
		LiteLLMToken:         "k",
		FlexInferWeaverModel: "or/kimi-k3",
	}
	got := resolveMillsWeaverClient(cfg, flex, discardLogger())
	if got == nil || got == flex {
		t.Fatalf("weaver client = %p, want a distinct litellm client (flex %p)", got, flex)
	}
	if got.WeaverModel() != "or/kimi-k3" {
		t.Errorf("litellm weaver model = %q, want or/kimi-k3", got.WeaverModel())
	}
}

func TestResolveMillsWeaverClient_LiteLLMMisconfiguredFallsBackLoud(t *testing.T) {
	flex := newBackendTestFlexClient(t)
	cases := map[string]Config{
		"no gateway": {WeaverBackend: "litellm", FlexInferWeaverModel: "or/kimi-k3"},
		"no model":   {WeaverBackend: "litellm", LiteLLMProxyURL: "http://litellm.test"},
	}
	for name, cfg := range cases {
		if got := resolveMillsWeaverClient(cfg, flex, discardLogger()); got != flex {
			t.Errorf("%s: weaver client = %p, want flex fallback %p", name, got, flex)
		}
	}
}

// TestResolveMillsJudgeClient_LiteLLMWorksWithoutFlexInfer confirms the litellm
// judge binds even when the FlexInfer proxy is absent (flexClient nil), so a
// gateway-only deployment still gets LLM-judged gates.
func TestResolveMillsJudgeClient_LiteLLMWorksWithoutFlexInfer(t *testing.T) {
	cfg := Config{
		JudgeBackend:        "litellm",
		LiteLLMProxyURL:     "http://litellm.test",
		FlexInferJudgeModel: "or/kimi-k3",
	}
	got, model := resolveMillsJudgeClient(cfg, nil, discardLogger())
	if got == nil {
		t.Fatal("judge client nil; litellm should bind without a flexinfer proxy")
	}
	if model != "or/kimi-k3" {
		t.Errorf("council judge model = %q, want or/kimi-k3", model)
	}
}
