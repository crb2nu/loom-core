package worker

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// fakeSpawnClient records the request it received and returns a canned
// response/error. It optionally implements pipeline.SpawnResumeClient.
type fakeSpawnClient struct {
	gotReq  pipeline.SpawnRequest
	resp    pipeline.SpawnResponse
	err     error
	resumed string
}

func (f *fakeSpawnClient) Run(_ context.Context, req pipeline.SpawnRequest) (pipeline.SpawnResponse, error) {
	f.gotReq = req
	return f.resp, f.err
}

// resumableFakeSpawnClient embeds the fake and adds Resume so the adapter
// promotes it to a WorkerResumer.
type resumableFakeSpawnClient struct {
	fakeSpawnClient
}

func (f *resumableFakeSpawnClient) Resume(_ context.Context, spawnID string) (pipeline.SpawnResponse, error) {
	f.resumed = spawnID
	return f.resp, f.err
}

func sampleWorkerRequest() WorkerRequest {
	return WorkerRequest{
		Prompt:                "plan slices for BL-X",
		Model:                 "claude-code",
		WorkingDir:            "/wt/BL-X",
		Env:                   map[string]string{"LOOM_MILLS_RUN_ID": "PIPE-X-1"},
		BudgetUSD:             2.0,
		BudgetTurns:           50,
		BudgetMinutes:         30,
		ParentSessionID:       "session-op-1",
		BacklogID:             "BL-X",
		Project:               "loom-core",
		Branch:                "mills/BL-X/plan_slice",
		BaseBranch:            "main",
		Namespace:             "loom-mills",
		Substrate:             "k8s",
		CompletionHoldSeconds: 90,
		AgentType:             AgentTypeClaudeCode,
		IdempotencyKey:        "idem-1",
	}
}

// TestWorkerRequestRoundTripFieldParity asserts every REUSED field
// survives WorkerRequest -> SpawnRequest -> WorkerRequest unchanged.
func TestWorkerRequestRoundTripFieldParity(t *testing.T) {
	orig := sampleWorkerRequest()
	sr := orig.ToSpawnRequest()
	back := FromSpawnRequest(sr)

	// IdempotencyKey now maps through SpawnRequest (Slice 2b) and must
	// round-trip alongside every reused field.
	if back.IdempotencyKey != orig.IdempotencyKey {
		t.Errorf("IdempotencyKey: got %q want %q", back.IdempotencyKey, orig.IdempotencyKey)
	}
	if sr.IdempotencyKey != orig.IdempotencyKey {
		t.Errorf("SpawnRequest.IdempotencyKey: got %q want %q", sr.IdempotencyKey, orig.IdempotencyKey)
	}
	if back.Prompt != orig.Prompt {
		t.Errorf("Prompt: got %q want %q", back.Prompt, orig.Prompt)
	}
	if back.Model != orig.Model {
		t.Errorf("Model: got %q want %q", back.Model, orig.Model)
	}
	if back.WorkingDir != orig.WorkingDir {
		t.Errorf("WorkingDir: got %q want %q", back.WorkingDir, orig.WorkingDir)
	}
	if !reflect.DeepEqual(back.Env, orig.Env) {
		t.Errorf("Env: got %v want %v", back.Env, orig.Env)
	}
	if back.BudgetUSD != orig.BudgetUSD {
		t.Errorf("BudgetUSD: got %v want %v", back.BudgetUSD, orig.BudgetUSD)
	}
	if back.BudgetTurns != orig.BudgetTurns {
		t.Errorf("BudgetTurns: got %v want %v", back.BudgetTurns, orig.BudgetTurns)
	}
	if back.BudgetMinutes != orig.BudgetMinutes {
		t.Errorf("BudgetMinutes: got %v want %v", back.BudgetMinutes, orig.BudgetMinutes)
	}
	if back.ParentSessionID != orig.ParentSessionID {
		t.Errorf("ParentSessionID: got %q want %q", back.ParentSessionID, orig.ParentSessionID)
	}
	if back.BacklogID != orig.BacklogID {
		t.Errorf("BacklogID: got %q want %q", back.BacklogID, orig.BacklogID)
	}
	if back.Project != orig.Project {
		t.Errorf("Project: got %q want %q", back.Project, orig.Project)
	}
	if back.Branch != orig.Branch {
		t.Errorf("Branch: got %q want %q", back.Branch, orig.Branch)
	}
	if back.BaseBranch != orig.BaseBranch {
		t.Errorf("BaseBranch: got %q want %q", back.BaseBranch, orig.BaseBranch)
	}
	if back.Namespace != orig.Namespace {
		t.Errorf("Namespace: got %q want %q", back.Namespace, orig.Namespace)
	}
	if back.Substrate != orig.Substrate {
		t.Errorf("Substrate: got %q want %q", back.Substrate, orig.Substrate)
	}
	if back.CompletionHoldSeconds != orig.CompletionHoldSeconds {
		t.Errorf("CompletionHoldSeconds: got %d want %d", back.CompletionHoldSeconds, orig.CompletionHoldSeconds)
	}
	if sr.CompletionHoldSeconds != orig.CompletionHoldSeconds {
		t.Errorf("SpawnRequest.CompletionHoldSeconds: got %d want %d", sr.CompletionHoldSeconds, orig.CompletionHoldSeconds)
	}
	if back.AgentType != orig.AgentType {
		t.Errorf("AgentType: got %q want %q", back.AgentType, orig.AgentType)
	}
}

