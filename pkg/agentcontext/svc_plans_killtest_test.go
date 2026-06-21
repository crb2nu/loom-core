package agentcontext

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// TestPlan_KillTest_CrossProcessQdrant is the Slice-1 riskiest-assumption
// kill-test, run against the REAL shared Qdrant. It proves the load-bearing
// claim: a Plan created by one process/agent is retrievable byte-identical by a
// SEPARATE process (fresh in-memory cache) using only the plan_id — no agent_id,
// no shared memory. A different worktree, a Codex session, and a Mills pod are
// all just "another process pointed at the same Qdrant", which is exactly what
// the two independent PlanSvc instances below model.
//
// Gated behind RUN_PLAN_STORE_IT=1 so it never runs in normal unit CI. It uses
// a throwaway collection so it cannot pollute agent_plans_v1.
//
//	RUN_PLAN_STORE_IT=1 QDRANT_URL=... QDRANT_API_KEY=... \
//	  go test ./pkg/agentcontext/ -run TestPlan_KillTest -count=1 -v
func TestPlan_KillTest_CrossProcessQdrant(t *testing.T) {
	if os.Getenv("RUN_PLAN_STORE_IT") != "1" {
		t.Skip("set RUN_PLAN_STORE_IT=1 to run the live Qdrant kill-test")
	}
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("QDRANT_URL not set")
	}
	apiKey := os.Getenv("QDRANT_API_KEY")
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")

	const coll = "agent_plans_killtest"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// Producer process: agent A, its own QdrantClient + cache.
	producerQ := NewQdrantClient(httpclient.NewDefault(), url, apiKey, coll, "Cosine")
	producerQ.SetKind(CollPlans)
	producer := NewPlanSvc(producerQ, logger)

	res, err := producer.Create(ctx, map[string]any{
		"title":     "Kill-test plan " + time.Now().UTC().Format(time.RFC3339Nano),
		"project":   "services/loom-core",
		"namespace": "loom-core/killtest",
		"agent_id":  "producer-agent-A",
		"spec_doc":  "# canonical body\nstore is the source of truth",
		"slices": []any{
			map[string]any{"name": "entity", "files": []any{"pkg/agentcontext/schema_plan.go"}},
		},
	})
	created := okJSON(t, res, err)
	planID, _ := created["plan_id"].(string)
	if planID == "" {
		t.Fatalf("no plan_id: %v", created)
	}
	t.Logf("producer created plan_id=%s in collection=%s", planID, coll)

	// Best-effort cleanup of the throwaway point.
	t.Cleanup(func() {
		_ = producerQ.Delete(context.Background(), []string{planID})
	})

	// Consumer process: a DIFFERENT QdrantClient + a FRESH PlanSvc with an empty
	// in-memory cache — models a fresh subagent in another worktree / Codex /
	// Mills pod. Retrieve with NO agent_id.
	consumerQ := NewQdrantClient(httpclient.NewDefault(), url, apiKey, coll, "Cosine")
	consumerQ.SetKind(CollPlans)
	consumer := NewPlanSvc(consumerQ, logger)

	res, err = consumer.Get(ctx, map[string]any{"plan_id": planID})
	got := okJSON(t, res, err)
	plan, ok := got["plan"].(map[string]any)
	if !ok {
		t.Fatalf("consumer got no plan (cross-process retrieval FAILED): %v", got)
	}
	if plan["id"] != planID {
		t.Fatalf("consumer got wrong plan: %v", plan["id"])
	}
	if plan["created_by"] != "producer-agent-A" {
		t.Fatalf("attribution lost cross-process: %v", plan["created_by"])
	}
	if plan["spec_doc"] != "# canonical body\nstore is the source of truth" {
		t.Fatalf("spec_doc not byte-identical cross-process: %q", plan["spec_doc"])
	}
	slices, _ := plan["slices"].([]any)
	if len(slices) != 1 {
		t.Fatalf("slices lost cross-process: %v", plan["slices"])
	}

	// Cross-process LIST by project (not agent) must also surface it.
	res, err = consumer.List(ctx, map[string]any{"project": "services/loom-core", "namespace": "loom-core/killtest"})
	listed := okJSON(t, res, err)
	if c, _ := listed["count"].(float64); c < 1 {
		t.Fatalf("project-scoped list did not find the plan: %v", listed)
	}

	t.Logf("KILL-TEST PASS: plan %s retrieved cross-process, non-agent-scoped, byte-identical", planID)
}
