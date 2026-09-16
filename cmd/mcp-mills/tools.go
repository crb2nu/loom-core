package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/validate"
)

func (s *millsServer) handleStatus(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
	var status map[string]any
	if err := s.api.get(ctx, "/api/mills/status", &status); err != nil {
		return mcp.ErrorResult(err), nil
	}
	out := map[string]any{
		"autonomy_ready":       status["autonomy_ready"],
		"autonomy_blockers":    status["autonomy_blockers"],
		"active_pipeline_runs": status["active_pipeline_runs"],
		"budget":               status["budget"],
	}
	// Only non-green capabilities are worth an agent's attention.
	var degraded []any
	if caps, ok := status["capabilities"].([]any); ok {
		for _, c := range caps {
			if m, ok := c.(map[string]any); ok && m["status"] != "green" {
				degraded = append(degraded, m)
			}
		}
	}
	out["degraded_capabilities"] = degraded
	return mcp.JSONResult(out)
}

func (s *millsServer) handleBacklogList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	state := v.String("state", "")
	contains := v.String("id_contains", "")
	limit := v.IntRange("limit", 50, 1, 200)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	items, err := s.fetchBacklog(ctx)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	var rows []map[string]any
	for _, it := range items {
		if state != "" && it["State"] != state {
			continue
		}
		id, _ := it["ID"].(string)
		if contains != "" && !strings.Contains(id, contains) {
			continue
		}
		title, _ := it["Title"].(string)
		rows = append(rows, map[string]any{
			"id": id, "state": it["State"], "priority": it["Priority"],
			"title": truncate(title, 90), "updated": it["UpdatedAt"],
		})
		if len(rows) >= limit {
			break
		}
	}
	return mcp.JSONResult(map[string]any{"count": len(rows), "items": rows})
}

func (s *millsServer) handleBacklogGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var item map[string]any
	if err := s.api.get(ctx, "/api/mills/backlog/"+url.PathEscape(id), &item); err != nil {
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(item)
}

func (s *millsServer) handleRuns(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	scope := v.String("scope", "active")
	contains := v.String("backlog_contains", "")
	limit := v.IntRange("limit", 25, 1, 200)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	path := "/api/mills/pipeline/runs"
	if scope == "terminal" {
		path += "?state=terminal&limit=200"
	}
	var runs []map[string]any
	if err := s.api.get(ctx, path, &runs); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var rows []map[string]any
	for _, r := range runs {
		backlog, _ := r["BacklogID"].(string)
		if contains != "" && !strings.Contains(backlog, contains) {
			continue
		}
		id, _ := r["ID"].(string)
		rows = append(rows, map[string]any{
			"id": truncate(id, 70), "backlog": backlog, "state": r["State"],
			"stage": r["CurrentStage"], "failure_class": r["FailureClass"],
			"escalation_class": r["EscalationClass"], "mr_iid": r["MRIID"],
			"attempts": r["Attempts"],
		})
		if len(rows) >= limit {
			break
		}
	}
	return mcp.JSONResult(map[string]any{"scope": scope, "count": len(rows), "runs": rows})
}

