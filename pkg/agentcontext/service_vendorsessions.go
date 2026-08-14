package agentcontext

import (
	"context"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
	"github.com/crb2nu/loom/pkg/vendorsessions"
)

// VendorSessionsSvc exposes read-only list/search over the on-disk session
// transcripts of the vendor CLIs (Claude Code, Codex) running on this host.
// It gives every agent a unified window into what the *other* vendor's
// sessions did, without either vendor needing the other's native tooling.
type VendorSessionsSvc struct {
	*Service
	store vendorsessions.Store
}

func (s *VendorSessionsSvc) listOptions(v *validate.Args) vendorsessions.ListOptions {
	opts := vendorsessions.ListOptions{
		Vendor:      v.String("vendor", ""),
		CwdContains: v.String("cwd_contains", ""),
		Limit:       v.Int("limit", 0),
	}
	if h := v.Int("since_hours", 0); h > 0 {
		opts.Since = time.Now().Add(-time.Duration(h) * time.Hour)
	}
	return opts
}

// HandleVendorSessionList lists vendor CLI sessions across claude + codex.
func (s *VendorSessionsSvc) HandleVendorSessionList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	_ = ctx
	v := validate.NewArgs(args)
	opts := s.listOptions(v)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if opts.Vendor != "" && opts.Vendor != vendorsessions.VendorClaude && opts.Vendor != vendorsessions.VendorCodex {
		return mcp.ErrorResult(fmt.Errorf("vendor must be %q or %q", vendorsessions.VendorClaude, vendorsessions.VendorCodex)), nil
	}

	sessions, err := s.store.List(opts)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list vendor sessions: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":       true,
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// HandleVendorSessionSearch greps vendor CLI transcripts for a substring.
func (s *VendorSessionsSvc) HandleVendorSessionSearch(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	_ = ctx
	v := validate.NewArgs(args)
	query := v.Required("query")
	opts := vendorsessions.SearchOptions{
		ListOptions:   s.listOptions(v),
		MaxPerSession: v.Int("max_per_session", 0),
		MaxResults:    v.Int("max_results", 0),
	}
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	opts.Query = query

	matches, err := s.store.Search(opts)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("search vendor sessions: %w", err)), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":      true,
		"query":   query,
		"matches": matches,
		"count":   len(matches),
	})
}

// HandleVendorSessionTail returns a bounded conversational tail for one transcript.
func (s *VendorSessionsSvc) HandleVendorSessionTail(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	_ = ctx
	v := validate.NewArgs(args)
	vendor, id := v.Required("vendor"), v.Required("id")
	maxLines := v.Int("lines", 200)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if vendor != vendorsessions.VendorClaude && vendor != vendorsessions.VendorCodex {
		return mcp.ErrorResult(fmt.Errorf("vendor must be %q or %q", vendorsessions.VendorClaude, vendorsessions.VendorCodex)), nil
	}
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 500 {
		maxLines = 500
	}
	sessions, err := s.store.List(vendorsessions.ListOptions{Vendor: vendor, Limit: 500})
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("list vendor sessions: %w", err)), nil
	}
	for _, sess := range sessions {
		if sess.ID != id {
			continue
		}
		entries := vendorsessions.Tail(sess, vendorsessions.TailOptions{MaxEntries: maxLines})
		if entries == nil {
			return mcp.ErrorResult(fmt.Errorf("vendor session %q transcript is unavailable", id)), nil
		}
		total := len(entries)
		if len(entries) > 0 && entries[len(entries)-1].Line > 0 {
			total = entries[len(entries)-1].Line
		}
		return mcp.JSONResult(map[string]any{
			"lines": entries, "total_lines": total,
			"truncated": total > len(entries) || (len(entries) > 0 && entries[0].Line == 0),
			"degraded":  false,
		})
	}
	return mcp.ErrorResult(fmt.Errorf("vendor session %q not found", id)), nil
}