// TestWorkerRequestRoundTripAllAgentTypes verifies parity for every
// harness, including the AgentType-only (Model-empty) form where the
// adapter must reconstruct AgentType from the SpawnRequest Model it set.
func TestWorkerRequestRoundTripAllAgentTypes(t *testing.T) {
	for _, at := range []string{AgentTypeClaudeCode, AgentTypeCodex, AgentTypeGemini} {
		// Model explicitly set to canonical token.
		orig := sampleWorkerRequest()
		orig.AgentType = at
		orig.Model = at
		back := FromSpawnRequest(orig.ToSpawnRequest())
		if back.AgentType != at {
			t.Errorf("explicit model %q: AgentType round-trip got %q", at, back.AgentType)
		}

		// Model empty: adapter sets SpawnRequest.Model from AgentType so
		// the harness is recoverable.
		origEmpty := sampleWorkerRequest()
		origEmpty.AgentType = at
		origEmpty.Model = ""
		sr := origEmpty.ToSpawnRequest()
		if sr.Model != at {
			t.Errorf("empty model %q: SpawnRequest.Model = %q, want %q", at, sr.Model, at)
		}
		back = FromSpawnRequest(sr)
		if back.AgentType != at {
			t.Errorf("empty model %q: AgentType round-trip got %q", at, back.AgentType)
		}
	}
}

// TestRunMapsRequestFieldsByteIdentical asserts the adapter forwards every
// SpawnRequest field exactly across the worker boundary.
func TestRunMapsRequestFieldsByteIdentical(t *testing.T) {
	fake := &fakeSpawnClient{resp: pipeline.SpawnResponse{SpawnID: "s1", CostUSD: 1.0}}
	runner := NewSpawnRunner(fake)

	req := sampleWorkerRequest()
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := pipeline.SpawnRequest{
		Prompt:                req.Prompt,
		WorkingDir:            req.WorkingDir,
		Model:                 req.Model,
		Env:                   req.Env,
		BudgetUSD:             req.BudgetUSD,
		BudgetTurns:           req.BudgetTurns,
		BudgetMinutes:         req.BudgetMinutes,
		ParentSessionID:       req.ParentSessionID,
		BacklogID:             req.BacklogID,
		Project:               req.Project,
		Branch:                req.Branch,
		BaseBranch:            req.BaseBranch,
		Namespace:             req.Namespace,
		Substrate:             req.Substrate,
		CompletionHoldSeconds: req.CompletionHoldSeconds,
		IdempotencyKey:        req.IdempotencyKey,
	}
	if !reflect.DeepEqual(fake.gotReq, want) {
		t.Errorf("forwarded SpawnRequest mismatch:\n got %+v\nwant %+v", fake.gotReq, want)
	}
}

