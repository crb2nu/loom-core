// Package gates is the deterministic check library for Loom Mills's pipeline.
//
// Every auto_gate node in the mills-default-pipeline template (see
// cmd/mcp-mentatlab/templates/mills-default-pipeline.yaml) resolves to one or
// more gate evaluations from this package. Each Gate is a pure function over
// the StageInput it sees — diff content, scope sidecar, item policy — and
// returns a deterministic Outcome. LLM-judged gates (spec_conformance,
// pr_self_review) live alongside but invoke FlexInfer; they share the same
// Gate interface so the reconciler doesn't care.
//
// The library is intentionally pure-Go and free of network I/O so the
// reconciler can evaluate gates without leaving the operator pod and so
// tests run as table cases without fixtures.
package gates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Outcome is the verdict of one gate evaluation. It mirrors the on-disk
// store.GateOutcome so the reconciler can persist it directly.
type Outcome struct {
	Pass    bool
	Reasons []string
	// JudgedBy identifies the evaluator: "go" for pure-Go gates, or
	// "flexinfer:<model>" for LLM-judged gates. Persisted into
	// gate_outcomes.judged_by for audit.
	JudgedBy string
	// Terminal marks a failure that no upstream-stage retry can change:
	// the verdict is a function of the backlog item's own state (e.g. a
	// slice-less item failing scope), not of what the worker produced.
	// The runner escalates a terminal gate failure immediately instead of
	// burning the RetryFrom attempt budget re-running a stage that cannot
	// alter the outcome (escalations #272–#278 each spent 3 implement
	// attempts on the same deterministic no-slices fail). Meaningless on
	// a pass.
	Terminal bool
	// Skip marks a gate as not-applicable rather than passed or failed: the
	// check had no basis to run against the item's state (e.g. a slice-less
	// item has no scope to enforce), so the verdict is advisory. Pass stays
	// true so the aggregate verdict and the pipeline both proceed, but the
	// runner persists gate_outcomes.outcome='skip' and gate_pass_rate excludes
	// it (a skip is neither a pass nor a fail — see kpi_writer). Meaningless
	// unless Pass is true.
	Skip bool
	// Judgements carries the numeric verdicts behind an LLM-judged gate.
	// The gate itself only needs the boolean, but a factory that grades its
	// own work has to be able to ask later whether the grades tracked
	// reality, and the score exists nowhere else: gate_outcomes has no score
	// column and the pass path never even renders it into Reasons. Empty for
	// pure-Go gates and for LLM gates that returned without a score
	// (disabled, canary-skipped, unparseable envelope).
	Judgements []Judgement
}

// FailureCategory is the bounded taxonomy attached where gate outcomes
// originate. It aliases telemetry's wire type so no free-form category can
// cross the evaluation boundary.
type FailureCategory = telemetry.GateFailureCategory

const (
	FailureCategoryFail                = telemetry.GateFailureCategoryFail
	FailureCategoryUnknownGate         = telemetry.GateFailureCategoryUnknownGate
	FailureCategoryInfrastructureError = telemetry.GateFailureCategoryInfrastructureError
	FailureCategoryExternalDependency  = telemetry.GateFailureCategoryExternalDependency
)

var gitLabUnreachableGateEvaluationsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "loom_mills_gate_gitlab_unreachable_evaluations_total",
	Help: "Gate evaluations parked fail-closed because the GitLab dependency was unreachable.",
})

// Judgement is one judge's scored opinion behind an LLM gate Outcome.
//
// A tiebroken gate carries two — the primary and the second opinion — so a
// calibration read attributes an overrule to the model that actually made it
// rather than folding both scores onto the primary.
type Judgement struct {
	// Role is JudgeRolePrimary or JudgeRoleTiebreaker.
	Role      string
	Model     string
	Score     float64
	Threshold float64
	// Pass is this judge's own verdict (Score >= Threshold), not the gate's:
	// an overruled primary reports Pass false inside a passing Outcome.
	Pass bool
}

const (
	JudgeRolePrimary    = "primary"
	JudgeRoleTiebreaker = "tiebreaker"
)

