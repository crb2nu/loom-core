package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// engramGraphCaller serves the full-catalog agent_engram_graph payload the
// bridge requests with empty args and counts every upstream call, so tests can
// pin how many fetches a sequence of bridge methods costs. Any other tool is a
// failure: the summary must roll up the graph, not fall back to
// agent_engram_list.
func engramGraphCaller(t *testing.T, graph map[string]any, calls *atomic.Int32) *stubCaller {
	t.Helper()
	return &stubCaller{callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
		if name != "agent_context__agent_engram_graph" {
			return nil, fmt.Errorf("unexpected tool %s: the summary must roll up the graph fetch", name)
		}
		if len(args) != 0 {
			return nil, fmt.Errorf("full-catalog graph must be requested with empty args, got %v", args)
		}
		calls.Add(1)
		return toolPayload(t, graph), nil
	}}
}

// fullGraphFixture mirrors fullEngramGraph's payload: rich nodes keyed by URI
// plus one stub for a dangling prerequisite (a gap in the tree, not an engram).
func fullGraphFixture() map[string]any {
	return map[string]any{
		"ok": true, "root": "", "direction": "down", "truncated": false,
		"nodes": []any{
			map[string]any{"id": "engram://a/x", "uri": "engram://a/x", "title": "A", "tier": 1, "proof_status": "verified", "prerequisites": []string{}, "content": "", "proof": "a.go:1", "last_verified": ""},
			map[string]any{"id": "engram://b/x", "uri": "engram://b/x", "title": "B", "tier": 2, "proof_status": "verified", "prerequisites": []string{"engram://a/x"}, "content": "", "proof": "", "last_verified": ""},
			map[string]any{"id": "engram://c/x", "uri": "engram://c/x", "title": "C", "tier": 1, "proof_status": "stale", "prerequisites": []string{}, "content": "", "proof": "", "last_verified": ""},
			map[string]any{"id": "engram://d/x", "uri": "engram://d/x", "title": "D", "tier": 3, "proof_status": "failing", "prerequisites": []string{"engram://gone/x"}, "content": "", "proof": "", "last_verified": ""},
			map[string]any{"id": "engram://gone/x", "uri": "engram://gone/x", "title": "", "tier": 1, "proof_status": "unverified", "prerequisites": []string{}, "stub": true},
		},
		"edges": []any{
			map[string]any{"from": "engram://b/x", "to": "engram://a/x"},
			map[string]any{"from": "engram://d/x", "to": "engram://gone/x"},
		},
	}
}

