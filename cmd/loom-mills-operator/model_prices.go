package main

import (
	"log/slog"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

// applyPolicyModelPrices pushes budgets.model_prices into the clients price
// overlay. Called once at boot and again on every policy reload, so a new
// model can be priced (or a list price corrected) by editing the gitops
// policy — no image deploy, no restart. The overlay is swapped whole, so a
// row removed from policy falls back to the compiled table on reload.
func applyPolicyModelPrices(pol *mills.Policy, logger *slog.Logger) {
	rows := pol.ModelPriceOverrides()
	prices := make(map[string]clients.ModelPrice, len(rows))
	for id, r := range rows {
		prices[id] = clients.ModelPrice{
			Provider:              r.Provider,
			InputPerMillion:       r.InputPerMillion,
			CachedInputPerMillion: r.CachedInputPerMillion,
			CacheWritePerMillion:  r.CacheWritePerMillion,
			OutputPerMillion:      r.OutputPerMillion,
		}
	}
	clients.RegisterModelPrices(prices)
	if logger != nil && len(prices) > 0 {
		ids := make([]string, 0, len(prices))
		for id := range prices {
			ids = append(ids, id)
		}
		logger.Info("policy model prices applied", "count", len(prices), "models", ids)
	}
}
