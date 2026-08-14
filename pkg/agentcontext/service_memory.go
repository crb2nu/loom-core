package agentcontext

import (
	"context"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

type MemorySvc struct{ *Service }

// GetMemoryHierarchy returns the memory hierarchy for direct access
func (s *MemorySvc) GetMemoryHierarchy() *MemoryHierarchy {
	return s.memoryHierarchy
}

// HandleMemoryAdd adds items to the memory hierarchy
func (s *MemorySvc) HandleMemoryAdd(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	sessionID := v.String("session_id", "")
	agentID := v.String("agent_id", s.cfg.DefaultAgentID)
	namespace := v.String("namespace", s.cfg.DefaultNamespace)
	itemsRaw := v.RequiredAny("items")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	itemsArr, ok := itemsRaw.([]any)
	if !ok || len(itemsArr) == 0 {
		return mcp.ErrorResult(fmt.Errorf("items array is required")), nil
	}

	var addedIDs []string
	for i, itemRaw := range itemsArr {
		itemMap, ok := itemRaw.(map[string]any)
		if !ok {
			return mcp.ErrorResult(fmt.Errorf("item %d must be an object", i)), nil
		}

		item := &MemoryItem{
			ID:         toString(itemMap["id"]),
			Tier:       MemoryTier(toString(itemMap["tier"])),
			Importance: ImportanceLevel(toString(itemMap["importance"])),
			Title:      toString(itemMap["title"]),
			Content:    toString(itemMap["content"]),
			Category:   toString(itemMap["category"]),
			Namespace:  namespace,
			SessionID:  sessionID,
			AgentID:    agentID,
		}

		// Parse tags
		if tags, ok := itemMap["tags"].([]any); ok {
			for _, t := range tags {
				if ts := toString(t); ts != "" {
					item.Tags = append(item.Tags, ts)
				}
			}
		}

		// Parse metadata
		if metadata, ok := itemMap["metadata"].(map[string]any); ok {
			item.Metadata = metadata
		}

		// Parse related_ids
		if related, ok := itemMap["related_ids"].([]any); ok {
			for _, r := range related {
				if rs := toString(r); rs != "" {
					item.RelatedIDs = append(item.RelatedIDs, rs)
				}
			}
		}

		if err := s.persistedMemoryHierarchy.AddItemWithPersistence(ctx, item, nil); err != nil {
			return mcp.ErrorResult(fmt.Errorf("failed to add item %d: %w", i, err)), nil
		}

		addedIDs = append(addedIDs, item.ID)
	}

	// Update tier-specific metrics
	for _, itemRaw := range itemsArr {
		if im, ok := itemRaw.(map[string]any); ok {
			tokens := int64(EstimateTokens(toString(im["content"])))
			switch MemoryTier(toString(im["tier"])) {
			case MemoryTierWorking:
				s.metrics.WorkingMemoryItems.Add(1)
				s.metrics.WorkingMemoryTokens.Add(tokens)
			case MemoryTierLongTerm:
				s.metrics.LongTermMemoryItems.Add(1)
				s.metrics.LongTermMemoryTokens.Add(tokens)
			default: // short_term or unspecified
				s.metrics.ShortTermMemoryItems.Add(1)
				s.metrics.ShortTermMemoryTokens.Add(tokens)
			}
		}
	}

	return mcp.JSONResult(map[string]any{
		"ok":       true,
		"count":    len(addedIDs),
		"item_ids": addedIDs,
	})
}

// HandleMemoryGet retrieves memory items by ID
func (s *MemorySvc) HandleMemoryGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	itemIDs := v.RequiredStringSlice("item_ids")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	var items []map[string]any
	for _, id := range itemIDs {
		item, err := s.memoryHierarchy.GetItem(id)
		if err != nil {
			continue // Skip not found
		}
		items = append(items, memoryItemToMap(item))
	}

	return mcp.JSONResult(map[string]any{
		"ok":    true,
		"count": len(items),
		"items": items,
	})
}

