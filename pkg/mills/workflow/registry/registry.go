// Package registry is the S7 closed workflow-template registry (.loom/134
// §S7): named, versioned, immutable workflow programs with clamped numeric
// parameters and closed enum vocabularies.
//
// It is deliberately a LEAF package (imports pkg/mills/store and nothing else
// from mills) so both the imperative runtime (pkg/mills/workflow) and the
// council's authoring guard (pkg/mills/council) can consume it without an
// import cycle — the council sits upstream of worker/pipeline, which sit
// upstream of the workflow runtime.
//
// Rendering is the proven canary pattern generalized: the Starlark source is
// derived deterministically from FROZEN, CLAMPED parameters, so a resumed run
// re-derives the byte-identical script — step keys, call hashes, and
// idempotency keys replay stably across operator restarts. Only validated
// numerics and closed-enum tokens are ever interpolated, so a hostile caller
// cannot smuggle Starlark through a parameter.
//
// Every frozen selection embeds the template's content hash. At execution the
// interpreter re-derives the hash from the compiled-in registry and refuses a
// mismatch: a template whose source drifted under an in-flight run is the same
// hazard as an interpreter bump mid-journal (see the drain gate in
// pkg/mills/workflow/surface_test.go) and fails closed rather than replaying
// under new semantics.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Agent-type tokens for closed enum vocabularies. These are the wire values of
// pkg/mills/worker's AgentType* constants, inlined so this package stays a
// leaf; TestRegistryAgentTypeParity in pkg/mills/workflow pins the equality.
const (
	AgentTypeClaudeCode = "claude-code"
	AgentTypeCodex      = "codex"
)

// ErrUnknownTemplate reports resolution of a name/version outside the closed
// registry. Callers must fail closed (skip or escalate), never fall through to
// a default program.
var ErrUnknownTemplate = errors.New("workflow template not in the closed registry")

// ParamSpec is the clamp contract for one numeric parameter. Values outside
// [Min,Max] clamp to the violated bound; missing / NaN / ±Inf take Default.
// Clamping is idempotent, so re-clamping frozen params at replay is
// byte-stable.
type ParamSpec struct {
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Default float64 `json:"default"`
}

// EnumSpec is the closed-vocabulary contract for one string parameter. A value
// outside Allowed is rejected (not defaulted): an explicit unknown token is an
// authoring error, and silently rewriting it would mask it.
type EnumSpec struct {
	Allowed []string `json:"allowed"`
	Default string   `json:"default"`
}

// Template is one immutable entry in the closed registry.
type Template struct {
	Name    string
	Version string
	Params  map[string]ParamSpec
	Enums   map[string]EnumSpec
	// render derives the Starlark program from CLAMPED numeric and VALIDATED
	// enum parameters. It must be a pure function of its arguments.
	render func(nums map[string]float64, enums map[string]string) string
}

// ClampParams applies the numeric clamp contract. Unknown keys are dropped,
// missing keys take defaults, non-finite values take defaults, out-of-range
// values clamp to the violated bound.
func (t *Template) ClampParams(raw map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(t.Params))
	for name, spec := range t.Params {
		v, ok := raw[name]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			v = spec.Default
		}
		if v < spec.Min {
			v = spec.Min
		}
		if v > spec.Max {
			v = spec.Max
		}
		out[name] = v
	}
	return out
}

// ValidateEnums applies the closed-vocabulary contract. Missing keys take
// defaults; unknown keys are dropped; a present-but-disallowed value errors.
func (t *Template) ValidateEnums(raw map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(t.Enums))
	for name, spec := range t.Enums {
		v, ok := raw[name]
		if !ok || v == "" {
			out[name] = spec.Default
			continue
		}
		allowed := false
		for _, a := range spec.Allowed {
			if v == a {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("template %s@%s: enum param %q rejects value %q (allowed: %s)",
				t.Name, t.Version, name, v, strings.Join(spec.Allowed, ", "))
		}
		out[name] = v
	}
	return out, nil
}

// Render derives the byte-stable program from raw parameters, returning the
// script alongside the clamped/validated parameters that produced it (the
// caller freezes exactly these).
func (t *Template) Render(nums map[string]float64, enums map[string]string) (string, map[string]float64, map[string]string, error) {
	clamped := t.ClampParams(nums)
	validated, err := t.ValidateEnums(enums)
	if err != nil {
		return "", nil, nil, err
	}
	return t.render(clamped, validated), clamped, validated, nil
}

// ContentHash binds the template identity: name, version, the full parameter
// contract, and the default-rendered source. Any change to source or contract
// changes the hash, which invalidates frozen selections fail-closed.
func (t *Template) ContentHash() string {
	h := sha256.New()
	h.Write([]byte("mills-workflow-template\x00"))
	h.Write([]byte(t.Name))
	h.Write([]byte{0x00})
	h.Write([]byte(t.Version))
	h.Write([]byte{0x00})

	numNames := make([]string, 0, len(t.Params))
	for n := range t.Params {
		numNames = append(numNames, n)
	}
	sort.Strings(numNames)
	for _, n := range numNames {
		spec := t.Params[n]
		fmt.Fprintf(h, "num:%s:%s:%s:%s\x00", n,
			formatParamFloat(spec.Min), formatParamFloat(spec.Max), formatParamFloat(spec.Default))
	}
	enumNames := make([]string, 0, len(t.Enums))
	for n := range t.Enums {
		enumNames = append(enumNames, n)
	}
	sort.Strings(enumNames)
	for _, n := range enumNames {
		spec := t.Enums[n]
		fmt.Fprintf(h, "enum:%s:%s:%s\x00", n, strings.Join(spec.Allowed, ","), spec.Default)
	}

	defaults := t.ClampParams(nil)
	enumDefaults, _ := t.ValidateEnums(nil)
	h.Write([]byte(t.render(defaults, enumDefaults)))
	return hex.EncodeToString(h.Sum(nil))
}

func formatParamFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Registry is the closed name→version→template set.
type Registry struct {
	templates map[string]map[string]*Template
}

// NewDefault builds the compiled-in closed set. Adding a template is a code
// change reviewed like any other interpreter-surface change.
func NewDefault() *Registry {
	r := &Registry{templates: map[string]map[string]*Template{}}
	r.add(implementGateV1())
	return r
}

func (r *Registry) add(t *Template) {
	byVersion, ok := r.templates[t.Name]
	if !ok {
		byVersion = map[string]*Template{}
		r.templates[t.Name] = byVersion
	}
	byVersion[t.Version] = t
}

// Resolve returns the immutable template for name@version, or
// ErrUnknownTemplate. The workflow canary is deliberately NOT in the registry:
// its identity machinery (agent-type + merging params, tamper guards) predates
// S7 and is proven by the S1c/S6-full gates; it stays on its own path.
func (r *Registry) Resolve(name, version string) (*Template, error) {
	byVersion, ok := r.templates[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrUnknownTemplate, name, version)
	}
	t, ok := byVersion[version]
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrUnknownTemplate, name, version)
	}
	return t, nil
}

// Names returns the sorted template names (for diagnostics and tests).
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.templates))
	for n := range r.templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// runParams is the frozen selection stored in workflow_runs.workflow_params
// for registry-resolved runs. The top-level key is distinct from the canary's
// params shape so the two identities can never be confused.
type runParams struct {
	RegistryTemplate *frozenSelection `json:"registry_template,omitempty"`
}

type frozenSelection struct {
	ContentHash string             `json:"content_hash"`
	Params      map[string]float64 `json:"params,omitempty"`
	Enums       map[string]string  `json:"enums,omitempty"`
}

// FreezeSelection resolves name@version, clamps/validates the raw parameters,
// and returns the params JSON to stamp verbatim onto a new run.
func (r *Registry) FreezeSelection(name, version string, nums map[string]float64, enums map[string]string) (string, error) {
	t, err := r.Resolve(name, version)
	if err != nil {
		return "", err
	}
	_, clamped, validated, err := t.Render(nums, enums)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(runParams{RegistryTemplate: &frozenSelection{
		ContentHash: t.ContentHash(),
		Params:      clamped,
		Enums:       validated,
	}})
	if err != nil {
		return "", fmt.Errorf("freeze selection %s@%s: %w", name, version, err)
	}
	return string(blob), nil
}

// Validate reports whether a selection would freeze cleanly, without
// producing the frozen blob. This is the authoring-side guard (council).
func (r *Registry) Validate(name, version string, nums map[string]float64, enums map[string]string) error {
	_, err := r.FreezeSelection(name, version, nums, enums)
	return err
}

// ScriptFromRun re-derives the byte-stable program for a registry-resolved
// run. It fails closed on: unknown template/version, undecodable or missing
// frozen selection, and content-hash drift (the compiled-in template no longer
// matches the one the run froze — the deploy-time analog of interpreter
// version drift).
func (r *Registry) ScriptFromRun(run *store.WorkflowRun) (string, error) {
	if run == nil {
		return "", errors.New("workflow registry: nil run")
	}
	t, err := r.Resolve(run.Template, run.TemplateVersion)
	if err != nil {
		return "", err
	}
	var params runParams
	if strings.TrimSpace(run.WorkflowParams) == "" {
		return "", fmt.Errorf("workflow registry: run %s has no frozen selection", run.ID)
	}
	if err := json.Unmarshal([]byte(run.WorkflowParams), &params); err != nil {
		return "", fmt.Errorf("workflow registry: decode frozen selection for run %s: %w", run.ID, err)
	}
	sel := params.RegistryTemplate
	if sel == nil {
		return "", fmt.Errorf("workflow registry: run %s params carry no registry_template selection", run.ID)
	}
	if current := t.ContentHash(); sel.ContentHash != current {
		return "", fmt.Errorf("workflow registry: run %s frozen template %s@%s content hash %s does not match compiled-in %s (template drifted under a frozen run)",
			run.ID, run.Template, run.TemplateVersion, sel.ContentHash, current)
	}
	script, _, _, err := t.Render(sel.Params, sel.Enums)
	if err != nil {
		return "", fmt.Errorf("workflow registry: render run %s: %w", run.ID, err)
	}
	return script, nil
}

// ----- Compiled-in templates -----------------------------------------------

// implementGateV1 is the first registry template: the proven canary shape
// (one implement agent, one trivial gate, stop pre-merge) with a clamped
// budget and a closed harness choice.
func implementGateV1() *Template {
	return &Template{
		Name:    "implement-gate",
		Version: "v1",
		Params: map[string]ParamSpec{
			"budget_usd": {Min: 0.05, Max: 5.0, Default: 1.0},
		},
		Enums: map[string]EnumSpec{
			"model": {
				Allowed: []string{AgentTypeClaudeCode, AgentTypeCodex},
				Default: AgentTypeClaudeCode,
			},
		},
		render: func(nums map[string]float64, enums map[string]string) string {
			return fmt.Sprintf("\nagent('implement', model='%s', budget_usd=%s)\ngate('trivial')\n",
				enums["model"], formatParamFloat(nums["budget_usd"]))
		},
	}
}