// StageInput is the bundle of context every gate consumes. Fields are
// optional — a gate that doesn't need a particular input ignores it.
type StageInput struct {
	// RunID identifies the pipeline run evaluating the gate. It is telemetry
	// metadata and is deliberately excluded from InputDigest so identical
	// inputs can be compared across runs.
	RunID string

	// Item is the backlog item the pipeline run is materialising. Carries
	// labels, slice scope, success criteria, budget, and item-level policy.
	Item *store.BacklogItem

	// Policy is the active policy snapshot at the moment the gate runs.
	// The runtime captures it so a hot-reload mid-stage doesn't change the
	// verdict for an in-flight check.
	Policy *mills.Policy

	// FilesChanged is the list of repo-relative paths the upstream stage
	// added or modified. Empty for stages that are gated before any
	// implement step has run.
	FilesChanged []string

	// LinesAdded / LinesRemoved are diff-stat totals for size gates.
	LinesAdded   int
	LinesRemoved int

	// DiffPatch is the raw unified diff produced by the implement stage.
	// May be nil for gates that don't need pattern matching (diff_size,
	// scope, path_policy don't; secret_scan and commit_format do).
	DiffPatch []byte

	// CommitMessages is the list of commit messages the implement stage
	// produced. Drives the commit_format gate.
	CommitMessages []string

	// GitCaptureStatus / GitCaptureReason report whether the spawn
	// client's cumulative branch-vs-base git capture actually ran for the
	// implement stage, and why it did not. Sourced from the stage
	// artifacts under pipeline.GitCaptureArtifactKey; empty for stages or
	// backends that record nothing.
	//
	// Only nonempty_diff reads these, and only to phrase its failure. A
	// skipped capture and a genuinely empty branch reach every gate as
	// zero files plus an empty diff, so without this the gate asserted
	// "the agent did no work" over finished work that was already pushed
	// (issue #224) and burned the bounded retry budget re-running it.
	GitCaptureStatus string
	GitCaptureReason string

	// ProjectBootstrapped reports that the item's TargetProject is a
	// runtime-minted repo from the bootstrapped_projects registry. The
	// scope gate relaxes its no-slices fail for these: a plan authored
	// before its repo existed cannot declare file paths, so its emitted
	// items land slice-less by construction and there is nothing in a
	// freshly-seeded repo for the scope envelope to protect.
	ProjectBootstrapped bool

	// TestsPassed reports that the deterministic tests stage (the devbox
	// quality gate: fmt → lint → build → test over the FULL repository)
	// completed successfully earlier in this run. Wired by
	// runner.gateInputFor from the prior stage outputs, and rendered into
	// the LLM-judge prompt so the judge grades with the knowledge that the
	// build is verifiably green (escalation #304: the judge fabricated
	// "undefined symbol" compile failures on code whose tests stage had
	// passed). False means the stage has not run in this template — never
	// that it failed; a failed tests stage retries/escalates before any
	// downstream gate fires.
	TestsPassed bool
}

// Gate is the contract every check satisfies. Implementations must be
// deterministic given (input, ctx) and side-effect-free; the reconciler
// runs them concurrently inside a single transaction.
type Gate interface {
	// Name is the stable identifier persisted into gate_outcomes.gate_name.
	Name() string
	// Evaluate runs the check. Returning a non-nil error means the check
	// itself failed (e.g. an LLM call timed out); the reconciler treats
	// that as an infrastructure error, not a gate fail. A successful run
	// always returns (Outcome, nil) regardless of pass/fail.
	Evaluate(ctx context.Context, in StageInput) (Outcome, error)
}

// Registry resolves gate names to implementations. It is concurrent-safe
// and the operator constructs one at startup; tests can build their own.
type Registry struct {
	mu            sync.RWMutex
	gates         map[string]Gate
	telemetrySink telemetry.GateEvaluationSink
}

// SetTelemetrySink installs the observer for gate evaluation records. A nil
// sink disables observation without changing gate behavior.
func (r *Registry) SetTelemetrySink(sink telemetry.GateEvaluationSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.telemetrySink = sink
}

// NewRegistry returns an empty registry. Callers seed it via Register or
// the package-level Default() helper.
func NewRegistry() *Registry {
	return &Registry{gates: make(map[string]Gate)}
}