// HandleMemoryRecall recalls memories matching criteria
func (s *MemorySvc) HandleMemoryRecall(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	query := v.String("query", "")
	namespace := v.String("namespace", "")
	sessionID := v.String("session_id", "")
	agentID := v.String("agent_id", "")
	tokenBudget := v.Int("token_budget", 8000)
	limit := v.Int("limit", 100)
	minImportance := v.Float("min_importance", 0)
	categories := v.StringSlice("categories")
	tags := v.StringSlice("tags")

	req := MemoryRecallRequest{
		Query:         query,
		Namespace:     namespace,
		SessionID:     sessionID,
		AgentID:       agentID,
		TokenBudget:   tokenBudget,
		Limit:         limit,
		MinImportance: minImportance,
		Categories:    categories,
		Tags:          tags,
	}

	// Parse tiers
	if tiers, ok := args["tiers"].([]any); ok {
		for _, t := range tiers {
			if ts, ok := t.(string); ok && ts != "" {
				req.Tiers = append(req.Tiers, MemoryTier(ts))
			}
		}
	}

	result, err := s.memoryHierarchy.Recall(req)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}

	items := make([]map[string]any, len(result.Items))
	for i, item := range result.Items {
		items[i] = memoryItemToMap(&item)
	}

	return mcp.JSONResult(map[string]any{
		"ok":           true,
		"count":        len(items),
		"items":        items,
		"by_tier":      result.ByTier,
		"total_tokens": result.TotalTokens,
		"truncated":    result.Truncated,
	})
}

// HandleMemoryDelete deletes memory items
func (s *MemorySvc) HandleMemoryDelete(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	itemIDs := v.RequiredStringSlice("item_ids")
	confirm := v.Bool("confirm", false)

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	if !confirm {
		return mcp.ErrorResult(fmt.Errorf("confirm must be true to delete items")), nil
	}

	var deleted []string
	for _, id := range itemIDs {
		if err := s.persistedMemoryHierarchy.DeleteItemWithPersistence(ctx, id); err == nil {
			deleted = append(deleted, id)
		}
	}

	return mcp.JSONResult(map[string]any{
		"ok":      true,
		"deleted": deleted,
	})
}