func (s *millsServer) handleKPIs(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	window := v.String("window", "1d")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var snap map[string]any
	if err := s.api.get(ctx, "/api/mills/kpis?window="+url.QueryEscape(window), &snap); err != nil {
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(snap)
}

func (s *millsServer) handleDiagnose(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var item map[string]any
	if err := s.api.get(ctx, "/api/mills/backlog/"+url.PathEscape(id), &item); err != nil {
		return mcp.ErrorResult(err), nil
	}
	project, _ := item["TargetProject"].(string)

	var terminal []map[string]any
	if err := s.api.get(ctx, "/api/mills/pipeline/runs?state=terminal&limit=200", &terminal); err != nil {
		return mcp.ErrorResult(err), nil
	}
	var latest map[string]any
	for _, r := range terminal {
		if backlog, _ := r["BacklogID"].(string); backlog == id {
			latest = r
			break // newest-first listing: the first hit is the latest run
		}
	}

	branches, branchErr := s.brancher.ImplementBranches(ctx, project, id)
	out := map[string]any{
		"id": id, "state": item["State"], "grade": item["Grade"],
	}
	if latest != nil {
		out["latest_terminal_run"] = map[string]any{
			"id": latest["ID"], "state": latest["State"], "stage": latest["CurrentStage"],
			"failure_class": latest["FailureClass"], "escalation_class": latest["EscalationClass"],
			"failure_signature": latest["FailureSignature"], "mr_iid": latest["MRIID"],
			"attempts": latest["Attempts"],
		}
	}
	if branchErr != nil {
		out["branch_probe_error"] = branchErr.Error()
	} else {
		out["implement_branches"] = branches
	}
	switch {
	case branchErr == nil && len(branches) > 0:
		out["verdict"] = "RESCUE: an implement branch exists — do not requeue (the fresh spawn collides with it and dies FailureClass=configuration). Finish the branch, ready its MR, arm merge-when-pipeline-succeeds, and let mills adopt-green-MR settle the item."
	case latest != nil && latest["MRIID"] != nil:
		out["verdict"] = "CHECK MR: a run left an MR behind — verify its pipeline and merge state before deciding."
	default:
		out["verdict"] = "REQUEUE-SAFE: no implement branch found; re-ground the spec (verify every Slices file exists on the target's main) and requeue."
	}
	return mcp.JSONResult(out)
}

func (s *millsServer) handleBacklogPost(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("id")
	title := v.Required("title")
	spec := v.Required("spec_doc")
	priority := v.String("priority", "P2")
	project := v.String("target_project", "")
	sliceName := v.String("slice_name", "")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	files, err := scopeAdditions(args["files"])
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("files: %w", err)), nil
	}
	files, dropped, err := filterScopeFiles(files)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("files: %w", err)), nil
	}
	if len(files) == 0 {
		return emptyScopeResult(dropped)
	}
	if sliceName == "" {
		sliceName = id
	}
	item := map[string]any{
		"ID": id, "Title": title, "SpecDoc": spec, "Priority": priority,
		"TargetProject": project, "CreatedBy": "mcp-mills",
		"Labels": stringSlice(args["labels"]),
		"Slices": []map[string]any{{
			"name": sliceName, "files": files, "tests": stringSlice(args["tests"]),
		}},
	}
	var created map[string]any
	if err := s.api.post(ctx, "/api/mills/backlog", item, &created); err != nil {
		var stale errStaleRevision
		if errors.As(err, &stale) {
			return mcp.ErrorResult(mcperror.InvalidParam("id", "item already exists (idempotent 409): "+id)), nil
		}
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(map[string]any{"created": id, "revision": created["Revision"], "state": created["State"], "dropped_files": dropped, "warnings": scopeEnvelopeWarnings(files)})
}

var allowedTransitions = map[string]bool{"queued": true, "retired": true, "merged": true}

func (s *millsServer) handleUpdateState(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("id")
	target := v.Required("state")
	force := v.Bool("force", false)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if !allowedTransitions[target] {
		return mcp.ErrorResult(mcperror.InvalidParam("state", "must be queued, retired, or merged")), nil
	}

	var item map[string]any
	if err := s.api.get(ctx, "/api/mills/backlog/"+url.PathEscape(id), &item); err != nil {
		return mcp.ErrorResult(err), nil
	}

	if target == "queued" && !force {
		project, _ := item["TargetProject"].(string)
		branches, err := s.brancher.ImplementBranches(ctx, project, id)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("refusing requeue: branch probe failed (%w); pass force=true only if you have verified no implement branch exists", err)), nil
		}
		if len(branches) > 0 {
			return mcp.ErrorResult(mcperror.InvalidParam("state",
				"requeue refused: implement branch exists ("+strings.Join(branches, ", ")+") — a fresh spawn collides with it (FailureClass=configuration). Finish the branch and let mills adopt the green MR, or pass force=true after deleting it.")), nil
		}
	}

	oldState := item["State"]
	item["State"] = target
	var updated map[string]any
	err := s.api.post(ctx, "/api/mills/backlog", item, &updated)
	var stale errStaleRevision
	if errors.As(err, &stale) {
		// One retry from a fresh read: never resubmit the old body.
		if err = s.api.get(ctx, "/api/mills/backlog/"+url.PathEscape(id), &item); err != nil {
			return mcp.ErrorResult(err), nil
		}
		oldState = item["State"]
		item["State"] = target
		err = s.api.post(ctx, "/api/mills/backlog", item, &updated)
	}
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(map[string]any{
		"id": id, "old_state": oldState, "new_state": target, "revision": updated["Revision"],
	})
}

