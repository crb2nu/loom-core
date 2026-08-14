package agentcontext

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/vendorsessions"
)

// vendorSessionStore resolves the vendor transcript roots, preferring
// explicit config over the conventional $HOME locations.
func vendorSessionStore(cfg Config) vendorsessions.Store {
	store := vendorsessions.DefaultStore()
	if cfg.ClaudeSessionsDir != "" {
		store.ClaudeRoot = cfg.ClaudeSessionsDir
	}
	if cfg.CodexSessionsDir != "" {
		store.CodexRoot = cfg.CodexSessionsDir
	}
	return store
}

func (s *Service) HandleMessageSend(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.messages.HandleMessageSend(ctx, args)
}

func (s *Service) HandleMessageInbox(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.messages.HandleMessageInbox(ctx, args)
}

func (s *Service) HandleVendorSessionList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.vendorSess.HandleVendorSessionList(ctx, args)
}

func (s *Service) HandleVendorSessionSearch(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.vendorSess.HandleVendorSessionSearch(ctx, args)
}

func (s *Service) HandleVendorSessionTail(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.vendorSess.HandleVendorSessionTail(ctx, args)
}

func (s *Service) HandleMRStatus(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.mrStatus.HandleMRStatus(ctx, args)
}
