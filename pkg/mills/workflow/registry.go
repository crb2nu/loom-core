package workflow

// S7 (.loom/134): council template selection + closed registry + clamping.
//
// The pure registry (templates, clamps, content hashes, script derivation)
// lives in the leaf subpackage pkg/mills/workflow/registry so the council's
// authoring guard can import it without a cycle. This file is the
// runtime-side layer: it binds a resolved selection to the interpreter
// version pin and the immutable run-creation shape.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow/registry"
)

// Registry is the closed template set (see pkg/mills/workflow/registry).
type Registry = registry.Registry

// ErrUnknownTemplate re-exports the leaf package's fail-closed resolution
// error for runtime-side callers.
var ErrUnknownTemplate = registry.ErrUnknownTemplate

// NewDefaultRegistry builds the compiled-in closed set.
func NewDefaultRegistry() *Registry { return registry.NewDefault() }

// ErrWorkflowsDisabled reports an explicit imperative selection while the
// workflow runtime is policy-disabled. The caller defers the item — silently
// running the DAG instead would override the author's explicit choice.
var ErrWorkflowsDisabled = errors.New("workflow runtime is disabled by policy")

// ResolvedSelection is the frozen identity a new imperative run is created
// with. Every field is immutable for the life of the run. Aliased to the
// store type so the reconciler's WorkflowSelector contract (pkg/mills, which
// cannot import this package) and ClaimWorkflowStart share it directly.
type ResolvedSelection = store.WorkflowSelection

// ResolveItemSelection is the S7 admission-side resolution: consulted exactly
// once, when a queued backlog item is claimed, BEFORE any run row exists.
//
//   - No selection on the item → (nil, nil): the caller creates the default
//     DAG pipeline, byte-identical to pre-S7 behavior.
//   - Selection while workflows are policy-disabled → ErrWorkflowsDisabled:
//     the caller defers the item.
//   - Unknown template/version or a rejected enum → the underlying error:
//     the caller fails closed (skip/escalate), never a default program.
//   - Valid selection → the frozen identity to stamp onto the new run,
//     pinned to this binary's interpreter version.
//
// Started runs are NEVER re-resolved: execution derives everything from the
// run row (Registry.ScriptFromRun), so later edits to the item's selection
// fields cannot re-route an in-flight run.
func ResolveItemSelection(r *Registry, workflowsEnabled bool, item *store.BacklogItem) (*ResolvedSelection, error) {
	if item == nil || strings.TrimSpace(item.Policy.WorkflowTemplate) == "" {
		return nil, nil
	}
	if !workflowsEnabled {
		return nil, fmt.Errorf("%w: item %s selects template %s", ErrWorkflowsDisabled, item.ID, item.Policy.WorkflowTemplate)
	}
	name := strings.TrimSpace(item.Policy.WorkflowTemplate)
	version := strings.TrimSpace(item.Policy.WorkflowTemplateVersion)
	paramsJSON, err := r.FreezeSelection(name, version, item.Policy.WorkflowParams, item.Policy.WorkflowEnums)
	if err != nil {
		return nil, err
	}
	return &ResolvedSelection{
		Engine:             store.WorkflowEngineImperative,
		Template:           name,
		TemplateVersion:    version,
		InterpreterVersion: HostInterpreterVersion,
		ParamsJSON:         paramsJSON,
	}, nil
}

// CreateRunFromSelection inserts a fresh running imperative run carrying a
// frozen registry selection. This is the S7 analog of the canary's
// CreateImperativeRunWithOptions: the admin/test entrypoint today, and the
// shape the transactional claim kernel stamps when reconciler-side selection
// lands. Every identity field comes from the ResolvedSelection verbatim.
func CreateRunFromSelection(ctx context.Context, dao *store.WorkflowDAO, id, backlogID string, sel *ResolvedSelection) (*store.WorkflowRun, error) {
	if dao == nil {
		return nil, errors.New("workflow: nil dao")
	}
	if id == "" {
		return nil, errors.New("workflow: run id required")
	}
	if sel == nil || sel.Engine != store.WorkflowEngineImperative {
		return nil, errors.New("workflow: a frozen imperative selection is required")
	}
	now := time.Now().UTC()
	run := &store.WorkflowRun{
		ID:                 id,
		BacklogID:          backlogID,
		Engine:             sel.Engine,
		Template:           sel.Template,
		TemplateVersion:    sel.TemplateVersion,
		InterpreterVersion: sel.InterpreterVersion,
		WorkflowParams:     sel.ParamsJSON,
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := dao.CreateWorkflowRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}
