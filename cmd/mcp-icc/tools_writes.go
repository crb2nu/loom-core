package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/iccclient"
	"github.com/crb2nu/loom/pkg/validate"
)

// All write tools accept a "payload" map[string]any that is forwarded
// verbatim to the ICC backend. The backend's store.* helpers do
// schema-shaped validation; mirroring those rules in Go would add
// drift risk for low value. Tools that template ids into the URL
// (e.g. demote_artifact's /api/artifacts/<id>/demote) pull the id
// out as a separate required arg.
//
// Every handler returned by these factories is wrapped with the
// writes-enabled gate by the central registerTools() in tools.go.
// Tools should NOT check the gate themselves; the wrapper means a
// caller-visible 403-style "writes_disabled" error is returned before
// any network I/O happens.

// payloadFromArgs extracts a "payload" object from MCP args. Callers
// that don't provide payload get an empty map (backend will 400).
func payloadFromArgs(args map[string]any) (map[string]any, error) {
	if raw, ok := args["payload"]; ok {
		if m, ok := raw.(map[string]any); ok {
			return m, nil
		}
		return nil, errors.New("payload: must be an object")
	}
	return map[string]any{}, nil
}

// payloadSchema is the shared input schema for "create *" tools that
// take a free-form payload. extraRequired lists keys the caller must
// also provide at the top level (e.g. "id" for update tools).
func payloadSchema(description string, extraRequired ...string) mcp.InputSchema {
	required := append([]string{"payload"}, extraRequired...)
	props := map[string]any{
		"payload": map[string]any{
			"type":        "object",
			"description": description,
		},
	}
	for _, r := range extraRequired {
		if r == "payload" {
			continue
		}
		props[r] = map[string]any{"type": "string"}
	}
	return mcp.InputSchema{
		Type:       "object",
		Required:   required,
		Properties: props,
	}
}

// makeCreateHandler builds a write-tool handler that POSTs the
// caller-supplied payload to a fixed ICC path and returns the
// envelope's result verbatim.
func makeCreateHandler(icc *iccclient.Client, label, path string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		payload, err := payloadFromArgs(args)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		_, result, err := postJSON[json.RawMessage](ctx, icc, path, payload)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(result)
	}
}

// makeIDPayloadHandler is the variant that requires an `id` arg AND
// folds it into the payload under "id" before POSTing (mirrors the
// backend's "id"-in-payload convention for /update + /transition +
// /delete style routes).
func makeIDPayloadHandler(icc *iccclient.Client, label, path, idKey string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		id := v.Required("id")
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		if strings.TrimSpace(id) == "" {
			return mcp.ErrorResult(errors.New("id must not be empty")), nil
		}
		payload, err := payloadFromArgs(args)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		// Inject the id under the configured key so payloads that
		// omitted it still match the backend's pop("id") expectation.
		// If the caller already set it, theirs wins (consistent with
		// "you asked for it, you get it" semantics).
		if _, ok := payload[idKey]; !ok {
			payload[idKey] = id
		}
		_, result, err := postJSON[json.RawMessage](ctx, icc, path, payload)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(result)
	}
}

// makeIDInURLHandler templates a required id segment into the URL
// (e.g. /api/artifacts/<id>/demote). The remaining payload is
// forwarded verbatim. urlBuilder receives the validated id and
// returns the full path.
func makeIDInURLHandler(icc *iccclient.Client, label, idArg string, urlBuilder func(id string) string) iccToolHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		v := validate.NewArgs(args)
		id := v.Required(idArg)
		if err := v.Validate(); err != nil {
			return mcp.ErrorResult(err), nil
		}
		if strings.TrimSpace(id) == "" {
			return mcp.ErrorResult(fmt.Errorf("%s must not be empty", idArg)), nil
		}
		payload, err := payloadFromArgs(args)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
		_, result, err := postJSON[json.RawMessage](ctx, icc, urlBuilder(id), payload)
		if err != nil {
			return mcp.ErrorResult(fmt.Errorf("%s: %w", label, err)), nil
		}
		return jsonResult(result)
	}
}