func TestAgentBridge_EngramSummary_AggregatesCounts(t *testing.T) {
	var calls atomic.Int32
	b := NewAgentBridge(engramGraphCaller(t, fullGraphFixture(), &calls))
	got, err := b.EngramSummary()
	if err != nil {
		t.Fatalf("summary failed: %v", err)
	}

	if got.Total != 4 {
		t.Errorf("total: got %d want 4 (four engrams; the stub is not one)", got.Total)
	}
	if got.ByStatus["verified"] != 2 {
		t.Errorf("verified: got %d want 2", got.ByStatus["verified"])
	}
	if got.ByStatus["stale"] != 1 {
		t.Errorf("stale: got %d want 1", got.ByStatus["stale"])
	}
	if got.ByStatus["failing"] != 1 {
		t.Errorf("failing: got %d want 1", got.ByStatus["failing"])
	}
	if n, ok := got.ByStatus["unverified"]; !ok || n != 0 {
		t.Errorf("unverified: got %d (present=%v) want 0 (key must be present even when zero)", n, ok)
	}
	if got.ByTier["tier:1"] != 2 || got.ByTier["tier:2"] != 1 || got.ByTier["tier:3"] != 1 || len(got.ByTier) != 3 {
		t.Errorf("by_tier wrong: %+v", got.ByTier)
	}
	if got.Degraded {
		t.Error("a bridge-backed summary is never degraded")
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

// TestAgentBridge_EngramSummary_ExcludesStubNodes: the stub stays in the graph
// (the tree renders the gap) but never reaches the wire as a marker, and the
// summary does not count it as an unverified engram.
func TestAgentBridge_EngramSummary_ExcludesStubNodes(t *testing.T) {
	var calls atomic.Int32
	b := NewAgentBridge(engramGraphCaller(t, fullGraphFixture(), &calls))

	graph, err := b.EngramGraph()
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 5 || !graph.Nodes[4].Stub {
		t.Fatalf("graph must keep the stub node: %#v", graph.Nodes)
	}
	for _, node := range graph.Nodes[:4] {
		if node.Stub {
			t.Fatalf("catalog node %s flagged as stub", node.ID)
		}
	}
	wire, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"stub"`)) {
		t.Fatalf("stub marker leaked onto the graph wire: %s", wire)
	}

	summary, err := b.EngramSummary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 4 || summary.ByStatus["unverified"] != 0 {
		t.Fatalf("stub counted as an engram: %+v", summary)
	}
}

func TestAgentBridge_EngramSummary_LegacyNodesDefaultUnverifiedTierOne(t *testing.T) {
	var calls atomic.Int32
	// Older servers: URI-only nodes, and rich nodes without proof_status/tier.
	b := NewAgentBridge(engramGraphCaller(t, map[string]any{
		"nodes": []any{"engram://legacy/x", map[string]any{"uri": "engram://legacy/y"}},
		"edges": []any{},
	}, &calls))
	got, err := b.EngramSummary()
	if err != nil {
		t.Fatalf("summary failed: %v", err)
	}
	if got.Total != 2 || got.ByStatus["unverified"] != 2 {
		t.Errorf("legacy nodes should default to unverified; got %+v", got)
	}
	if got.ByTier["tier:1"] != 2 || len(got.ByTier) != 1 {
		t.Errorf("an unset tier folds into DefaultEngramTier, not a tier:0 bucket; got %+v", got.ByTier)
	}
}

func TestAgentBridge_EngramSummary_EmptyLibrary(t *testing.T) {
	var calls atomic.Int32
	b := NewAgentBridge(engramGraphCaller(t, map[string]any{"nodes": []any{}, "edges": []any{}}, &calls))
	got, err := b.EngramSummary()
	if err != nil {
		t.Fatalf("summary failed: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("total: got %d want 0", got.Total)
	}
	for _, k := range []string{"unverified", "verified", "stale", "failing"} {
		if n, ok := got.ByStatus[k]; !ok || n != 0 {
			t.Errorf("by_status[%s] should be present and 0; got %d (present=%v)", k, n, ok)
		}
	}
	if got.ByTier == nil || len(got.ByTier) != 0 {
		t.Errorf("by_tier should be a non-nil empty map; got %#v", got.ByTier)
	}
}

// TestAgentBridge_EngramGraphAndSummary_ShareOneFetch pins the request budget
// the HUD relies on: the engrams store asks for the graph and the summary
// together, and one agent_engram_graph fetch must serve both in either order.
func TestAgentBridge_EngramGraphAndSummary_ShareOneFetch(t *testing.T) {
	for _, order := range []string{"graph then summary", "summary then graph"} {
		t.Run(order, func(t *testing.T) {
			var calls atomic.Int32
			b := NewAgentBridge(engramGraphCaller(t, fullGraphFixture(), &calls))

			var (
				graph   *EngramGraphResult
				summary *EngramSummaryResult
				err     error
			)
			if order == "graph then summary" {
				if graph, err = b.EngramGraph(); err != nil {
					t.Fatal(err)
				}
				if summary, err = b.EngramSummary(); err != nil {
					t.Fatal(err)
				}
			} else {
				if summary, err = b.EngramSummary(); err != nil {
					t.Fatal(err)
				}
				if graph, err = b.EngramGraph(); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1: one agent_engram_graph fetch must serve both", calls.Load())
			}
			// The summary describes exactly the graph that was served.
			engrams := 0
			for _, node := range graph.Nodes {
				if !node.Stub {
					engrams++
				}
			}
			if summary.Total != engrams {
				t.Fatalf("summary total %d != %d non-stub graph nodes", summary.Total, engrams)
			}
		})
	}
}

// TestAgentBridge_EngramGraph_ConcurrentCallersCoalesce: the two HUD requests
// arrive within the same instant (Promise.all), so overlapping callers must
// share one in-flight upstream call rather than each starting their own.
func TestAgentBridge_EngramGraph_ConcurrentCallersCoalesce(t *testing.T) {
	const callers = 8
	var (
		calls   atomic.Int32
		started sync.WaitGroup
		release = make(chan struct{})
	)
	started.Add(callers)
	caller := &stubCaller{callToolFn: func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "agent_context__agent_engram_graph" {
			return nil, fmt.Errorf("unexpected tool %s", name)
		}
		calls.Add(1)
		// Hold the leader's fetch open until every caller is in play.
		<-release
		return toolPayload(t, fullGraphFixture()), nil
	}}
	b := NewAgentBridge(caller)

	var done sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			started.Done()
			var err error
			if i%2 == 0 {
				_, err = b.EngramGraph()
			} else {
				_, err = b.EngramSummary()
			}
			errs <- err
		}(i)
	}
	started.Wait()
	close(release)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 for %d overlapping callers", calls.Load(), callers)
	}
}

// TestAgentBridge_EngramGraph_TTLBoundsReuse: the shared result is a short
// window, not a cache — the next poll cycle must observe a fresh catalog, and
// a non-positive TTL keeps only the in-flight coalescing.
func TestAgentBridge_EngramGraph_TTLBoundsReuse(t *testing.T) {
	t.Run("window expires", func(t *testing.T) {
		var calls atomic.Int32
		b := NewAgentBridge(engramGraphCaller(t, fullGraphFixture(), &calls))
		b.engramGraphTTL = 10 * time.Millisecond

		if _, err := b.EngramGraph(); err != nil {
			t.Fatal(err)
		}
		if _, err := b.EngramSummary(); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1 inside the reuse window", calls.Load())
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := b.EngramSummary(); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls = %d, want 2: the window expired, the next cycle must refetch", calls.Load())
		}
	})

	t.Run("non-positive TTL disables time-based reuse", func(t *testing.T) {
		var calls atomic.Int32
		b := NewAgentBridge(engramGraphCaller(t, fullGraphFixture(), &calls))
		b.engramGraphTTL = 0

		if _, err := b.EngramGraph(); err != nil {
			t.Fatal(err)
		}
		if _, err := b.EngramSummary(); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls = %d, want 2: sequential callers must each fetch when only in-flight coalescing is on", calls.Load())
		}
	})
}

// TestAgentBridge_EngramGraph_ErrorIsNotCached: a failed fetch must not poison
// the shared result; the next caller retries.
func TestAgentBridge_EngramGraph_ErrorIsNotCached(t *testing.T) {
	var (
		calls atomic.Int32
		fail  atomic.Bool
	)
	fail.Store(true)
	caller := &stubCaller{callToolFn: func(name string, _ map[string]any) (json.RawMessage, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("offline")
		}
		return toolPayload(t, fullGraphFixture()), nil
	}}
	b := NewAgentBridge(caller)

	if _, err := b.EngramSummary(); err == nil {
		t.Fatal("expected the upstream error to surface")
	}
	fail.Store(false)
	got, err := b.EngramSummary()
	if err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got.Total != 4 || calls.Load() != 2 {
		t.Fatalf("retry did not refetch: total=%d calls=%d", got.Total, calls.Load())
	}
}