// Default returns a registry pre-populated with every pure-Go gate this
// package ships. LLM-judged gates are added by the operator later because
// they need a FlexInfer client wired in.
func Default() *Registry {
	r := NewRegistry()
	r.SetTelemetrySink(telemetry.MultiGateEvaluationSink{
		telemetry.DefaultGateMetrics(),
		telemetry.DefaultGateFailureCollector(),
	})
	r.Register(&NonEmptyDiff{})
	r.Register(&DiffSize{})
	r.Register(&Scope{})
	r.Register(&FabricatedSlice{})
	r.Register(&PathPolicy{})
	r.Register(&SecretScan{})
	r.Register(&CommitFormat{})
	r.Register(&DocsGuardrail{})
	return r
}

// Register adds g to the registry, panicking on a duplicate name. Names are
// part of the persisted contract; collisions are programmer errors.
func (r *Registry) Register(g Gate) {
	if g == nil {
		panic("gates: cannot register nil gate")
	}
	name := g.Name()
	if name == "" {
		panic("gates: gate has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.gates[name]; dup {
		panic(fmt.Sprintf("gates: duplicate registration for %q", name))
	}
	r.gates[name] = g
}

// Get returns the gate registered under name, or ErrUnknownGate.
func (r *Registry) Get(name string) (Gate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.gates[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownGate, name)
	}
	return g, nil
}

// Names returns the list of registered gate names, sorted for deterministic
// iteration in tests + logs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.gates))
	for n := range r.gates {
		out = append(out, n)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// EvaluateAll runs every named gate and returns the per-gate Outcomes plus
// an aggregate Pass that's true iff every gate passed and no infrastructure
// error fired. Order of returned outcomes mirrors the input slice; an
// unknown gate name produces an explicit fail Outcome rather than a panic
// so callers can render the error in HUD.
func (r *Registry) EvaluateAll(ctx context.Context, names []string, in StageInput) ([]NamedOutcome, bool, error) {
	out := make([]NamedOutcome, 0, len(names))
	allPass := true
	for _, n := range names {
		started := time.Now()
		g, err := r.Get(n)
		if err != nil {
			o := Outcome{
				Pass: false, Reasons: []string{err.Error()}, JudgedBy: "go",
			}
			r.recordOutcome(n, in, o, FailureCategoryUnknownGate, started)
			out = append(out, NamedOutcome{Name: n, Outcome: o})
			allPass = false
			continue
		}
		o, err := g.Evaluate(ctx, in)
		if err != nil {
			recordGateVerdictParse(ctx, n, o, err)
			category := FailureCategoryInfrastructureError
			if isGitLabUnreachable(err) {
				category = FailureCategoryExternalDependency
				gitLabUnreachableGateEvaluationsTotal.Inc()
				// The runner's external-incident classifier consumes the error
				// text after this boundary. Preserve the original chain while
				// adding its stable GitLab-unavailable signal so transport errors
				// park for dependency recovery instead of spending a code retry.
				err = fmt.Errorf("gitlab: service unavailable: %w", err)
			}
			r.recordEvaluation(telemetry.GateEvaluation{
				GateID: n, RunID: in.RunID, InputDigest: inputDigestForGate(n, in),
				Verdict: telemetry.GateVerdictError, FailureCategory: category,
				Reason:     err.Error(),
				DurationMS: elapsedMilliseconds(started),
			})
			return out, false, fmt.Errorf("gate %q: %w", n, err)
		}
		recordGateVerdictParse(ctx, n, o, nil)
		r.recordOutcome(n, in, o, failureCategoryForOutcome(o), started)
		out = append(out, NamedOutcome{Name: n, Outcome: o})
		if !o.Pass {
			allPass = false
		}
	}
	return out, allPass, nil
}

// isGitLabUnreachable conservatively recognizes dependency availability
// failures. GitLab attribution is mandatory: a timeout from an LLM judge (or
// any other gate dependency) remains a generic infrastructure error. Caller
// cancellation is shutdown, not an outage. GitLab edge 502/503/504 responses
// are included because they mean the dependency produced no usable verdict.
func isGitLabUnreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "gitlab") {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	for _, status := range []int{
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		code := strconv.Itoa(status)
		if strings.Contains(lower, "status "+code) ||
			strings.Contains(lower, "status="+code) ||
			strings.Contains(lower, code+" "+strings.ToLower(http.StatusText(status))) {
			return true
		}
	}
	return false
}

