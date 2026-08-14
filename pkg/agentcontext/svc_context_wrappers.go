package agentcontext

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// Context handlers — thin delegation to ContextSvc.

func (s *Service) HandleContextAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.ctxSvc.Add(ctx, args)
}

func (s *Service) HandleContextSearch(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.ctxSvc.Search(ctx, args)
}

func (s *Service) HandleContextSummarize(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.ctxSvc.Summarize(ctx, args)
}

func (s *Service) HandleAnnotationAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.ctxSvc.AnnotationAdd(ctx, args)
}
