package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

// PlanSummary is the projection of a Plan returned by agent_plan_list. The
// list view omits heavy fields (the spec_doc body, namespace) by design, so
// only the columns the Mills plan-slice emitter needs are decoded here.
// Priority is the plan's warp-beam bucket (P0..P3, "" = unset); the emitter
// propagates it onto emitted backlog items so the dispatcher's
// priority-ordered pickup reflects the operator's plan ordering.
type PlanSummary struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Phase    string `json:"phase"`
	Title    string `json:"title"`
	Priority string `json:"priority"`
}

// PlanSliceSummary is the projection of a PlanSlice returned by
// agent_plan_slice_list. goal + acceptance_criteria author the BacklogItem's
// spec; Files carries the slice's declared file set so the plan-slice emitter
// can stamp a single-slice scope onto the emitted item. Without Files the
// emitted item lands slice-less and trips the scope gate ("backlog item has
// no slices; no scope to enforce") on every implement attempt — the same
// escalation cascade a sliceless council item hits.
type PlanSliceSummary struct {
	ID                 string   `json:"id"`
	PlanID             string   `json:"plan_id"`
	Name               string   `json:"name"`
	Phase              string   `json:"phase"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Files              []string `json:"files"`
	// MRRef + Decisions serve the take-up reconciler: MRRef is the slice's
	// recorded merge request ("!912", bare IID, or URL) whose terminal state
	// drives phase sync; Decisions carries prior notes so the reconciler can
	// dedupe its orphan flag. Like Files, array columns are omitted by the
	// TOON tabular LIST view — recover Decisions via GetSlice (detail).
	MRRef     string   `json:"mr_ref"`
	Decisions []string `json:"decisions"`
}

type planListEnvelope struct {
	OK    bool          `json:"ok"`
	Plans []PlanSummary `json:"plans"`
}

type sliceListEnvelope struct {
	OK     bool               `json:"ok"`
	Slices []PlanSliceSummary `json:"slices"`
}

type sliceGetEnvelope struct {
	OK    bool             `json:"ok"`
	Slice PlanSliceSummary `json:"slice"`
}

// ListPlans returns plans matching project/namespace/phase via agent_plan_list.
// Any filter may be empty to widen the query. Read-only and cross-agent (the
// store never scopes plan reads by agent_id).
func (c *PlanClient) ListPlans(ctx context.Context, project, namespace, phase string) ([]PlanSummary, error) {
	if c == nil || c.Hub == nil {
		return nil, errors.New("plan: client not configured")
	}
	args := map[string]any{}
	if s := strings.TrimSpace(project); s != "" {
		args["project"] = s
	}
	if s := strings.TrimSpace(namespace); s != "" {
		args["namespace"] = s
	}
	if s := strings.TrimSpace(phase); s != "" {
		args["phase"] = s
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_list", args)
	if err != nil && body == "" {
		return nil, fmt.Errorf("plan: list: %w", err)
	}
	var env planListEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return nil, fmt.Errorf("plan: list: %w; raw=%q", err, truncateBody(body, 240))
		}
		return nil, fmt.Errorf("plan: list decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK {
		return nil, fmt.Errorf("plan: list rejected: %s", truncateBody(body, 240))
	}
	return env.Plans, nil
}

// ListExistingPlans projects the plans in a namespace (ALL phases) into the
// council's minimal ExistingPlan shape (id + title) so the BacklogMutator can
// dedup a to-be-authored sliced Plan against what's already there. Listing all
// phases is deliberate: a theme already served on main has its Plan advanced to
// "merged" by the take-up reconciler, and we still want that to block a re-ask.
// The council package can't import clients (clients imports council), so the
// projection happens here and *PlanClient satisfies council.PlanLister
// structurally.
func (c *PlanClient) ListExistingPlans(ctx context.Context, project, namespace string) ([]council.ExistingPlan, error) {
	sums, err := c.ListPlans(ctx, project, namespace, "")
	if err != nil {
		return nil, err
	}
	out := make([]council.ExistingPlan, 0, len(sums))
	for _, s := range sums {
		out = append(out, council.ExistingPlan{ID: s.ID, Title: s.Title})
	}
	return out, nil
}

// ListSlices returns the ordered slices of a plan via agent_plan_slice_list.
func (c *PlanClient) ListSlices(ctx context.Context, planID string) ([]PlanSliceSummary, error) {
	if c == nil || c.Hub == nil {
		return nil, errors.New("plan: client not configured")
	}
	if strings.TrimSpace(planID) == "" {
		return nil, errors.New("plan: plan_id required")
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_slice_list", map[string]any{"plan_id": planID})
	if err != nil && body == "" {
		return nil, fmt.Errorf("plan: slice list: %w", err)
	}
	var env sliceListEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return nil, fmt.Errorf("plan: slice list: %w; raw=%q", err, truncateBody(body, 240))
		}
		return nil, fmt.Errorf("plan: slice list decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK {
		return nil, fmt.Errorf("plan: slice list rejected: %s", truncateBody(body, 240))
	}
	return env.Slices, nil
}

// GetSlice fetches one slice's full detail via agent_plan_slice_get. Unlike the
// LIST view (agent_plan_slice_list), whose token-efficient TOON tabular form
// omits array columns, the detail view includes the `files` array — so the
// plan-slice emitter calls this to recover a slice's declared file scope (the
// list always returns empty Files, which left every emitted item slice-less and
// fail-closed at the `scope` gate).
func (c *PlanClient) GetSlice(ctx context.Context, sliceID string) (PlanSliceSummary, error) {
	if c == nil || c.Hub == nil {
		return PlanSliceSummary{}, errors.New("plan: client not configured")
	}
	if strings.TrimSpace(sliceID) == "" {
		return PlanSliceSummary{}, errors.New("plan: slice_id required")
	}
	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_slice_get", map[string]any{"slice_id": sliceID})
	if err != nil && body == "" {
		return PlanSliceSummary{}, fmt.Errorf("plan: slice get: %w", err)
	}
	var env sliceGetEnvelope
	if derr := decodeListBody(body, &env); derr != nil {
		if err != nil {
			return PlanSliceSummary{}, fmt.Errorf("plan: slice get: %w; raw=%q", err, truncateBody(body, 240))
		}
		return PlanSliceSummary{}, fmt.Errorf("plan: slice get decode: %w; raw=%q", derr, truncateBody(body, 240))
	}
	if !env.OK {
		return PlanSliceSummary{}, fmt.Errorf("plan: slice get rejected: %s", truncateBody(body, 240))
	}
	return env.Slice, nil
}

// SliceScopeForPlan resolves a plan's slices into the store-shaped scope
// (name + files) the pipeline's scope gate enforces, plus the flat file list
// for protected-path pre-declaration. The LIST view's TOON tabular encoding
// omits array columns, so each file-less slice is re-fetched via GetSlice
// (detail) — the same recovery the plan-slice emitter and the backlog-intake
// handler use. A slice that still declares no files after the detail fetch is
// dropped: it contributes nothing enforceable, and stamping it would make the
// item LOOK scoped while the gate's allowlist stayed empty. Detail-fetch
// errors skip only that slice (best-effort, mirroring the emitter); a list
// error is returned so callers can log the hydration miss.
func (c *PlanClient) SliceScopeForPlan(ctx context.Context, planID string) ([]store.Slice, []string, error) {
	slices, err := c.ListSlices(ctx, planID)
	if err != nil {
		return nil, nil, err
	}
	var (
		out      []store.Slice
		allFiles []string
	)
	for _, sl := range slices {
		files := sl.Files
		if len(files) == 0 {
			full, gerr := c.GetSlice(ctx, sl.ID)
			if gerr != nil {
				continue
			}
			files = full.Files
		}
		name := strings.TrimSpace(sl.Name)
		if name == "" || len(files) == 0 {
			continue
		}
		out = append(out, store.Slice{Name: name, Files: append([]string(nil), files...)})
		allFiles = append(allFiles, files...)
	}
	return out, allFiles, nil
}

func (c *PlanClient) serverName() string {
	if c.ServerName != "" {
		return c.ServerName
	}
	return AgentContextServerName
}

// decodeListBody parses an agent-context response that may arrive as plain
// JSON or TOON (the hub's token-efficient tabular encoding).
//
// The decode is STRICT: the document must carry the boolean "ok" field every
// genuine agent_plan_* / agent_pattern_* envelope includes. Without the
// probe, a plain-text tool-error body (e.g. "plan get: not found: xyz")
// slipped through the TOON fallback — any single line containing a colon
// decodes as a {key: value} object — and unmarshaled into a ZERO-VALUE
// envelope, so a hub/tool error silently read as "no plans" or "plan has no
// slices" (the sliceless-item scope-gate cascade) instead of surfacing an
// error. Mirrors parseDevboxQualityGateResult's "passed" probe
// (pkg/mills/clients/devbox.go, escalation #322). out must be a pointer to
// an envelope struct with an `ok` field.
func decodeListBody(body string, out any) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("empty body")
	}
	raw := []byte(body)
	if !json.Valid(raw) {
		jsonBody, err := mcp.DecodeTOONToJSON(body)
		if err != nil {
			return err
		}
		raw = jsonBody
	}
	var probe struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	if probe.OK == nil {
		return errors.New(`not an agent-context envelope (no "ok" field)`)
	}
	return json.Unmarshal(raw, out)
}

// truncateBody bounds a raw response for error messages so a large TOON blob
// doesn't flood the logs.
func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