// TestRunRejectsInvalidAgentType ensures the adapter never reaches the
// spawn client with an unknown harness.
func TestRunRejectsInvalidAgentType(t *testing.T) {
	fake := &fakeSpawnClient{}
	runner := NewSpawnRunner(fake)
	req := sampleWorkerRequest()
	req.AgentType = "gpt-5.5"
	if _, err := runner.Run(context.Background(), req); err == nil {
		t.Fatal("expected error for invalid agent type")
	}
	if fake.gotReq.Prompt != "" {
		t.Error("spawn client should not be invoked for invalid agent type")
	}
}

// TestCostSourceSurvivesForAllHarnesses is the provenance kill-test: each
// harness's distinct (cost, estimated) signature maps to the right
// CostSource through the adapter.
func TestCostSourceSurvivesForAllHarnesses(t *testing.T) {
	cases := []struct {
		name       string
		agentType  string
		resp       pipeline.SpawnResponse
		wantSource CostSource
		wantCost   float64
	}{
		{
			name:       "claude real cost",
			agentType:  AgentTypeClaudeCode,
			resp:       pipeline.SpawnResponse{SpawnID: "c1", CostUSD: 1.23, CostEstimated: false},
			wantSource: CostSourceReal,
			wantCost:   1.23,
		},
		{
			name:       "codex estimated cost",
			agentType:  AgentTypeCodex,
			resp:       pipeline.SpawnResponse{SpawnID: "x1", CostUSD: 0.42, CostEstimated: true},
			wantSource: CostSourceEstimated,
			wantCost:   0.42,
		},
		{
			name:       "gemini unavailable cost",
			agentType:  AgentTypeGemini,
			resp:       pipeline.SpawnResponse{SpawnID: "g1", CostUSD: 0, CostEstimated: false},
			wantSource: CostSourceUnavailable,
			wantCost:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeSpawnClient{resp: c.resp}
			runner := NewSpawnRunner(fake)
			req := sampleWorkerRequest()
			req.AgentType = c.agentType
			req.Model = c.agentType

			got, err := runner.Run(context.Background(), req)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.CostSource != c.wantSource {
				t.Errorf("CostSource = %v, want %v", got.CostSource, c.wantSource)
			}
			// Provenance must NOT alter the cost value.
			if got.CostUSD != c.wantCost {
				t.Errorf("CostUSD = %v, want %v (value must be unchanged)", got.CostUSD, c.wantCost)
			}
			if got.Telemetry == nil {
				t.Fatal("expected telemetry snapshot")
			}
			if got.Telemetry.TotalCostUSD != c.wantCost {
				t.Errorf("Telemetry.TotalCostUSD = %v, want %v", got.Telemetry.TotalCostUSD, c.wantCost)
			}
			if got.Telemetry.CostEstimated != c.resp.CostEstimated {
				t.Errorf("Telemetry.CostEstimated = %v, want %v", got.Telemetry.CostEstimated, c.resp.CostEstimated)
			}
		})
	}
}

