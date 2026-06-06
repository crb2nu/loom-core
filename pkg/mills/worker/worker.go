// Package worker defines the Layer-1 harness-neutral Worker contract for
// the Mills durable workflow engine.
//
// The contract exists so a future durable runtime (Layer 3) never has to
// import internal/hud or the mobile spawn API directly. It REUSES the
// fields already proven on pipeline.SpawnRequest / pipeline.SpawnResponse
// (the current harness-neutral boundary) and adds two pieces of
// first-class provenance the old boundary lost:
//
//   - AgentType: today inferred from the overloaded Model string via
//     agentTypeOrDefault. Layer 1 makes it an explicit, validated field so
//     a runtime declares the harness instead of smuggling it through Model.
//   - CostSource: cost provenance diverges by harness — Claude reports a
//     real SDK cost, Codex is estimated from a pricing table, Gemini is
//     unavailable. The old SpawnResponse carried only a float and dropped
//     the estimated/unavailable marker. WorkerResult surfaces it.
//
// This package is additive: it wraps the existing pipeline.SpawnWorker /
// SpawnClient via spawnClientAdapter (adapter.go) with byte-identical
// field mapping. No runtime behavior changes.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// derivedSpawnIDHexLen is the hex-char width of a derived spawn id body.
// It MUST stay in sync with internal/spawn.derivedSpawnIDHexLen so the id
// the worker layer derives for a key matches the one the spawn controller
// registers under. A parity test (idempotency_test.go) pins both to the
// same vectors.
const derivedSpawnIDHexLen = 12

// DeriveSpawnID returns the deterministic spawn id for an idempotency key.
// It MUST produce the byte-identical result of internal/spawn.deriveSpawnID
// so that a key sent on a create (idempotency_key) and the same key used to
// Resume map to the same spawn id. The worker package cannot import
// internal/spawn (that package drags in k8s client-go and internal/hud
// deps the harness-neutral operator should not carry), so the algorithm is
// mirrored here and locked by a shared-vector parity test.
func DeriveSpawnID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "spawn-" + hex.EncodeToString(sum[:])[:derivedSpawnIDHexLen]
}

// Sentinel errors returned by the spawn adapter.
var (
	// errNilClient is returned when a runner is invoked with no wrapped
	// client configured.
	errNilClient = errors.New("worker: spawn client not configured")
	// errNotResumable is returned when Resume is called on an adapter
	// whose wrapped client does not implement resume.
	errNotResumable = errors.New("worker: wrapped spawn client cannot resume")
)

// AgentType enumerates the spawn harnesses Mills can drive. The string
// values match the loom spawn AgentType vocabulary on the HUD mobile API
// (agent_type field) so the adapter maps through without translation.
const (
	AgentTypeClaudeCode = "claude-code"
	AgentTypeCodex      = "codex"
	AgentTypeGemini     = "gemini"
)

// WorkerRequest is the harness-neutral bundle a runtime hands to a
// WorkerRunner to start one subordinate agent run.
//
// Every field except AgentType and IdempotencyKey is REUSED verbatim from
// pipeline.SpawnRequest; the adapter copies them across one-for-one. The
// two new fields make provenance and replay first-class:
//
//   - AgentType is REQUIRED and validated (claude-code|codex|gemini). It
//     replaces the fragile "infer the harness from the Model string" path.
//   - IdempotencyKey is an OPTIONAL replay key (may be empty). As of Slice
//     2b it maps through pipeline.SpawnRequest to the HUD spawn client's
//     idempotency_key, so a non-empty key makes a duplicate spawn create a
//     deterministic AlreadyExists re-attach. A durable runtime also uses it
//     to resume a worker invocation via WorkerResumer. Empty preserves
//     legacy server-minted-id behavior.
type WorkerRequest struct {
	// ----- Fields reused verbatim from pipeline.SpawnRequest -----
	Prompt          string
	Model           string
	WorkingDir      string
	Env             map[string]string
	BudgetUSD       float64
	BudgetTurns     int
	BudgetMinutes   int
	ParentSessionID string
	BacklogID       string
	Project         string
	Branch          string
	BaseBranch      string
	Namespace       string
	Substrate       string

	// ----- Layer-1 additions -----

	// AgentType is the spawn harness to run. REQUIRED. Validated against
	// the claude-code|codex|gemini set by ValidateAgentType. The adapter
	// rejects an invalid value rather than silently defaulting.
	AgentType string

	// IdempotencyKey is an optional caller-supplied replay key. Empty is
	// allowed (the common case today). A durable runtime sets it so a
	// re-driven step is deduped/resumed. As of Slice 2b it maps through
	// pipeline.SpawnRequest -> the HUD spawn client's idempotency_key: a
	// non-empty key makes the spawn controller derive a deterministic id
	// and turn a duplicate create into an AlreadyExists re-attach (no
	// second pod). WorkerResumer.Resume also keys off it. Empty keeps the
	// legacy server-minted-id path byte-identical.
	IdempotencyKey string
}

