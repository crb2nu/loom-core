package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// clearIntents empties the canonical roadmap-intent store that newRunnerEnv
// seeds, reproducing the production condition this guardrail exists for.
func clearIntents(t *testing.T, env *runnerEnv) {
	t.Helper()
	if _, err := env.store.Roadmap.DeleteStale(context.Background(), "no-such-sha"); err != nil {
		t.Fatalf("clear intents: %v", err)
	}
	intents, err := env.store.Roadmap.List(context.Background())
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) != 0 {
		t.Fatalf("intent store not empty after clear: %d rows", len(intents))
	}
}

// fixtureRoadmap is a minimal tiered ROADMAP.md: two open bullets under a
// section parseSectionHeader promotes, plus a checked bullet the extractor
// must drop.
const fixtureRoadmap = `# Roadmap

## Tier 1: Now

- [ ] wire the roadmap extractor into the council run
- [ ] block scheduling when the intent store is empty
- [x] already shipped, must not become an intent
`

// The council must refuse to schedule work when its brief was marked
// intents_missing: planning against no stated intent is worse than skipping
// the tick. The brief is still returned so the operator can see WHY.
func TestExecute_BlocksSchedulingWhenIntentStoreEmpty(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()
	clearIntents(t, env)

	res, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerCron})
	if !errors.Is(err, ErrIntentsMissing) {
		t.Fatalf("err = %v, want ErrIntentsMissing", err)
	}
	// The error must name the override key or an operator cannot recover.
	if !strings.Contains(err.Error(), "require_roadmap_intents") {
		t.Errorf("error %q does not name the override key", err)
	}

	if res == nil || res.Brief == nil {
		t.Fatal("blocked run must still return the compiled brief")
	}
	if !res.Brief.IntentsMissing {
		t.Error("returned brief was not marked IntentsMissing")
	}
	if !strings.Contains(res.Brief.Markdown, council.IntentsMissingMarker) {
		t.Error("returned brief markdown missing the intents_missing marker")
	}

	// Nothing downstream of the gate may have run.
	if res.Editor != nil || res.Write != nil || res.Verdict != nil || res.Mutation != nil {
		t.Errorf("work happened past the gate: editor=%v write=%v verdict=%v mutation=%v",
			res.Editor != nil, res.Write != nil, res.Verdict != nil, res.Mutation != nil)
	}
	if len(res.Reviews) != 0 {
		t.Errorf("reviewers ran past the gate: %d reviews", len(res.Reviews))
	}
	if res.CostUSDApprox != 0 {
		t.Errorf("blocked run cost %.4f USD, want 0", res.CostUSDApprox)
	}

	// The durable row must be terminal, with a note that explains the block.
	run, getErr := env.store.Council.Get(ctx, res.RunID)
	if getErr != nil {
		t.Fatalf("get run: %v", getErr)
	}
	if run.Outcome != store.CouncilOutcomeError {
		t.Errorf("outcome = %q, want %q", run.Outcome, store.CouncilOutcomeError)
	}
	if !strings.Contains(run.Notes, "require_roadmap_intents") {
		t.Errorf("run notes = %q, want the override key named", run.Notes)
	}
	if run.EndedAt == nil {
		t.Error("blocked run was not terminalized (EndedAt nil)")
	}
}

// The guardrail applies uniformly to dryruns. Exempting them would make
// POST /api/mills/council/dryrun a trivial bypass and would diverge the
// operator's audit surface from the scheduled path.
func TestExecute_BlocksDryrunToo(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	clearIntents(t, env)

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual, Dryrun: true,
	})
	if !errors.Is(err, ErrIntentsMissing) {
		t.Fatalf("dryrun err = %v, want ErrIntentsMissing", err)
	}
	if res == nil || res.Brief == nil || !res.Brief.IntentsMissing {
		t.Fatal("blocked dryrun must return a brief marked IntentsMissing")
	}
}

// overridePolicyYAML is validPolicyYAML with the break-glass opt-out set.
const overridePolicyYAML = `
version: 1
budgets:
  council:  { max_usd_per_run: 5, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 50 }
council:
  schedule_cron: "0 5 * * *"
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
  require_roadmap_intents: false
  ensemble:
    editor: { model: claude-opus, backend: claude-code }
    reviewers:
      - { name: security,  model: gpt-5-codex, backend: codex }
      - { name: tech-debt, model: qwen3.5-9b,  backend: flexinfer }
pipeline:
  default_template: mills-default-pipeline
  retry: { max_attempts: 3, cooldown_seconds: 60 }
human_handoff:
  on_escalation_create_handoff: true
  on_escalation_create_issue: true
`

// Break-glass: an operator who knowingly wants planning to continue against an
// empty intent store can opt out, and the run proceeds end to end.
func TestExecute_PolicyOverrideAllowsEmptyIntentStore(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()
	clearIntents(t, env)

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(overridePolicyYAML), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := mills.NewPolicyManager(ctx, policyPath, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	if pm.Current().CouncilRequireRoadmapIntents() {
		t.Fatal("override policy still requires roadmap intents")
	}
	env.runner.Policy = pm
	env.runner.Budget = mills.NewBudget(pm, mills.NewStoreBudgetReader(env.store))

	res, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("override run: %v", err)
	}
	if res.Brief == nil || !res.Brief.IntentsMissing {
		t.Error("override must still MARK the brief, only skip the block")
	}
	if res.Verdict == nil || res.Write == nil {
		t.Errorf("override run did not complete: verdict=%v write=%v",
			res.Verdict != nil, res.Write != nil)
	}
}