// HandleMemoryPromote promotes items to a higher tier
func (s *MemorySvc) HandleMemoryPromote(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	itemIDs := v.RequiredStringSlice("item_ids")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	var promoted []string
	var errors []string
	for _, id := range itemIDs {
		if err := s.persistedMemoryHierarchy.PromoteItemWithPersistence(ctx, id); err == nil {
			promoted = append(promoted, id)
		} else {
			errors = append(errors, fmt.Sprintf("%s: %v", id, err))
		}
	}

	result := map[string]any{
		"ok":       true,
		"promoted": promoted,
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	return mcp.JSONResult(result)
}

// HandleMemoryDemote demotes items to a lower tier
func (s *MemorySvc) HandleMemoryDemote(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	itemIDs := v.RequiredStringSlice("item_ids")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	var demoted []string
	var errors []string
	for _, id := range itemIDs {
		if err := s.persistedMemoryHierarchy.DemoteItemWithPersistence(ctx, id); err == nil {
			demoted = append(demoted, id)
		} else {
			errors = append(errors, fmt.Sprintf("%s: %v", id, err))
		}
	}

	result := map[string]any{
		"ok":      true,
		"demoted": demoted,
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	return mcp.JSONResult(result)
}

// HandleMemoryStats returns memory hierarchy statistics
func (s *MemorySvc) HandleMemoryStats(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	stats := s.memoryHierarchy.Stats()

	return mcp.JSONResult(map[string]any{
		"ok":                        true,
		"total_items":               stats.TotalItems,
		"total_tokens":              stats.TotalTokens,
		"compression_ratio":         stats.CompressionRatio,
		"items_added_last_24h":      stats.ItemsAddedLast24h,
		"items_compressed_last_24h": stats.ItemsCompressedLast24h,
		"working_memory": map[string]any{
			"item_count":     stats.WorkingMemory.ItemCount,
			"token_count":    stats.WorkingMemory.TokenCount,
			"avg_importance": stats.WorkingMemory.AvgImportance,
			"by_category":    stats.WorkingMemory.ByCategory,
			"by_importance":  stats.WorkingMemory.ByImportance,
		},
		"short_term_memory": map[string]any{
			"item_count":     stats.ShortTermMemory.ItemCount,
			"token_count":    stats.ShortTermMemory.TokenCount,
			"avg_importance": stats.ShortTermMemory.AvgImportance,
			"by_category":    stats.ShortTermMemory.ByCategory,
			"by_importance":  stats.ShortTermMemory.ByImportance,
		},
		"long_term_memory": map[string]any{
			"item_count":     stats.LongTermMemory.ItemCount,
			"token_count":    stats.LongTermMemory.TokenCount,
			"avg_importance": stats.LongTermMemory.AvgImportance,
			"by_category":    stats.LongTermMemory.ByCategory,
			"by_importance":  stats.LongTermMemory.ByImportance,
		},
	})
}

// HandleMemoryPolicyGet returns retention policy for a tier
func (s *MemorySvc) HandleMemoryPolicyGet(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	tierStr := v.Required("tier")

	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	tier := MemoryTier(tierStr)

	policy := s.memoryHierarchy.GetRetentionPolicy(tier)
	if policy == nil {
		return mcp.ErrorResult(fmt.Errorf("no policy for tier: %s", tier)), nil
	}

	return mcp.JSONResult(map[string]any{
		"ok": true,
		"policy": map[string]any{
			"id":                     policy.ID,
			"name":                   policy.Name,
			"tier":                   string(policy.Tier),
			"default_ttl_hours":      policy.DefaultTTL,
			"compress_after_hours":   policy.CompressAfterHours,
			"compression_ratio":      policy.CompressionRatio,
			"merge_threshold":        policy.MergeThreshold,
			"promotion_threshold":    policy.PromotionThreshold,
			"demotion_threshold":     policy.DemotionThreshold,
			"access_count_threshold": policy.AccessCountThreshold,
			"max_items":              policy.MaxItems,
			"max_tokens":             policy.MaxTokens,
			"dedupe_enabled":         policy.DedupeEnabled,
			"dedupe_similarity":      policy.DedupeSimilarity,
		},
	})
}

// Helper function for memory items
func memoryItemToMap(item *MemoryItem) map[string]any {
	m := map[string]any{
		"id":               item.ID,
		"tier":             string(item.Tier),
		"status":           string(item.Status),
		"importance":       string(item.Importance),
		"importance_score": item.ImportanceScore,
		"title":            item.Title,
		"content":          item.Content,
		"category":         item.Category,
		"original_tokens":  item.OriginalTokens,
		"access_count":     item.AccessCount,
		"created_at":       item.CreatedAt.Format(time.RFC3339Nano),
		"last_accessed_at": item.LastAccessedAt.Format(time.RFC3339Nano),
	}

	if item.Summary != "" {
		m["summary"] = item.Summary
		m["compressed_tokens"] = item.CompressedTokens
	}
	if item.Namespace != "" {
		m["namespace"] = item.Namespace
	}
	if item.SessionID != "" {
		m["session_id"] = item.SessionID
	}
	if item.AgentID != "" {
		m["agent_id"] = item.AgentID
	}
	if len(item.Tags) > 0 {
		m["tags"] = item.Tags
	}
	if item.Metadata != nil {
		m["metadata"] = item.Metadata
	}
	if len(item.RelatedIDs) > 0 {
		m["related_ids"] = item.RelatedIDs
	}
	if item.ExpiresAt != nil {
		m["expires_at"] = item.ExpiresAt.Format(time.RFC3339Nano)
	}
	if item.CompressedAt != nil {
		m["compressed_at"] = item.CompressedAt.Format(time.RFC3339Nano)
	}

	return m
}
