package gates

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalConfigChecker_SafeWhenEveryCheckPasses(t *testing.T) {
	result, err := LocalConfigChecker{Checks: []LocalConfigCheck{
		{Name: "a", Check: func(context.Context) error { return nil }},
		{Name: "b", Check: func(context.Context) error { return nil }},
	}}.PreflightLocalConfig(context.Background())

	if err != nil {
		t.Fatalf("PreflightLocalConfig() error = %v", err)
	}
	if !result.Safe || len(result.Reasons) != 0 {
		t.Fatalf("result = %+v, want safe with no reasons", result)
	}
}

// Every check runs even after one fails: an operator fixing configuration
// wants the whole list, not a one-at-a-time reveal.
func TestLocalConfigChecker_ReportsEveryFailure(t *testing.T) {
	result, err := LocalConfigChecker{Checks: []LocalConfigCheck{
		{Name: "first", Check: func(context.Context) error { return errors.New("boom") }},
		{Name: "second", Check: func(context.Context) error { return errors.New("bang") }},
	}}.PreflightLocalConfig(context.Background())

	if err != nil {
		t.Fatalf("PreflightLocalConfig() error = %v", err)
	}
	if result.Safe {
		t.Fatal("result is safe despite failing checks")
	}
	if len(result.Reasons) != 2 {
		t.Fatalf("reasons = %v, want 2", result.Reasons)
	}
	if !strings.Contains(result.Reasons[0], "first") || !strings.Contains(result.Reasons[0], "boom") {
		t.Errorf("reason %q does not name the check and its error", result.Reasons[0])
	}
}

// A checker with no checks is a wiring mistake, not a clean bill of health.
func TestLocalConfigChecker_EmptyIsUnsafe(t *testing.T) {
	result, err := LocalConfigChecker{}.PreflightLocalConfig(context.Background())
	if err != nil {
		t.Fatalf("PreflightLocalConfig() error = %v", err)
	}
	if result.Safe {
		t.Fatal("empty checker reported safe")
	}
}

func TestLocalConfigChecker_NilCheckFuncIsUnsafe(t *testing.T) {
	result, _ := LocalConfigChecker{Checks: []LocalConfigCheck{{Name: "unset"}}}.PreflightLocalConfig(context.Background())
	if result.Safe {
		t.Fatal("checker with a nil check func reported safe")
	}
}

func TestWritableDirCheck(t *testing.T) {
	dir := t.TempDir()
	if err := WritableDirCheck("store", dir).Check(context.Background()); err != nil {
		t.Fatalf("writable dir reported an error: %v", err)
	}
	if err := WritableDirCheck("store", filepath.Join(dir, "missing")).Check(context.Background()); err == nil {
		t.Fatal("missing dir passed the writable check")
	}
	if err := WritableDirCheck("store", "").Check(context.Background()); err == nil {
		t.Fatal("empty path passed the writable check")
	}

	file := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := WritableDirCheck("store", file).Check(context.Background()); err == nil {
		t.Fatal("a regular file passed the writable directory check")
	}
}

func TestRepoRootCheck(t *testing.T) {
	dir := t.TempDir()
	if err := RepoRootCheck("repo", dir).Check(context.Background()); err == nil {
		t.Fatal("a directory with no .git passed the repo root check")
	}

	// A clone has a .git directory; a linked worktree has a .git file. Both
	// are valid Mills roots.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("seed .git: %v", err)
	}
	if err := RepoRootCheck("repo", dir).Check(context.Background()); err != nil {
		t.Fatalf("clone-style repo root rejected: %v", err)
	}

	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere"), 0o600); err != nil {
		t.Fatalf("seed .git file: %v", err)
	}
	if err := RepoRootCheck("repo", worktree).Check(context.Background()); err != nil {
		t.Fatalf("worktree-style repo root rejected: %v", err)
	}

	if err := RepoRootCheck("repo", "").Check(context.Background()); err == nil {
		t.Fatal("empty repo root passed the check")
	}
}

func TestRequiredValueCheck(t *testing.T) {
	if err := RequiredValueCheck("token", "set", "").Check(context.Background()); err != nil {
		t.Fatalf("set value rejected: %v", err)
	}
	err := RequiredValueCheck("token", "  ", "set LOOM_MILLS_ADMIN_TOKEN").Check(context.Background())
	if err == nil {
		t.Fatal("blank value passed the required check")
	}
	if !strings.Contains(err.Error(), "LOOM_MILLS_ADMIN_TOKEN") {
		t.Errorf("error %q omits the remediation", err)
	}
}

func TestLocalConfigFunc_SatisfiesInterface(t *testing.T) {
	var p LocalConfigPreflight = LocalConfigFunc(func(context.Context) (LocalConfigResult, error) {
		return LocalConfigResult{Safe: true}, nil
	})
	result, err := p.PreflightLocalConfig(context.Background())
	if err != nil || !result.Safe {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}
