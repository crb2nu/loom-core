package hud

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/internal/hud/autofix"
	domainalerting "github.com/crb2nu/loom/internal/hud/domain/alerting"
)

func newAutofixTestApp(cfg Config) *App {
	return &App{
		config: cfg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestInitAutofixEngine_DisabledByDefault(t *testing.T) {
	app := newAutofixTestApp(Config{FlexInferURL: "http://127.0.0.1:1"})
	app.initAutofixEngine()
	if app.autofixEngine != nil {
		t.Fatal("expected no autofix engine when AutofixEnabled is false")
	}
}

func TestInitAutofixEngine_NoLLMBackend(t *testing.T) {
	app := newAutofixTestApp(Config{AutofixEnabled: true})
	app.initAutofixEngine()
	if app.autofixEngine != nil {
		t.Fatal("expected no autofix engine without coordinator or FlexInferURL")
	}
}

func TestInitAutofixEngine_DedicatedClient(t *testing.T) {
	app := newAutofixTestApp(Config{
		AutofixEnabled: true,
		FlexInferURL:   "http://127.0.0.1:1",
	})
	app.initAutofixEngine()
	if app.autofixEngine == nil {
		t.Fatal("expected autofix engine with FlexInferURL configured")
	}
}

func TestInitAutofixEngine_CoordinatorClient(t *testing.T) {
	app := newAutofixTestApp(Config{AutofixEnabled: true})
	app.coordinator = newPassiveCoordinatorForTest()
	app.initAutofixEngine()
	if app.autofixEngine == nil {
		t.Fatal("expected autofix engine sharing the coordinator's LLM client")
	}
}

// TestAutofixRoutes_RealEngine proves the /api/autofix routes serve real data
// from a wired engine (not the honest-empty fallback) through the production
// alertingDepsAdapter.
func TestAutofixRoutes_RealEngine(t *testing.T) {
	app := newAutofixTestApp(Config{
		AutofixEnabled: true,
		FlexInferURL:   "http://127.0.0.1:1",
		AdminToken:     "test-admin",
	})
	app.initAutofixEngine()
	if app.autofixEngine == nil {
		t.Fatal("expected autofix engine")
	}

	// Seed a proposal through the engine's real path.
	proposal, err := app.autofixEngine.ProposeAutoFix(autofix.Diagnosis{
		PipelineID:   42,
		Project:      "loom-core",
		RootCause:    "flaky runner",
		Category:     "infra",
		SuggestedFix: "retry pipeline",
		Confidence:   0.9,
	})
	if err != nil {
		t.Fatalf("ProposeAutoFix: %v", err)
	}

	mux := http.NewServeMux()
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	domainalerting.New(&alertingDepsAdapter{app: app}).RegisterRoutes(mux, identity)

	// List proposals — must contain the seeded proposal, not the empty fallback.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/autofix/proposals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list proposals: status %d: %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Proposals []autofix.AutoFixProposal `json:"proposals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal proposals: %v", err)
	}
	if len(listResp.Proposals) != 1 || listResp.Proposals[0].ID != proposal.ID {
		t.Fatalf("expected seeded proposal %q, got %+v", proposal.ID, listResp.Proposals)
	}

	// Reject the proposal, then verify the execution record surfaces.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/autofix/proposals/"+proposal.ID+"/reject", nil)
	req.Header.Set("X-Admin-Token", "test-admin")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject proposal: status %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/autofix/executions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list executions: status %d: %s", rec.Code, rec.Body.String())
	}
	var execResp struct {
		Executions []autofix.AutoFixExecution `json:"executions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &execResp); err != nil {
		t.Fatalf("unmarshal executions: %v", err)
	}
	if len(execResp.Executions) != 1 || execResp.Executions[0].Status != "rejected" {
		t.Fatalf("expected one rejected execution, got %+v", execResp.Executions)
	}
}
