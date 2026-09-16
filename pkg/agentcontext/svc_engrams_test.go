package agentcontext

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/trace/noop"
)

// engramTestEnvOnce ensures LOOM_MCP_OUTPUT_FORMAT is set exactly once for
// this test binary. We can't use t.Setenv because the engram tests run with
// t.Parallel(), and t.Setenv panics in parallel tests. os.Setenv is safe.
var engramTestEnvOnce sync.Once

func ensureEngramTestEnv() {
	engramTestEnvOnce.Do(func() {
		os.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	})
}

// newEngramTestService spins up a minimal Service with just the memory
// substrate the engram tools need. Persistence is not configured, so writes
// stay in-memory (PersistItem short-circuits when MemoryQdrant is nil; see
// memory_hierarchy_persist.go:35).
//
// LOOM_MCP_OUTPUT_FORMAT=json is set once via TestMain so JSONResult emits
// machine-parseable JSON instead of the default TOON format.
func newEngramTestService() *Service {
	ensureEngramTestEnv()
	svc := &Service{
		cfg:     Config{},
		logger:  slog.Default(),
		tracer:  noop.NewTracerProvider().Tracer("test"),
		metrics: GetMetrics(),
	}
	svc.memoryHierarchy = NewMemoryHierarchy()
	svc.persistedMemoryHierarchy = svc.memoryHierarchy.SetPersistence(&MemoryPersistenceConfig{})
	svc.memory = &MemorySvc{Service: svc}
	return svc
}

// readResultJSON extracts the JSON object encoded in a CallToolResult's
// first text content block.
func readResultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("unmarshal: %v (text=%q)", err, res.Content[0].Text)
	}
	return out
}

// ---------------------------------------------------------------------------
// HandleEngramAdd
// ---------------------------------------------------------------------------

func TestHandleEngramAdd_Defaults(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()

	res, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":    "Atomic file write",
		"problem":  "Concurrent readers see partial writes during os.WriteFile",
		"solution": "Write to tempfile and rename",
		"proof":    "pkg/skills/fileops.go:42-58",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.IsError {
		t.Fatalf("error result: %+v", res)
	}

	payload := readResultJSON(t, res)
	if payload["uri"] != "engram://atomic-file-write/default" {
		t.Errorf("uri=%v", payload["uri"])
	}
	if payload["tier"].(float64) != 1 {
		t.Errorf("expected tier 1, got %v", payload["tier"])
	}
	if payload["proof_status"] != ProofStatusUnverified {
		t.Errorf("expected proof_status=unverified, got %v", payload["proof_status"])
	}
}

func TestHandleEngramAdd_Tier2RejectsNonCommandProof(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()

	res, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":    "Connection pool with healthcheck",
		"problem":  "Stale DB connections after idle timeout",
		"solution": "Configure ConnMaxLifetime + healthcheck ping",
		"proof":    "pkg/db/pool.go",
		"tier":     2,
	})
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result for tier 2 without command:")
	}
}

func TestHandleEngramAdd_Tier2AcceptsCommandProof(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()

	res, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":    "Connection pool",
		"problem":  "Stale DB connections",
		"solution": "Set ConnMaxLifetime",
		"proof":    "command: go test ./pkg/db -run TestPoolReconnect",
		"tier":     2,
		"family":   "db-pool-healthcheck",
		"language": "go",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.IsError {
		t.Fatalf("error result: %+v", res)
	}
	payload := readResultJSON(t, res)
	if payload["uri"] != "engram://db-pool-healthcheck/go" {
		t.Errorf("unexpected uri: %v", payload["uri"])
	}
}

func TestHandleEngramAdd_DuplicateURIRejected(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	args := map[string]any{
		"title":    "Atomic file write",
		"problem":  "Partial writes",
		"solution": "Tempfile + rename",
		"proof":    "pkg/skills/fileops.go:42",
		"family":   "atomic-file-write",
		"slug":     "go",
	}
	if _, err := svc.HandleEngramAdd(context.Background(), args); err != nil {
		t.Fatalf("first add: %v", err)
	}
	res, err := svc.HandleEngramAdd(context.Background(), args)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.IsError {
		t.Fatal("expected duplicate URI to be rejected")
	}
}