func (s *millsServer) fetchBacklog(ctx context.Context) ([]map[string]any, error) {
	// The endpoint historically returns either a bare array or {items:[...]}.
	var raw any
	if err := s.api.get(ctx, "/api/mills/backlog?limit=1000", &raw); err != nil {
		return nil, err
	}
	var list []any
	switch t := raw.(type) {
	case []any:
		list = t
	case map[string]any:
		if inner, ok := t["items"].([]any); ok {
			list = inner
		} else if inner, ok := t["Items"].([]any); ok {
			list = inner
		}
	}
	items := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items, nil
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// handleAmendScope re-applies only the requested additions to a fresh full
// record on conflict, preserving concurrent edits and unknown wire fields.
func (s *millsServer) handleAmendScope(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("id")
	name := v.String("slice_name", "")
	requeue, force := v.Bool("requeue", false), v.Bool("force", false)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	files, err := scopeAdditions(args["add_files"])
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("add_files: %w", err)), nil
	}
	tests, err := scopeAdditions(args["add_tests"])
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("add_tests: %w", err)), nil
	}
	if len(files)+len(tests) == 0 {
		return mcp.ErrorResult(fmt.Errorf("at least one of add_files/add_tests is required")), nil
	}
	files, dropped, err := filterScopeFiles(files)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("add_files: %w", err)), nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		var item map[string]any
		if err := s.api.get(ctx, "/api/mills/backlog/"+url.PathEscape(id), &item); err != nil {
			return mcp.ErrorResult(err), nil
		}
		slices, _ := item["Slices"].([]any)
		var selected map[string]any
		for i, raw := range slices {
			slice, _ := raw.(map[string]any)
			if (name == "" && i == 0) || (name != "" && slice["name"] == name) {
				selected = slice
				break
			}
		}
		if selected == nil {
			return mcp.ErrorResult(fmt.Errorf("slice %q not found (item must have a slice)", name)), nil
		}
		if requeue && !force {
			project, _ := item["TargetProject"].(string)
			if err := s.guardScopeRequeue(ctx, project, id); err != nil {
				return mcp.ErrorResult(err), nil
			}
		}
		oldRevision := item["Revision"]
		mergedFiles, addedFiles := appendScopeValues(selected["files"], files)
		mergedFiles, existingDropped, err := filterScopeFiles(mergedFiles)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("files: %w", err)), nil
		}
		allDropped := append(append([]string{}, dropped...), existingDropped...)
		if len(mergedFiles) == 0 {
			return emptyScopeResult(allDropped)
		}
		selected["files"] = mergedFiles
		success, _ := item["Success"].(map[string]any)
		if success == nil {
			success = map[string]any{}
		}
		mergedTests, addedTests := appendScopeValues(success["tests"], tests)
		if len(tests) > 0 {
			success["tests"] = mergedTests
			item["Success"] = success
		}
		var updated map[string]any
		err = s.api.post(ctx, "/api/mills/backlog", item, &updated)
		var stale errStaleRevision
		if attempt == 0 && errors.As(err, &stale) {
			continue
		}
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		summary := map[string]any{"id": id, "amended": true, "old_revision": oldRevision, "revision": updated["Revision"], "files_added": addedFiles, "tests_added": addedTests, "dropped_files": allDropped, "warnings": scopeEnvelopeWarnings(mergedFiles)}
		if requeue {
			var start map[string]any
			if err := s.api.post(ctx, "/api/mills/pipeline/runs/"+url.PathEscape(id)+"/start?requeue=1", nil, &start); err != nil {
				summary["requeue_error"] = err.Error()
				result, jsonErr := mcp.JSONResult(summary)
				if result != nil {
					result.IsError = true
				}
				return result, jsonErr
			}
			summary["requeue"] = map[string]any{"decision": start["decision"], "run_id": start["run_id"]}
		}
		return mcp.JSONResult(summary)
	}
	return mcp.ErrorResult(fmt.Errorf("scope amendment retry exhausted")), nil
}