// CostSource records the provenance of WorkerResult.CostUSD so downstream
// accounting can distinguish a real SDK-reported cost from a Loom-side
// estimate or an unavailable signal.
type CostSource int

const (
	// CostSourceUnknown is the zero value. The adapter never emits it for
	// a terminal spawn; it exists so a partially-built WorkerResult is
	// not mistaken for "real cost".
	CostSourceUnknown CostSource = iota
	// CostSourceReal means the harness reported an authoritative SDK cost
	// (Claude's result event total_cost_usd).
	CostSourceReal
	// CostSourceEstimated means Loom computed the cost from a pricing
	// table because the harness does not report it (Codex).
	CostSourceEstimated
	// CostSourceUnavailable means no cost is known and none was estimated
	// (Gemini reports zero cost with no pricing model).
	CostSourceUnavailable
)

// String returns the wire/log token for a CostSource.
func (c CostSource) String() string {
	switch c {
	case CostSourceReal:
		return "real"
	case CostSourceEstimated:
		return "estimated"
	case CostSourceUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// WorkerResult is the harness-neutral bundle a WorkerRunner returns.
//
// The first block of fields is REUSED verbatim from
// pipeline.SpawnResponse. CostSource and Telemetry are the Layer-1
// additions that carry provenance the old SpawnResponse dropped.
type WorkerResult struct {
	// ----- Fields reused verbatim from pipeline.SpawnResponse -----
	SpawnID        string
	CostUSD        float64
	LogTail        string
	FilesChanged   []string
	LinesAdded     int
	LinesRemoved   int
	DiffPatch      []byte
	CommitMessages []string
	Artifacts      map[string]any

	// ----- Layer-1 additions -----

	// CostSource is the provenance of CostUSD (real|estimated|unavailable).
	CostSource CostSource

	// Telemetry is an optional snapshot of the harness-reported telemetry
	// the cost provenance was derived from. Nil when the harness returned
	// no telemetry (e.g. a spawn that failed before its first turn).
	Telemetry *TelemetrySnapshot
}

// TelemetrySnapshot is the harness-neutral subset of spawn telemetry a
// runtime may want for accounting/audit. It is a value snapshot — the
// adapter fills it from whatever the underlying client surfaced — so the
// runtime never imports internal/hud's bridge.SpawnTelemetry.
type TelemetrySnapshot struct {
	TurnCount    int
	TotalCostUSD float64
	// CostEstimated mirrors bridge.SpawnTelemetry.CostEstimated: true when
	// TotalCostUSD is a Loom-side estimate (Codex). Drives CostSource.
	CostEstimated bool
	StopReason    string
	LastMessage   string
}

// WorkerRunner is the Layer-1 entry point: run one agent invocation to a
// terminal result. Implementations live behind this interface so the
// runtime is harness-neutral. spawnClientAdapter (adapter.go) wraps the
// existing pipeline.SpawnClient to satisfy it.
type WorkerRunner interface {
	Run(ctx context.Context, req WorkerRequest) (WorkerResult, error)
}

// WorkerResumer is implemented by runners that can re-attach to an
// already-started invocation after a restart, keyed by the request's
// IdempotencyKey. Optional: a runner that cannot resume simply does not
// implement it.
type WorkerResumer interface {
	Resume(ctx context.Context, idempotencyKey string) (WorkerResult, error)
}

// Capabilities describes what a given harness can do. A runtime consults
// it before planning multi-turn work or trusting a cost figure.
type Capabilities struct {
	// SupportsRealCost is true when the harness reports an authoritative
	// SDK cost (vs. an estimate or nothing).
	SupportsRealCost bool
	// SupportsMultiTurn is true when the harness can accept follow-up
	// turns on a running spawn.
	SupportsMultiTurn bool
	// SupportsStreaming is true when the harness streams incremental
	// telemetry/events during a run.
	SupportsStreaming bool
}

// CapabilitiesFor returns the capability matrix for an agent type. The
// values are grounded in the current spawn parsers' behavior:
//
//   - claude-code: real cost (result event), multi-turn, streaming.
//   - codex: estimated cost (AddEstimatedCost pricing table), NO
//     multi-turn yet, streaming.
//   - gemini: unavailable cost (SetResult(0,0,...)), NO multi-turn,
//     streaming.
//
// An unknown agent type returns the zero Capabilities and ok=false.
func CapabilitiesFor(agentType string) (Capabilities, bool) {
	switch normalizeAgentType(agentType) {
	case AgentTypeClaudeCode:
		return Capabilities{SupportsRealCost: true, SupportsMultiTurn: true, SupportsStreaming: true}, true
	case AgentTypeCodex:
		return Capabilities{SupportsRealCost: false, SupportsMultiTurn: false, SupportsStreaming: true}, true
	case AgentTypeGemini:
		return Capabilities{SupportsRealCost: false, SupportsMultiTurn: false, SupportsStreaming: true}, true
	default:
		return Capabilities{}, false
	}
}

// ValidateAgentType normalizes an agent type and returns the canonical
// form. It accepts the canonical tokens plus the common shorthands the
// old agentTypeOrDefault tolerated (claude, claude-sonnet, openai-codex,
// …). Empty or unrecognized input returns an error so an unknown harness
// is rejected rather than silently defaulting.
func ValidateAgentType(agentType string) (string, error) {
	if strings.TrimSpace(agentType) == "" {
		return "", fmt.Errorf("worker: agent type is required")
	}
	canon := normalizeAgentType(agentType)
	switch canon {
	case AgentTypeClaudeCode, AgentTypeCodex, AgentTypeGemini:
		return canon, nil
	default:
		return "", fmt.Errorf("worker: unknown agent type %q (want one of claude-code, codex, gemini)", agentType)
	}
}

// normalizeAgentType maps shorthands to the canonical AgentType token.
// It mirrors agentTypeOrDefault's known mappings but, unlike that
// function, returns the input unchanged (lowercased) for anything it
// does not recognize so the caller can reject it.
func normalizeAgentType(agentType string) string {
	switch strings.ToLower(strings.TrimSpace(agentType)) {
	case "claude", "claude-code", "claude-sonnet", "claude-opus":
		return AgentTypeClaudeCode
	case "codex", "openai-codex":
		return AgentTypeCodex
	case "gemini":
		return AgentTypeGemini
	default:
		return strings.ToLower(strings.TrimSpace(agentType))
	}
}

// costSourceFromTelemetry derives provenance from a telemetry snapshot.
// The rules mirror the spawn parsers:
//
//   - CostEstimated set            → estimated (Codex's AddEstimatedCost).
//   - cost > 0 and not estimated   → real (Claude's SDK total_cost_usd).
//   - cost == 0 and not estimated  → unavailable (Gemini SetResult(0,...)).
//
// A nil snapshot means no telemetry was returned → unavailable.
func costSourceFromTelemetry(tel *TelemetrySnapshot) CostSource {
	if tel == nil {
		return CostSourceUnavailable
	}
	if tel.CostEstimated {
		return CostSourceEstimated
	}
	if tel.TotalCostUSD > 0 {
		return CostSourceReal
	}
	return CostSourceUnavailable
}
