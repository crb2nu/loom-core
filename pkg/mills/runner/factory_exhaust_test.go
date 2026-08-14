package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubExhaust records what the runner asked for so the policy-derived bounds
// can be asserted at the seam that actually reads policy.
type stubExhaust struct {
	calls int
	since time.Time
	limit int
}

func (s *stubExhaust) ListFactoryExhaust(_ context.Context, since time.Time, limit int) ([]council.FactoryExhaustItem, error) {
	s.calls++
	s.since = since
	s.limit = limit
	return []council.FactoryExhaustItem{{
		Kind:      council.FactoryExhaustFlakyTest,
		IID:       612,
		Title:     "flake: TestReconcilerIdleBackoff",
		WebURL:    "https://gitlab.flexinfer.ai/services/loom-core/-/issues/612",
		UpdatedAt: since.Add(time.Hour),
	}}, nil
}

// exhaustPolicyYAML is validPolicyYAML with an explicit factory-exhaust block
// spliced into the council section.
func exhaustPolicyYAML(block string) string {
	return strings.Replace(validPolicyYAML,
		"  schedule_cron: \"0 5 * * *\"\n",
		"  schedule_cron: \"0 5 * * *\"\n"+block, 1)
}

func swapPolicy(t *testing.T, env *runnerEnv, yaml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), path, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	env.runner.Policy = pm
}

// Default policy omits the key, so the source is consulted with the documented
// defaults and its items reach the compiled brief.
func TestExecute_FactoryExhaustDefaultOn(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	src := &stubExhaust{}
	env.runner.FactoryExhaust = src

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if src.calls != 1 {
		t.Fatalf("ListFactoryExhaust calls = %d, want 1 (omitted policy key defaults ON)", src.calls)
	}
	if src.limit != 10 {
		t.Errorf("limit = %d, want the default 10", src.limit)
	}
	if want := env.now.Add(-14 * 24 * time.Hour); !src.since.Equal(want) {
		t.Errorf("since = %s, want the default 14d cutoff %s", src.since, want)
	}
	if res.Brief == nil {
		t.Fatal("run returned no brief")
	}
	if !strings.Contains(res.Brief.Markdown, council.FactoryExhaustHeading) {
		t.Errorf("brief is missing the exhaust section:\n%s", res.Brief.Markdown)
	}
	if res.Brief.SourceCounts.FactoryExhaustItems != 1 {
		t.Errorf("FactoryExhaustItems = %d, want 1", res.Brief.SourceCounts.FactoryExhaustItems)
	}
}

// The policy gate is the seam that turns "disabled" into "nil source": the
// runner must not even ask GitLab, and the brief must carry no section — not an
// empty one, and not an "unavailable" one, which would misreport an opt-out as
// an outage.
func TestExecute_FactoryExhaustPolicyOffOmitsSection(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	src := &stubExhaust{}
	env.runner.FactoryExhaust = src
	swapPolicy(t, env, exhaustPolicyYAML("  sources:\n    factory_exhaust:\n      enabled: false\n"))

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if src.calls != 0 {
		t.Fatalf("ListFactoryExhaust calls = %d, want 0 when policy disables the source", src.calls)
	}
	if strings.Contains(res.Brief.Markdown, council.FactoryExhaustHeading) {
		t.Errorf("policy-off brief still carries the exhaust section:\n%s", res.Brief.Markdown)
	}
	if strings.Contains(res.Brief.Markdown, council.FactoryExhaustUnavailableBody) {
		t.Error("policy-off rendered the unavailable body — an opt-out is not an outage")
	}
	if res.Brief.SourceCounts.FactoryExhaustUnavailable {
		t.Error("policy-off set SourceCounts.FactoryExhaustUnavailable")
	}
}

// Bounds are read per-run from policy, so a hot reload takes effect without a
// restart.
func TestExecute_FactoryExhaustPolicyBounds(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	src := &stubExhaust{}
	env.runner.FactoryExhaust = src
	swapPolicy(t, env, exhaustPolicyYAML("  sources:\n    factory_exhaust:\n      lookback_hours: 48\n      max_items: 3\n"))

	if _, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if src.limit != 3 {
		t.Errorf("limit = %d, want the policy override 3", src.limit)
	}
	if want := env.now.Add(-48 * time.Hour); !src.since.Equal(want) {
		t.Errorf("since = %s, want the policy override cutoff %s", src.since, want)
	}
}

// A nil source (no GitLab client wired) is inert, not fail-closed: the run
// proceeds and the brief simply has no exhaust section.
func TestExecute_FactoryExhaustUnwiredIsInert(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.Brief.Markdown, council.FactoryExhaustHeading) {
		t.Errorf("unwired source produced an exhaust section:\n%s", res.Brief.Markdown)
	}
}
