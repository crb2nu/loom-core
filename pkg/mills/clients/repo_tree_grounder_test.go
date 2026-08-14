package clients

import (
	"context"
	"errors"
	"testing"
	"time"
)

// groundedRunner returns per-subcommand canned results, keyed on the first
// git arg (fetch / rev-parse / ls-tree), and counts calls per key.
type groundedRunner struct {
	results map[string]struct {
		stdout string
		stderr string
		exit   int
		err    error
	}
	calls map[string]int
}

func newGroundedRunner(tree string, revision string) *groundedRunner {
	r := &groundedRunner{
		results: map[string]struct {
			stdout string
			stderr string
			exit   int
			err    error
		}{},
		calls: map[string]int{},
	}
	r.set("fetch", "", "", 0, nil)
	r.set("rev-parse", revision+"\n", "", 0, nil)
	r.set("ls-tree", tree, "", 0, nil)
	return r
}

func (r *groundedRunner) set(sub, stdout, stderr string, exit int, err error) {
	r.results[sub] = struct {
		stdout string
		stderr string
		exit   int
		err    error
	}{stdout, stderr, exit, err}
}

func (r *groundedRunner) Run(_ context.Context, _ string, _ string, args ...string) (string, string, int, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	r.calls[sub]++
	res := r.results[sub]
	return res.stdout, res.stderr, res.exit, res.err
}

func testGrounder(runner CommandRunner) *RepoTreeGrounder {
	return &RepoTreeGrounder{
		RepoRoot: "/workspace/loom-core",
		Project:  "services/loom-core",
		Runner:   runner,
	}
}

func TestRepoTreeGrounder_ReportsMissingAtPinnedRevision(t *testing.T) {
	runner := newGroundedRunner("pkg/mills/policy.go\ncmd/op/main.go\n", "abc1234")
	g := testGrounder(runner)
	missing, rev, ok := g.Ground(context.Background(), "services/loom-core",
		[]string{"pkg/mills/policy.go", "pkg/mills/invented.go", "cmd/op/main.go"})
	if !ok {
		t.Fatal("ok=false, want grounded")
	}
	if rev != "abc1234" {
		t.Errorf("revision=%q, want abc1234", rev)
	}
	if len(missing) != 1 || missing[0] != "pkg/mills/invented.go" {
		t.Errorf("missing=%v, want [pkg/mills/invented.go]", missing)
	}
}

func TestRepoTreeGrounder_FetchFailureStillGrounds(t *testing.T) {
	// The fetch is best-effort: a LAN blip must not turn every slice
	// ungroundable when the previously fetched origin/main ref still answers.
	runner := newGroundedRunner("pkg/real.go\n", "def5678")
	runner.set("fetch", "", "network unreachable", 128, errors.New("exit 128"))
	missing, rev, ok := testGrounder(runner).Ground(context.Background(), "services/loom-core",
		[]string{"pkg/real.go", "pkg/fake.go"})
	if !ok || rev != "def5678" {
		t.Fatalf("ok=%v rev=%q, want grounded at def5678", ok, rev)
	}
	if len(missing) != 1 || missing[0] != "pkg/fake.go" {
		t.Errorf("missing=%v, want [pkg/fake.go]", missing)
	}
}

func TestRepoTreeGrounder_RevParseFailureFailsOpen(t *testing.T) {
	runner := newGroundedRunner("pkg/real.go\n", "")
	runner.set("rev-parse", "", "unknown revision", 128, errors.New("exit 128"))
	if _, _, ok := testGrounder(runner).Ground(context.Background(), "services/loom-core", []string{"pkg/real.go"}); ok {
		t.Fatal("ok=true with no resolvable ref; must fail open (ungrounded)")
	}
}

func TestRepoTreeGrounder_EmptyTreeFailsOpen(t *testing.T) {
	// An empty listing on a populated repo means the read is broken, not that
	// every declared path is fabricated.
	runner := newGroundedRunner("", "abc1234")
	if _, _, ok := testGrounder(runner).Ground(context.Background(), "services/loom-core", []string{"pkg/real.go"}); ok {
		t.Fatal("ok=true on an empty tree listing; must fail open")
	}
}

func TestRepoTreeGrounder_RefusesForeignProject(t *testing.T) {
	runner := newGroundedRunner("pkg/real.go\n", "abc1234")
	if _, _, ok := testGrounder(runner).Ground(context.Background(), "platform/gitops", []string{"clusters/k3s/app.yaml"}); ok {
		t.Fatal("ok=true for a foreign project; the clone cannot answer for a repo it does not hold")
	}
	if len(runner.calls) != 0 {
		t.Errorf("git invoked %v for a foreign project; must not run at all", runner.calls)
	}
}

func TestRepoTreeGrounder_SkipsGlobsAndNormalizes(t *testing.T) {
	runner := newGroundedRunner("pkg/real.go\n", "abc1234")
	missing, _, ok := testGrounder(runner).Ground(context.Background(), "services/loom-core",
		[]string{"pkg/**/*auth*.go", "./pkg/real.go", "  ", "pkg/gone.go"})
	if !ok {
		t.Fatal("ok=false, want grounded")
	}
	if len(missing) != 1 || missing[0] != "pkg/gone.go" {
		t.Errorf("missing=%v, want [pkg/gone.go] (glob + blank skipped, ./ normalized)", missing)
	}
}

func TestRepoTreeGrounder_CachesTreeWithinTTL(t *testing.T) {
	runner := newGroundedRunner("pkg/real.go\n", "abc1234")
	g := testGrounder(runner)
	g.TTL = time.Hour
	for i := 0; i < 3; i++ {
		if _, _, ok := g.Ground(context.Background(), "services/loom-core", []string{"pkg/real.go"}); !ok {
			t.Fatalf("call %d: ok=false", i)
		}
	}
	if runner.calls["ls-tree"] != 1 || runner.calls["fetch"] != 1 {
		t.Errorf("calls=%v, want one fetch + one ls-tree across three grounds (TTL cache)", runner.calls)
	}
}
