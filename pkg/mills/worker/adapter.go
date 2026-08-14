package worker

import (
	"context"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// ErrSpawnTerminalFailure re-exports the pipeline sentinel so a Layer-3
// runtime that depends only on the worker package (never on pipeline or
// internal/hud) can still detect that a spawn reached a terminal
// non-completed status via errors.Is. Same variable, not a copy — Is
// matching works across both packages.
var ErrSpawnTerminalFailure = pipeline.ErrSpawnTerminalFailure

// spawnClientAdapter wraps an existing pipeline.SpawnClient as a
// WorkerRunner. It does NOT reimplement the spawn flow — it maps
// WorkerRequest -> pipeline.SpawnRequest, delegates to the wrapped
// client, then maps pipeline.SpawnResponse -> WorkerResult. The field
// mapping is byte-identical for every reused field; the adapter adds only
// the derived CostSource + TelemetrySnapshot on the way back.
type spawnClientAdapter struct {
	client pipeline.SpawnClient
}

// NewSpawnRunner adapts a pipeline.SpawnClient (e.g. clients.HUDSpawnClient)
// into a WorkerRunner. The returned runner also satisfies WorkerResumer
// when the wrapped client implements pipeline.SpawnResumeClient.
//
// This is the seam that lets a future Layer-3 runtime depend only on the
// worker package: it receives a WorkerRunner and never imports
// internal/hud or the mobile spawn API.
func NewSpawnRunner(client pipeline.SpawnClient) WorkerRunner {
	a := &spawnClientAdapter{client: client}
	if _, ok := client.(pipeline.SpawnResumeClient); ok {
		// Promote to the resumable wrapper so a type assertion to
		// WorkerResumer succeeds for resume-capable clients.
		return &resumableSpawnAdapter{spawnClientAdapter: a}
	}
	return a
}

// Run satisfies WorkerRunner.
func (a *spawnClientAdapter) Run(ctx context.Context, req WorkerRequest) (WorkerResult, error) {
	if a == nil || a.client == nil {
		return WorkerResult{}, errNilClient
	}
	if _, err := ValidateAgentType(req.AgentType); err != nil {
		return WorkerResult{}, err
	}
	resp, err := a.client.Run(ctx, workerRequestToSpawnRequest(req))
	// Map the response even on error: the spawn flow returns partial
	// telemetry (cost, spawn id) alongside a terminal error, and the
	// runner records it for audit. This mirrors SpawnWorker.Run, which
	// converts resp -> StageOutput regardless of err.
	return spawnResponseToWorkerResult(resp), err
}

// resumableSpawnAdapter is the WorkerResumer-capable wrapper returned by
// NewSpawnRunner when the underlying client can resume.
type resumableSpawnAdapter struct {
	*spawnClientAdapter
}

// Resume satisfies WorkerResumer. Slice 2b: the argument is an
// idempotency key, which the spawn controller turns into a deterministic
// spawn id via the same derivation. We map key -> derived spawn id here
// (DeriveSpawnID) so the underlying pipeline.SpawnResumeClient.Resume —
// which re-attaches by spawn id — lands on the exact spawn the matching
// idempotent create produced. The mapping is a no-op for an empty key
// (the resumer then errors on the empty id), preserving the prior
// contract where Resume was only meaningful with a real handle.
func (a *resumableSpawnAdapter) Resume(ctx context.Context, idempotencyKey string) (WorkerResult, error) {
	if a == nil || a.client == nil {
		return WorkerResult{}, errNilClient
	}
	resumer, ok := a.client.(pipeline.SpawnResumeClient)
	if !ok {
		return WorkerResult{}, errNotResumable
	}
	spawnID := idempotencyKey
	if idempotencyKey != "" {
		spawnID = DeriveSpawnID(idempotencyKey)
	}
	resp, err := resumer.Resume(ctx, spawnID)
	return spawnResponseToWorkerResult(resp), err
}

// workerRequestToSpawnRequest maps every reused field one-for-one. The
// AgentType field has no SpawnRequest home and is carried via Model so the
// existing agentTypeOrDefault path resolves it to the same harness.
//
// IdempotencyKey now has a SpawnRequest home: it is mapped through
// one-for-one (Slice 2b) so a durable runtime's replay key reaches the HUD
// spawn client, which forwards it as idempotency_key. Empty stays empty,
// preserving legacy behavior for every current caller.
func workerRequestToSpawnRequest(req WorkerRequest) pipeline.SpawnRequest {
	model := req.Model
	if model == "" {
		// When the caller declared an AgentType but left Model empty, set
		// Model to the canonical agent type so the spawn client's
		// agentTypeOrDefault resolves the SAME harness the caller asked
		// for. With both empty the legacy default (claude-code) applies —
		// identical to pre-Layer-1 behavior.
		if canon, err := ValidateAgentType(req.AgentType); err == nil {
			model = canon
		}
	}
	return pipeline.SpawnRequest{
		Prompt:                req.Prompt,
		WorkingDir:            req.WorkingDir,
		Model:                 model,
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
}

// spawnResponseToWorkerResult maps every reused field one-for-one and
// derives the Layer-1 provenance (CostSource + TelemetrySnapshot) from
// the response's CostUSD + CostEstimated marker.
func spawnResponseToWorkerResult(resp pipeline.SpawnResponse) WorkerResult {
	tel := &TelemetrySnapshot{
		TotalCostUSD:  resp.CostUSD,
		CostEstimated: resp.CostEstimated,
	}
	return WorkerResult{
		SpawnID:        resp.SpawnID,
		CostUSD:        resp.CostUSD,
		LogTail:        resp.LogTail,
		FilesChanged:   resp.FilesChanged,
		LinesAdded:     resp.LinesAdded,
		LinesRemoved:   resp.LinesRemoved,
		DiffPatch:      resp.DiffPatch,
		CommitMessages: resp.CommitMessages,
		Artifacts:      resp.Artifacts,
		CostSource:     costSourceFromTelemetry(tel),
		Telemetry:      tel,
	}
}

// ToSpawnRequest exposes the WorkerRequest -> pipeline.SpawnRequest
// mapping for callers (and tests) that need the wire-level request
// without going through a runner.
func (req WorkerRequest) ToSpawnRequest() pipeline.SpawnRequest {
	return workerRequestToSpawnRequest(req)
}

// FromSpawnRequest reconstructs a WorkerRequest from a
// pipeline.SpawnRequest. The agent type is resolved from the SpawnRequest
// Model via the same normalization the spawn client uses, so a
// WorkerRequest -> SpawnRequest -> WorkerRequest round-trip preserves
// every reused field — including IdempotencyKey, which now maps through
// SpawnRequest (Slice 2b).
func FromSpawnRequest(sr pipeline.SpawnRequest) WorkerRequest {
	agentType := sr.Model
	if canon, err := ValidateAgentType(sr.Model); err == nil {
		agentType = canon
	}
	return WorkerRequest{
		Prompt:                sr.Prompt,
		Model:                 sr.Model,
		WorkingDir:            sr.WorkingDir,
		Env:                   sr.Env,
		BudgetUSD:             sr.BudgetUSD,
		BudgetTurns:           sr.BudgetTurns,
		BudgetMinutes:         sr.BudgetMinutes,
		ParentSessionID:       sr.ParentSessionID,
		BacklogID:             sr.BacklogID,
		Project:               sr.Project,
		Branch:                sr.Branch,
		BaseBranch:            sr.BaseBranch,
		Namespace:             sr.Namespace,
		Substrate:             sr.Substrate,
		CompletionHoldSeconds: sr.CompletionHoldSeconds,
		AgentType:             agentType,
		IdempotencyKey:        sr.IdempotencyKey,
	}
}