func recordGateVerdictParse(ctx context.Context, gate string, out Outcome, err error) {
	switch gate {
	case "spec_conformance", "pr_self_review", "scope":
	default:
		return
	}
	// Transport, configuration, and other evaluation failures did not produce
	// a verdict to parse. They are recorded by the existing gate-error metric
	// and must not inflate this counter's parse_error series.
	if err != nil && !isJudgeUnparseable(err) {
		return
	}
	outcome := telemetry.GateVerdictParseOutcomeParsed
	if isJudgeUnparseable(err) || out.JudgedBy == "flexinfer:unparseable" {
		outcome = telemetry.GateVerdictParseOutcomeParseError
	}
	telemetry.RecordGateVerdictParse(ctx, gate, outcome)
}

func (r *Registry) recordOutcome(name string, in StageInput, out Outcome, category FailureCategory, started time.Time) {
	r.recordEvaluation(telemetry.GateEvaluation{
		GateID: name, RunID: in.RunID, InputDigest: inputDigestForGate(name, in),
		Verdict: telemetryVerdictFor(out), FailureCategory: category,
		Reason:     strings.Join(out.Reasons, "; "),
		DurationMS: elapsedMilliseconds(started),
	})
}

func failureCategoryForOutcome(out Outcome) FailureCategory {
	if out.Pass || out.Skip {
		return ""
	}
	return FailureCategoryFail
}

func elapsedMilliseconds(started time.Time) int64 {
	duration := time.Since(started).Milliseconds()
	if duration < 0 { // Defensive against a non-monotonic clock implementation.
		return 0
	}
	return duration
}

func (r *Registry) recordEvaluation(e telemetry.GateEvaluation) {
	r.mu.RLock()
	sink := r.telemetrySink
	r.mu.RUnlock()
	if sink != nil {
		sink.RecordGateEvaluation(e)
	}
}

// inputDigest hashes the complete semantic gate input while excluding RunID,
// which identifies an evaluation rather than influencing its verdict. JSON
// gives structs and maps a stable encoding, and all StageInput fields are
// JSON-safe data.
func inputDigest(in StageInput) string {
	return inputDigestForGate("", in)
}