// TestResultMapsResponseFieldsByteIdentical asserts the reused response
// fields map across unchanged.
func TestResultMapsResponseFieldsByteIdentical(t *testing.T) {
	resp := pipeline.SpawnResponse{
		SpawnID:        "s9",
		CostUSD:        0.7,
		LogTail:        "tail",
		FilesChanged:   []string{"a.go", "b.go"},
		LinesAdded:     10,
		LinesRemoved:   3,
		DiffPatch:      []byte("diff --git"),
		CommitMessages: []string{"feat: x"},
		Artifacts:      map[string]any{"k": "v"},
	}
	fake := &fakeSpawnClient{resp: resp}
	runner := NewSpawnRunner(fake)
	req := sampleWorkerRequest()

	got, err := runner.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.SpawnID != resp.SpawnID {
		t.Errorf("SpawnID: got %q want %q", got.SpawnID, resp.SpawnID)
	}
	if got.LogTail != resp.LogTail {
		t.Errorf("LogTail: got %q want %q", got.LogTail, resp.LogTail)
	}
	if !reflect.DeepEqual(got.FilesChanged, resp.FilesChanged) {
		t.Errorf("FilesChanged: got %v want %v", got.FilesChanged, resp.FilesChanged)
	}
	if got.LinesAdded != resp.LinesAdded || got.LinesRemoved != resp.LinesRemoved {
		t.Errorf("Lines: got +%d/-%d want +%d/-%d", got.LinesAdded, got.LinesRemoved, resp.LinesAdded, resp.LinesRemoved)
	}
	if !bytes.Equal(got.DiffPatch, resp.DiffPatch) {
		t.Errorf("DiffPatch: got %q want %q", got.DiffPatch, resp.DiffPatch)
	}
	if !reflect.DeepEqual(got.CommitMessages, resp.CommitMessages) {
		t.Errorf("CommitMessages: got %v want %v", got.CommitMessages, resp.CommitMessages)
	}
	if !reflect.DeepEqual(got.Artifacts, resp.Artifacts) {
		t.Errorf("Artifacts: got %v want %v", got.Artifacts, resp.Artifacts)
	}
}

// TestRunMapsResponseOnError verifies partial telemetry survives a
// terminal error, matching SpawnWorker.Run's behavior.
func TestRunMapsResponseOnError(t *testing.T) {
	fake := &fakeSpawnClient{
		resp: pipeline.SpawnResponse{SpawnID: "s-fail", CostUSD: 0.5},
		err:  errors.New("spawn failed"),
	}
	runner := NewSpawnRunner(fake)
	got, err := runner.Run(context.Background(), sampleWorkerRequest())
	if err == nil {
		t.Fatal("expected error")
	}
	if got.SpawnID != "s-fail" || got.CostUSD != 0.5 {
		t.Errorf("partial result not mapped on error: %+v", got)
	}
}

// TestResumeWhenClientSupportsIt verifies the adapter promotes a
// resume-capable client to WorkerResumer and re-attaches by the
// DETERMINISTIC spawn id derived from the idempotency key (Slice 2b), so
// Resume(key) lands on the same spawn the matching idempotent create made.
func TestResumeWhenClientSupportsIt(t *testing.T) {
	const key = "mills/run-1/stage-implement"
	wantSpawnID := DeriveSpawnID(key)

	fake := &resumableFakeSpawnClient{
		fakeSpawnClient: fakeSpawnClient{resp: pipeline.SpawnResponse{SpawnID: wantSpawnID, CostUSD: 0.11}},
	}
	runner := NewSpawnRunner(fake)
	resumer, ok := runner.(WorkerResumer)
	if !ok {
		t.Fatal("expected runner to implement WorkerResumer")
	}
	got, err := resumer.Resume(context.Background(), key)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// The underlying spawn-id resumer must receive the DERIVED id, not the
	// raw key — that's what re-attaches to the idempotent create's spawn.
	if fake.resumed != wantSpawnID {
		t.Errorf("forwarded resume id = %q, want derived %q", fake.resumed, wantSpawnID)
	}
	if got.SpawnID != wantSpawnID || got.CostUSD != 0.11 {
		t.Errorf("resume result: %+v", got)
	}
}

// TestNonResumableClientIsNotResumer verifies a plain SpawnClient adapter
// does not advertise WorkerResumer.
func TestNonResumableClientIsNotResumer(t *testing.T) {
	runner := NewSpawnRunner(&fakeSpawnClient{})
	if _, ok := runner.(WorkerResumer); ok {
		t.Error("non-resumable client should not satisfy WorkerResumer")
	}
}

// TestNilClientErrors guards the defensive nil paths.
func TestNilClientErrors(t *testing.T) {
	a := &spawnClientAdapter{}
	if _, err := a.Run(context.Background(), sampleWorkerRequest()); err == nil {
		t.Error("expected error for nil client")
	}
}