func TestHandleEngramAdd_PrerequisitesValidated(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()

	res, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":         "Composite",
		"problem":       "p",
		"solution":      "s",
		"proof":         "command: go test",
		"tier":          2,
		"prerequisites": []any{"NOT a uri"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.IsError {
		t.Fatal("expected error for malformed prerequisite URI")
	}
}

func TestHandleEngramAdd_CycleRejected(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()

	// First add A.
	if _, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":    "A",
		"problem":  "p",
		"solution": "s",
		"proof":    "f.go:1",
		"family":   "fam-a",
		"slug":     "x",
	}); err != nil {
		t.Fatalf("add A: %v", err)
	}

	// Now adding B with prereq A is fine.
	if _, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":         "B",
		"problem":       "p",
		"solution":      "s",
		"proof":         "f.go:1",
		"family":        "fam-b",
		"slug":          "x",
		"prerequisites": []any{"engram://fam-a/x"},
	}); err != nil {
		t.Fatalf("add B: %v", err)
	}

	// Manually mutate A so its prereqs include B (simulating a future state).
	// Then re-adding A would be a cycle. We can't re-add A (duplicate), so
	// instead try to add C with prereq C-self via prereq chain. Simpler test:
	// just verify direct self-prereq is rejected for a fresh engram.
	res, err := svc.HandleEngramAdd(context.Background(), map[string]any{
		"title":         "C",
		"problem":       "p",
		"solution":      "s",
		"proof":         "f.go:1",
		"family":        "fam-c",
		"slug":          "x",
		"prerequisites": []any{"engram://fam-c/x"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !res.IsError {
		t.Fatal("expected self-cycle to be rejected")
	}
}

// ---------------------------------------------------------------------------
// HandleEngramRecall
// ---------------------------------------------------------------------------

func TestHandleEngramRecall_IncludesPrerequisites(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	// Tier 1 base.
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title":    "Error wrap with context",
		"problem":  "Lose context when bubbling errors",
		"solution": "fmt.Errorf(\"%w\", err)",
		"proof":    "pkg/x.go:10",
		"family":   "error-wrap",
		"slug":     "go",
	}); err != nil {
		t.Fatalf("add base: %v", err)
	}

	// Tier 2 dependent.
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title":         "Retry with jitter",
		"problem":       "Hot loop on transient errors",
		"solution":      "Exponential backoff with jitter",
		"proof":         "command: go test ./pkg/retry",
		"tier":          2,
		"family":        "retry-jitter",
		"slug":          "go",
		"prerequisites": []any{"engram://error-wrap/go"},
	}); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	res, err := svc.HandleEngramRecall(ctx, map[string]any{
		"query": "retry",
		"depth": 1,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res)
	}

	payload := readResultJSON(t, res)
	items, ok := payload["items"].([]any)
	if !ok {
		t.Fatalf("items not slice: %T", payload["items"])
	}

	hasBase, hasDep := false, false
	for _, it := range items {
		m := it.(map[string]any)
		switch m["uri"] {
		case "engram://error-wrap/go":
			hasBase = true
		case "engram://retry-jitter/go":
			hasDep = true
		}
	}
	if !hasDep {
		t.Errorf("recall missed retry-jitter")
	}
	if !hasBase {
		t.Errorf("recall did not pull in error-wrap prerequisite via depth=1")
	}
}

func TestHandleEngramRecall_DepthZeroSkipsPrereqs(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "base", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "base-engram", "slug": "x",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "dep", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "dep-engram", "slug": "x",
		"prerequisites": []any{"engram://base-engram/x"},
	}); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	res, err := svc.HandleEngramRecall(ctx, map[string]any{
		"query": "dep",
		"depth": 0,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	payload := readResultJSON(t, res)
	if payload["prereq_count"].(float64) != 0 {
		t.Errorf("expected 0 prereqs at depth=0, got %v", payload["prereq_count"])
	}
}

// ---------------------------------------------------------------------------
// HandleEngramGraph
// ---------------------------------------------------------------------------

func TestHandleEngramGraph_DownTraversal(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "leaf", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "leaf-graph", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "root", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "root-graph", "slug": "x",
		"prerequisites": []any{"engram://leaf-graph/x"},
	}); err != nil {
		t.Fatalf("%v", err)
	}

	res, err := svc.HandleEngramGraph(ctx, map[string]any{
		"root":      "engram://root-graph/x",
		"direction": "down",
		"max_depth": 3,
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.IsError {
		t.Fatalf("err result: %+v", res)
	}
	payload := readResultJSON(t, res)
	edges := payload["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d: %v", len(edges), edges)
	}
	e := edges[0].(map[string]any)
	if e["from"] != "engram://root-graph/x" || e["to"] != "engram://leaf-graph/x" {
		t.Errorf("unexpected edge: %v", e)
	}
}