// inputDigestForGate hashes only the inputs that can affect the selected
// deterministic gate. Sets are cleaned, de-duplicated, and sorted first so
// capture order and duplicate file events do not give semantically identical
// evaluations different telemetry identities.
func inputDigestForGate(gateID string, in StageInput) string {
	in.RunID = ""
	var semanticInput any = in
	switch gateID {
	case "spec_conformance", "pr_self_review":
		// These are exactly the fields rendered by clients.composePrompt plus
		// the canary label consumed before the judge runs. Keep unrelated item
		// metadata and pipeline policy out of the replay identity.
		var itemID, itemTitle, specDoc, specAnchor string
		var canary bool
		if in.Item != nil {
			itemID = in.Item.ID
			itemTitle = in.Item.Title
			specDoc = in.Item.SpecDoc
			specAnchor = in.Item.SpecAnchor
			canary = itemHasCanaryLabel(in.Item)
		}
		filesChanged := append([]string(nil), in.FilesChanged...)
		diffPatch := append([]byte(nil), in.DiffPatch...)
		if gateID == "spec_conformance" {
			// NewSpecConformanceGate gives the judge this canonical snapshot.
			sort.Strings(filesChanged)
			diffPatch = canonicalizeUnifiedDiff(diffPatch)
		}
		semanticInput = struct {
			ItemPresent    bool     `json:"item_present"`
			ItemID         string   `json:"item_id"`
			ItemTitle      string   `json:"item_title"`
			SpecDoc        string   `json:"spec_doc"`
			SpecAnchor     string   `json:"spec_anchor"`
			Canary         bool     `json:"canary"`
			FilesChanged   []string `json:"files_changed"`
			LinesAdded     int      `json:"lines_added"`
			LinesRemoved   int      `json:"lines_removed"`
			DiffPatch      []byte   `json:"diff_patch"`
			CommitMessages []string `json:"commit_messages"`
			TestsPassed    bool     `json:"tests_passed"`
		}{
			ItemPresent: in.Item != nil, ItemID: itemID, ItemTitle: itemTitle,
			SpecDoc: specDoc, SpecAnchor: specAnchor, Canary: canary,
			FilesChanged: filesChanged, LinesAdded: in.LinesAdded,
			LinesRemoved: in.LinesRemoved, DiffPatch: diffPatch,
			CommitMessages: append([]string(nil), in.CommitMessages...),
			TestsPassed:    in.TestsPassed,
		}
	case "nonempty_diff":
		// Only presence is verdict-relevant; paths, patch bytes, and capture
		// diagnostics affect neither pass nor fail.
		semanticInput = struct {
			HasFiles bool `json:"has_files"`
			HasDiff  bool `json:"has_diff"`
		}{len(in.FilesChanged) != 0, len(in.DiffPatch) != 0}
	case "docs_guardrail":
		semanticInput = struct {
			FilesChanged   []string `json:"files_changed"`
			CommitMessages []string `json:"commit_messages"`
		}{
			FilesChanged:   canonicalStrings(in.FilesChanged, true),
			CommitMessages: canonicalStrings(in.CommitMessages, false),
		}
	case "scope":
		var files, tests []string
		var canary bool
		if in.Item != nil {
			canary = itemHasCanaryLabel(in.Item)
			for _, slice := range in.Item.Slices {
				files = append(files, slice.Files...)
				tests = append(tests, slice.Tests...)
			}
		}
		semanticInput = struct {
			ItemPresent         bool     `json:"item_present"`
			FilesChanged        []string `json:"files_changed"`
			ScopeFiles          []string `json:"scope_files"`
			ScopeTests          []string `json:"scope_tests"`
			Canary              bool     `json:"canary"`
			ProjectBootstrapped bool     `json:"project_bootstrapped"`
		}{
			ItemPresent:         in.Item != nil,
			FilesChanged:        canonicalStrings(in.FilesChanged, true),
			ScopeFiles:          canonicalStrings(files, true),
			ScopeTests:          canonicalStrings(tests, true),
			Canary:              canary,
			ProjectBootstrapped: in.ProjectBootstrapped,
		}
	}
	encoded, err := json.Marshal(semanticInput)
	if err != nil {
		// StageInput is intentionally data-only. Keep this failure explicit:
		// an empty digest would silently collapse unrelated evaluations.
		panic(fmt.Sprintf("gates: marshal stage input: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func canonicalStrings(values []string, cleanPaths bool) []string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		if cleanPaths {
			value = filepath.Clean(value)
		}
		unique[value] = struct{}{}
	}
	out := make([]string, 0, len(unique))
	for value := range unique {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func telemetryVerdictFor(out Outcome) telemetry.GateVerdict {
	if out.Skip {
		return telemetry.GateVerdictSkip
	}
	if out.Pass {
		return telemetry.GateVerdictPass
	}
	return telemetry.GateVerdictFail
}

// NamedOutcome pairs a gate name with its verdict so callers can persist
// gate_outcomes rows in one pass.
type NamedOutcome struct {
	Name    string
	Outcome Outcome
}

// ErrUnknownGate is returned by Registry.Get when no gate is registered
// under the requested name.
var ErrUnknownGate = errors.New("gates: unknown gate")

// pass / fail are tiny constructors shared by every gate so reasons[] stays
// nil for the happy path (and JSON-encodes as []).
func pass() Outcome { return Outcome{Pass: true, JudgedBy: "go"} }
func fail(reasons ...string) Outcome {
	return Outcome{Pass: false, Reasons: reasons, JudgedBy: "go"}
}

// skip marks a gate as advisory/not-applicable: Pass stays true so the pipeline
// proceeds, but the runner records gate_outcomes.outcome='skip' and the KPI
// pass-rate excludes it (see Outcome.Skip).
func skip(reasons ...string) Outcome {
	return Outcome{Pass: true, Skip: true, Reasons: reasons, JudgedBy: "go"}
}
