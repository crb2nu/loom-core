package daemon

import (
	"context"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

type toolSourceKind uint8

const (
	toolSourceLocal toolSourceKind = iota
	toolSourceHub
)

type toolSource struct {
	name string
	kind toolSourceKind
}

type toolFetchResult struct {
	name  string
	tools []mcp.Tool
	err   error
}

func fetchToolsBounded(
	ctx context.Context,
	sources []toolSource,
	limit int,
	fetch func(context.Context, toolSource) ([]mcp.Tool, error),
) []toolFetchResult {
	if limit <= 0 {
		limit = 1
	}

	sem := make(chan struct{}, limit)
	results := make([]toolFetchResult, len(sources))
	type fetchOutcome struct {
		idx    int
		result toolFetchResult
	}
	outcomes := make(chan fetchOutcome, len(sources))

	for i, src := range sources {
		go func(idx int, source toolSource) {
			// Acquire slot or abort if context is cancelled.
			select {
			case sem <- struct{}{}:
				// ok
			case <-ctx.Done():
				outcomes <- fetchOutcome{
					idx: idx,
					result: toolFetchResult{
						name: source.name,
						err:  ctx.Err(),
					},
				}
				return
			}
			defer func() { <-sem }()

			tools, err := fetch(ctx, source)
			outcomes <- fetchOutcome{
				idx: idx,
				result: toolFetchResult{
					name:  source.name,
					tools: tools,
					err:   err,
				},
			}
		}(i, src)
	}

	// Join every worker even after cancellation. Waiting workers observe ctx at
	// the semaphore; active fetches receive the same ctx and must unwind before
	// the refresh leader is considered stopped.
	for remaining := len(sources); remaining > 0; remaining-- {
		outcome := <-outcomes
		results[outcome.idx] = outcome.result
	}

	return results
}