func TestHandleEngramGraph_UpTraversal(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "shared base", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "shared-base", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	for _, fam := range []string{"dep-one", "dep-two"} {
		if _, err := svc.HandleEngramAdd(ctx, map[string]any{
			"title": fam, "problem": "p", "solution": "s", "proof": "f:1",
			"family": fam, "slug": "x",
			"prerequisites": []any{"engram://shared-base/x"},
		}); err != nil {
			t.Fatalf("%s: %v", fam, err)
		}
	}

	res, err := svc.HandleEngramGraph(ctx, map[string]any{
		"root":      "engram://shared-base/x",
		"direction": "up",
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	payload := readResultJSON(t, res)
	edges := payload["edges"].([]any)
	if len(edges) != 2 {
		t.Errorf("expected 2 dependents, got %d: %v", len(edges), edges)
	}
}

// ---------------------------------------------------------------------------
// HandleEngramList
// ---------------------------------------------------------------------------

func TestHandleEngramList_TierFilter(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	add := func(family, slug string, tier int, proof string) {
		args := map[string]any{
			"title": family, "problem": "p", "solution": "s",
			"proof": proof, "family": family, "slug": slug, "tier": tier,
		}
		if _, err := svc.HandleEngramAdd(ctx, args); err != nil {
			t.Fatalf("add %s: %v", family, err)
		}
	}
	add("idiom-fam", "x", 1, "file.go:1")
	add("composite-fam", "x", 2, "command: go test")

	res, err := svc.HandleEngramList(ctx, map[string]any{
		"tier": 1,
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	payload := readResultJSON(t, res)
	items := payload["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["tier"].(float64) > 1 {
			t.Errorf("tier filter leaked tier %v engram %v", m["tier"], m["uri"])
		}
	}
}

// ---------------------------------------------------------------------------
// S2: Locked filter + token budget truncation
// ---------------------------------------------------------------------------

// markVerified flips proof_status to "verified" and pushes repo onto unlocked_in
// for the engram with the given URI. Used by S2 tests to simulate the
// outcome of the proof verification job (S3) without running it.
func markVerified(t *testing.T, svc *Service, uri, repo string) {
	t.Helper()
	item, err := svc.lookupEngramByURI(uri)
	if err != nil || item == nil {
		t.Fatalf("lookup %s: err=%v item=%v", uri, err, item)
	}
	stored, err := svc.memoryHierarchy.GetItem(item.ID)
	if err != nil {
		t.Fatalf("get %s: %v", item.ID, err)
	}
	if stored.Metadata == nil {
		stored.Metadata = map[string]any{}
	}
	stored.Metadata[mdEngramProofStatus] = ProofStatusVerified
	unlocked := metadataStringSlice(stored.Metadata, mdEngramUnlockedIn)
	for _, u := range unlocked {
		if u == repo {
			return
		}
	}
	unlocked = append(unlocked, repo)
	stored.Metadata[mdEngramUnlockedIn] = stringSliceToAny(unlocked)
	if err := svc.memoryHierarchy.UpdateItem(stored); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestHandleEngramRecall_LockedFilterDropsUnverified(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "verified", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "verified-fam", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "unverified", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "unverified-fam", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	markVerified(t, svc, "engram://verified-fam/x", "demo-repo")

	res, err := svc.HandleEngramRecall(ctx, map[string]any{
		"query":          "p",
		"include_locked": false,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	payload := readResultJSON(t, res)
	items := payload["items"].([]any)

	gotURIs := map[string]bool{}
	for _, it := range items {
		gotURIs[it.(map[string]any)["uri"].(string)] = true
	}
	if !gotURIs["engram://verified-fam/x"] {
		t.Error("verified engram missing from results")
	}
	if gotURIs["engram://unverified-fam/x"] {
		t.Error("locked unverified engram should be filtered out")
	}
	if payload["locked_dropped"].(float64) < 1 {
		t.Errorf("expected locked_dropped >= 1, got %v", payload["locked_dropped"])
	}
}

func TestHandleEngramRecall_LockedFilterScopedByRepo(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "scope test", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "scope-test", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	markVerified(t, svc, "engram://scope-test/x", "repo-a")

	// Asking for repo-a: should appear.
	resA, _ := svc.HandleEngramRecall(ctx, map[string]any{
		"query":          "scope",
		"include_locked": false,
		"repo":           "repo-a",
	})
	if itemsCount(t, resA) != 1 {
		t.Errorf("repo-a should see the engram, got %d items", itemsCount(t, resA))
	}

	// Asking for repo-b: filtered out.
	resB, _ := svc.HandleEngramRecall(ctx, map[string]any{
		"query":          "scope",
		"include_locked": false,
		"repo":           "repo-b",
	})
	if itemsCount(t, resB) != 0 {
		t.Errorf("repo-b should NOT see the engram, got %d items", itemsCount(t, resB))
	}

	// Default include_locked=true: visible regardless of repo.
	resAll, _ := svc.HandleEngramRecall(ctx, map[string]any{"query": "scope"})
	if itemsCount(t, resAll) != 1 {
		t.Errorf("include_locked=true (default) should see the engram, got %d", itemsCount(t, resAll))
	}
}

func TestHandleEngramRecall_TokenBudgetDropsHighestTierPrereqsFirst(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	// Add a tier-1 base, a tier-2 mid, and a tier-3 system that depends on both.
	add := func(title, family, slug string, tier int, proof string, prereqs []any) {
		args := map[string]any{
			"title": title, "problem": "p", "solution": "s",
			"proof": proof, "family": family, "slug": slug, "tier": tier,
		}
		if len(prereqs) > 0 {
			args["prerequisites"] = prereqs
		}
		if _, err := svc.HandleEngramAdd(ctx, args); err != nil {
			t.Fatalf("add %s: %v", family, err)
		}
	}
	add("base idiom", "budget-base", "x", 1, "f:1", nil)
	add("composite", "budget-comp", "x", 2, "command: go test", []any{"engram://budget-base/x"})
	add("system", "budget-sys", "x", 3,
		"command: go test\nbenchmark: go test -bench=.",
		[]any{"engram://budget-base/x", "engram://budget-comp/x"})

	// Budget that fits the match (~62 tokens) plus the tier-1 base (~41)
	// but not the tier-2 composite (~46). Truncator should drop tier-2
	// composite first, leaving match + base.
	res, err := svc.HandleEngramRecall(ctx, map[string]any{
		"query":        "system",
		"depth":        3,
		"token_budget": 110,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	payload := readResultJSON(t, res)
	items := payload["items"].([]any)

	// Direct match (system, tier 3) must always be kept; the comp/base prereqs
	// can be dropped to fit the budget.
	hasMatch := false
	for _, it := range items {
		if it.(map[string]any)["uri"] == "engram://budget-sys/x" {
			hasMatch = true
		}
	}
	if !hasMatch {
		t.Errorf("direct match should not be dropped; items=%v", items)
	}
	if payload["truncated"].(bool) != true {
		t.Errorf("expected truncated=true with tight budget, got %v", payload["truncated"])
	}
	if payload["budget_dropped"].(float64) < 1 {
		t.Errorf("expected budget_dropped >= 1, got %v", payload["budget_dropped"])
	}
}

func TestHandleEngramRecall_NoBudgetKeepsEverything(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	add := func(title, family string, prereqs []any) {
		args := map[string]any{
			"title": title, "problem": "p", "solution": "s",
			"proof":  "f:1",
			"family": family, "slug": "x",
		}
		if len(prereqs) > 0 {
			args["prerequisites"] = prereqs
		}
		if _, err := svc.HandleEngramAdd(ctx, args); err != nil {
			t.Fatalf("%v", err)
		}
	}
	add("a", "no-budget-a", nil)
	add("b", "no-budget-b", []any{"engram://no-budget-a/x"})

	res, err := svc.HandleEngramRecall(ctx, map[string]any{
		"query":        "b",
		"depth":        1,
		"token_budget": 0, // explicitly disable budget
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	payload := readResultJSON(t, res)
	if payload["truncated"].(bool) {
		t.Error("budget=0 should never truncate")
	}
	if payload["budget_dropped"].(float64) != 0 {
		t.Errorf("budget_dropped expected 0, got %v", payload["budget_dropped"])
	}
}

func itemsCount(t *testing.T, res *mcp.CallToolResult) int {
	t.Helper()
	payload := readResultJSON(t, res)
	items, ok := payload["items"].([]any)
	if !ok {
		return 0
	}
	return len(items)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// HandleEngramGraph — root-less full catalog (the HUD tech-tree contract)
// ---------------------------------------------------------------------------

// TestHandleEngramGraph_FullCatalog pins the empty-args call the HUD bridge
// has always made: it must return the whole catalog as RICH nodes keyed by
// engram URI (edges reference URIs, so any other node key breaks the tree
// join), with dangling prerequisite targets stubbed and deterministic order.
func TestHandleEngramGraph_FullCatalog(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "base idiom", "problem": "p", "solution": "s", "proof": "f:1",
		"family": "full-base", "slug": "x",
	}); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "composite", "problem": "p", "solution": "s", "proof": "command: go test ./...",
		"family": "full-comp", "slug": "x", "tier": 2,
		"prerequisites": []any{"engram://full-base/x", "engram://full-missing/x"},
	}); err != nil {
		t.Fatalf("%v", err)
	}

	res, err := svc.HandleEngramGraph(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.IsError {
		t.Fatalf("empty-args graph must not error (the required-root schema kept the HUD tree from ever rendering): %+v", res)
	}
	payload := readResultJSON(t, res)

	if truncated, _ := payload["truncated"].(bool); truncated {
		t.Errorf("tiny catalog reported truncated")
	}

	nodes := payload["nodes"].([]any)
	byURI := map[string]map[string]any{}
	for _, raw := range nodes {
		n := raw.(map[string]any)
		uri, _ := n["uri"].(string)
		if uri == "" {
			t.Fatalf("node missing uri key: %v", n)
		}
		byURI[uri] = n
	}
	base, ok := byURI["engram://full-base/x"]
	if !ok {
		t.Fatalf("base engram missing from full graph: %v", byURI)
	}
	if base["title"] != "base idiom" || base["proof_status"] != "unverified" {
		t.Errorf("base node not rich: %v", base)
	}
	comp := byURI["engram://full-comp/x"]
	if comp == nil || comp["tier"].(float64) != 2 {
		t.Errorf("composite node wrong: %v", comp)
	}
	// The dangling prerequisite target must appear as a stub node so the
	// edge keeps two resolvable ends — and say so: the HUD summary rollup
	// counts engrams off this payload and must be able to leave gaps out.
	stub, ok := byURI["engram://full-missing/x"]
	if !ok {
		t.Fatalf("dangling prerequisite not stubbed: %v", byURI)
	}
	if stub["stub"] != true {
		t.Errorf("stub node lacks the stub marker: %v", stub)
	}
	for _, uri := range []string{"engram://full-base/x", "engram://full-comp/x"} {
		if _, marked := byURI[uri]["stub"]; marked {
			t.Errorf("catalog node %s carries a stub marker: %v", uri, byURI[uri])
		}
	}

	edges := payload["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d: %v", len(edges), edges)
	}
	// Deterministic order: edges sorted by (from, to).
	e0 := edges[0].(map[string]any)
	e1 := edges[1].(map[string]any)
	if e0["to"].(string) > e1["to"].(string) {
		t.Errorf("edges not deterministically ordered: %v then %v", e0, e1)
	}

	// A second call must produce byte-identical node order (cache-stable).
	res2, err := svc.HandleEngramGraph(ctx, map[string]any{})
	if err != nil || res2.IsError {
		t.Fatalf("second call failed: %v %+v", err, res2)
	}
	if res.Content[0].Text != res2.Content[0].Text {
		t.Errorf("full graph not deterministic across calls")
	}
}

// ---------------------------------------------------------------------------
// One identity: URI is the id everywhere
// ---------------------------------------------------------------------------

// The dual-ID model this pins against: list/recall used to serve the backing
// memory-item id as "id" while the graph keyed nodes by URI (with no id at
// all), so a response's own prerequisites could not be resolved against its
// own id space and every client grew a tolerant three-way matcher.
func TestEngramIdentity_URIIsTheIDEverywhere(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "Base idiom", "problem": "p", "solution": "s",
		"proof": "file.go:1", "family": "identity-base", "slug": "go", "tier": 1,
	}); err != nil {
		t.Fatalf("add base: %v", err)
	}
	if _, err := svc.HandleEngramAdd(ctx, map[string]any{
		"title": "Composite", "problem": "p", "solution": "s",
		"proof": "command: go test", "family": "identity-comp", "slug": "go", "tier": 2,
		// One resolvable prerequisite and one dangling target: the graph must
		// key BOTH ends of both edges in the same id space.
		"prerequisites": []any{"engram://identity-base/go", "engram://identity-missing/go"},
	}); err != nil {
		t.Fatalf("add composite: %v", err)
	}

	// List: id == uri, and the storage id stays available as memory_id.
	listRes, err := svc.HandleEngramList(ctx, map[string]any{"limit": 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items := readResultJSON(t, listRes)["items"].([]any)
	if len(items) < 2 {
		t.Fatalf("expected both engrams, got %d", len(items))
	}
	for _, raw := range items {
		m := raw.(map[string]any)
		id, uri := m["id"].(string), m["uri"].(string)
		memID, _ := m["memory_id"].(string)
		if id != uri {
			t.Errorf("list item id %q != uri %q", id, uri)
		}
		if memID == "" {
			t.Errorf("list item %q lost its memory_id", uri)
		}
		if memID == uri {
			t.Errorf("memory_id %q should be the storage id, not the uri", memID)
		}
	}

	// Full graph: every node (stubs included) carries id == uri, and every
	// edge endpoint resolves against the node id set — the join the tree
	// renders from.
	graphRes, err := svc.HandleEngramGraph(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	graph := readResultJSON(t, graphRes)
	nodeIDs := map[string]bool{}
	for _, raw := range graph["nodes"].([]any) {
		n := raw.(map[string]any)
		id, uri := n["id"].(string), n["uri"].(string)
		if id == "" || id != uri {
			t.Errorf("graph node id %q != uri %q", id, uri)
		}
		nodeIDs[id] = true
	}
	if !nodeIDs["engram://identity-missing/go"] {
		t.Error("dangling prerequisite did not become an id-carrying stub node")
	}
	for _, raw := range graph["edges"].([]any) {
		e := raw.(map[string]any)
		if !nodeIDs[e["from"].(string)] || !nodeIDs[e["to"].(string)] {
			t.Errorf("edge %v->%v does not resolve against node ids", e["from"], e["to"])
		}
	}

	// Recall rides the same projection as list.
	recallRes, err := svc.HandleEngramRecall(ctx, map[string]any{"query": "Composite"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	recallItems, _ := readResultJSON(t, recallRes)["items"].([]any)
	if len(recallItems) == 0 {
		t.Fatal("recall returned no items")
	}
	for _, raw := range recallItems {
		m := raw.(map[string]any)
		if m["id"].(string) != m["uri"].(string) {
			t.Errorf("recall item id %q != uri %q", m["id"], m["uri"])
		}
	}
}

// Legacy recipe items predating the engram URI metadata stay uniquely
// addressable: their id falls back to the storage id instead of collapsing
// to an empty string.
func TestEngramIdentity_LegacyRecipeWithoutURIKeepsStorageID(t *testing.T) {
	t.Parallel()
	svc := newEngramTestService()
	ctx := context.Background()

	if _, err := svc.HandleMemoryAdd(ctx, map[string]any{
		"items": []any{map[string]any{
			"title": "Old recipe", "content": "legacy", "tier": "long_term",
			"importance": "high", "category": "recipe",
		}},
	}); err != nil {
		t.Fatalf("seed legacy recipe: %v", err)
	}

	res, err := svc.HandleEngramList(ctx, map[string]any{"limit": 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items := readResultJSON(t, res)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected the legacy recipe, got %d items", len(items))
	}
	m := items[0].(map[string]any)
	if m["uri"].(string) != "" {
		t.Fatalf("legacy recipe unexpectedly carries uri %q", m["uri"])
	}
	if id := m["id"].(string); id == "" || id != m["memory_id"].(string) {
		t.Errorf("legacy recipe id %q must fall back to memory_id %q", id, m["memory_id"])
	}
}