// The root cause of the empty store: ExtractFromFile + SyncToStore had no
// caller. The runner must fill the canonical store from ROADMAP.md BEFORE the
// brief is compiled, so a healthy repo never trips the gate at all.
func TestExecute_RoadmapExtractionFillsStoreBeforeBrief(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()
	clearIntents(t, env)

	path := filepath.Join(env.repoRoot, "ROADMAP.md")
	if err := os.WriteFile(path, []byte(fixtureRoadmap), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}

	res, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	intents, err := env.store.Roadmap.List(ctx)
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) != 2 {
		t.Fatalf("intents = %d, want 2 open bullets (checked bullet must be dropped)", len(intents))
	}

	wantSHA, err := roadmapBlobSHA(path)
	if err != nil {
		t.Fatalf("blob sha: %v", err)
	}
	for _, in := range intents {
		if in.LastSeenInRoadmapSHA != wantSHA {
			t.Errorf("intent %q sha = %q, want file blob sha %q", in.Summary, in.LastSeenInRoadmapSHA, wantSHA)
		}
	}

	// Extraction ran BEFORE Compile, so the brief must not be marked.
	if res.Brief.IntentsMissing {
		t.Error("brief marked IntentsMissing despite a readable ROADMAP.md")
	}
	if strings.Contains(res.Brief.Markdown, council.IntentsMissingMarker) {
		t.Error("brief stamped the missing marker despite a readable ROADMAP.md")
	}
	if !strings.Contains(res.Brief.Markdown, "wire the roadmap extractor into the council run") {
		t.Error("extracted intent did not reach the brief markdown")
	}
}

// A missing ROADMAP.md is tolerated, never fatal: with a populated store the
// run completes, and with an empty store it fails via the intents gate rather
// than an os.ErrNotExist leaking out of the extractor.
func TestExecute_MissingRoadmapFileIsToleratedNotFatal(t *testing.T) {
	t.Run("populated store completes", func(t *testing.T) {
		env := newRunnerEnv(t, sampleProposals(1))
		if _, err := os.Stat(filepath.Join(env.repoRoot, "ROADMAP.md")); !os.IsNotExist(err) {
			t.Fatalf("fixture unexpectedly has a ROADMAP.md: %v", err)
		}
		res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.Verdict == nil {
			t.Error("run did not reach a verdict")
		}
	})

	t.Run("empty store fails via the gate", func(t *testing.T) {
		env := newRunnerEnv(t, sampleProposals(1))
		clearIntents(t, env)
		_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
		if !errors.Is(err, ErrIntentsMissing) {
			t.Fatalf("err = %v, want ErrIntentsMissing (not a filesystem error)", err)
		}
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing ROADMAP.md leaked as a filesystem error: %v", err)
		}
	})
}

// An unreadable ROADMAP.md (here: a directory, which is portable and needs no
// chmod/root skew) must degrade to a log, not fail the council.
func TestExecute_UnreadableRoadmapIsToleratedWhenStorePopulated(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	if err := os.Mkdir(filepath.Join(env.repoRoot, "ROADMAP.md"), 0o755); err != nil {
		t.Fatalf("mkdir roadmap: %v", err)
	}
	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict == nil {
		t.Error("run did not reach a verdict")
	}
}

// Landmine guard: SyncToStore calls DeleteStale(sha), which deletes every
// intent whose sha differs. A readable ROADMAP.md that parses to ZERO intents
// would therefore wipe the store and self-inflict a permanent fail-closed
// block. The extractor must skip the sync entirely in that case.
func TestExecute_EmptyRoadmapParseDoesNotWipeExistingIntents(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()

	before, err := env.store.Roadmap.List(ctx)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("fixture should seed at least one intent")
	}

	// "Recently Shipped" is dropped by parseSectionHeader, so this file is
	// readable and parseable but yields no intents.
	const noIntents = "# Roadmap\n\n## Recently Shipped\n\n- [ ] not a plannable intent\n"
	if err := os.WriteFile(filepath.Join(env.repoRoot, "ROADMAP.md"), []byte(noIntents), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}

	if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerCron}); err != nil {
		t.Fatalf("run: %v", err)
	}

	after, err := env.store.Roadmap.List(ctx)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("intents = %d after a zero-intent parse, want %d preserved", len(after), len(before))
	}
}

// The sha recorded into last_seen_in_roadmap_sha must be a real git blob id,
// so a future `git hash-object`-based reconciliation agrees with what we wrote.
func TestRoadmapBlobSHAMatchesGitHashObject(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"hello", "hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := roadmapBlobSHA(path)
			if err != nil {
				t.Fatalf("roadmapBlobSHA: %v", err)
			}
			if got != tc.want {
				t.Errorf("sha = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := roadmapBlobSHA(filepath.Join(t.TempDir(), "absent.md")); err == nil {
		t.Error("expected an error for a missing file")
	}
}