func scopeAdditions(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values []string
	switch v := raw.(type) {
	case []string:
		values = v
	case []any:
		for _, entry := range v {
			str, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("must be an array of non-empty strings")
			}
			values = append(values, str)
		}
	default:
		return nil, fmt.Errorf("must be an array of non-empty strings")
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("must contain non-empty strings")
		}
	}
	return values, nil
}

// filterScopeFiles keeps changelog writes out of declared overlap envelopes.
// Validate before cleaning so a trailing slash cannot masquerade as a file.
func filterScopeFiles(files []string) ([]string, []string, error) {
	kept, dropped := []string{}, []string{}
	for _, file := range files {
		file = strings.TrimSpace(file)
		if err := validateScopeFile(file); err != nil {
			return nil, nil, err
		}
		if strings.HasPrefix(path.Clean(file), "changelog.d/") {
			dropped = append(dropped, file)
			continue
		}
		kept = append(kept, file)
	}
	return kept, dropped, nil
}

func emptyScopeResult(dropped []string) (*mcp.CallToolResult, error) {
	result, err := mcp.JSONResult(map[string]any{
		"error":         "slice grounding must name at least one file after dropping changelog fragments",
		"dropped_files": dropped,
	})
	if result != nil {
		result.IsError = true
	}
	return result, err
}

// Equal directories do not warn unless they are a known broad package root.
func scopeEnvelopeWarnings(files []string) []string {
	warnings := []string{}
	seen := map[string]bool{}
	for _, file := range files {
		dir := path.Dir(path.Clean(file))
		broad := dir == "pkg/mills" || dir == "internal/hud" || dir == "cmd"
		for _, other := range files {
			otherDir := path.Dir(path.Clean(other))
			if otherDir != dir && (strings.HasPrefix(otherDir, dir+"/") || dir == ".") {
				broad = true
			}
		}
		if broad && !seen[file] {
			warnings = append(warnings, fmt.Sprintf("%s creates directory envelope %q and serializes the whole tree; move root-level edits into a separate slice or accept the serialization knowingly.", file, dir))
			seen[file] = true
		}
	}
	return warnings
}

func validateScopeFile(file string) error {
	refuse := func() error {
		return fmt.Errorf("%q is a bare directory or unknown extensionless path; name a file or file pattern", file)
	}
	if strings.HasSuffix(file, "/") {
		return refuse()
	}
	if info, err := os.Stat(file); err == nil {
		if info.IsDir() {
			return refuse()
		}
		return nil
	}
	if _, err := path.Match(file, ""); err != nil {
		return fmt.Errorf("invalid file pattern %q: %w", file, err)
	}
	base := path.Base(file)
	if path.Ext(base) != "" || (strings.ContainsAny(base, "*?[") && base != "**") {
		return nil
	}
	return refuse()
}

func appendScopeValues(raw any, additions []string) ([]string, []string) {
	values := stringSlice(raw)
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		seen[value] = true
	}
	added := []string{}
	for _, value := range additions {
		if !seen[value] {
			values = append(values, value)
			added = append(added, value)
			seen[value] = true
		}
	}
	return values, added
}

func (s *millsServer) guardScopeRequeue(ctx context.Context, project, id string) error {
	branches, err := s.brancher.ImplementBranches(ctx, project, id)
	if err != nil {
		return fmt.Errorf("requeue refused: branch probe failed: %w", err)
	}
	rescue, canProbe := s.brancher.(interface {
		ScopeRescueBranch(context.Context, string, string) (bool, error)
	})
	for _, branch := range branches {
		if canProbe {
			ok, err := rescue.ScopeRescueBranch(ctx, project, branch)
			if err != nil {
				return fmt.Errorf("requeue refused: scope-rescue MR probe failed: %w", err)
			}
			if ok {
				continue
			}
		}
		return fmt.Errorf("requeue refused: implement branch exists (%s) without a matching open scope-rescue draft MR; finish the branch or pass force=true", branch)
	}
	return nil
}
