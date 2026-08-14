package gates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalConfigCheck is one named configuration invariant that must hold before
// Mills starts autonomous work. A returned error is the human-readable reason
// the configuration is unsafe.
type LocalConfigCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// LocalConfigChecker is the production LocalConfigPreflight. Every check runs
// on every evaluation — reporting all configuration problems at once is worth
// more to an operator than short-circuiting on the first.
type LocalConfigChecker struct {
	Checks []LocalConfigCheck
}

// PreflightLocalConfig satisfies LocalConfigPreflight. A failing check is a
// deterministic unsafe result rather than an error: the configuration is
// knowably wrong, which the gate should classify as a config block instead of
// an evaluator malfunction. A checker with no checks is itself a
// misconfiguration and reports unsafe.
func (c LocalConfigChecker) PreflightLocalConfig(ctx context.Context) (LocalConfigResult, error) {
	if len(c.Checks) == 0 {
		return LocalConfigResult{
			Safe:    false,
			Reasons: []string{"local config preflight has no checks configured"},
		}, nil
	}

	result := LocalConfigResult{Safe: true}
	for _, check := range c.Checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			name = "unnamed check"
		}
		if check.Check == nil {
			result.Safe = false
			result.Reasons = append(result.Reasons, fmt.Sprintf("%s: check is not configured", name))
			continue
		}
		if err := check.Check(ctx); err != nil {
			result.Safe = false
			result.Reasons = append(result.Reasons, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return result, nil
}

// WritableDirCheck verifies a directory exists and accepts a write. This is
// the check that catches the class of wedge where a spawn stage produces zero
// output because its working root is present but not writable by the
// container's uid.
func WritableDirCheck(name, path string) LocalConfigCheck {
	return LocalConfigCheck{
		Name: name,
		Check: func(context.Context) error {
			path = strings.TrimSpace(path)
			if path == "" {
				return fmt.Errorf("path is not configured")
			}
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("cannot stat %s: %w", path, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", path)
			}
			if err := probeWritable(path); err != nil {
				return fmt.Errorf("%s is not writable: %w", path, err)
			}
			return nil
		},
	}
}

// RepoRootCheck verifies the configured repo root is a real git working tree.
// A repo root that is missing its .git entry means every implement stage would
// produce an empty diff, which the pipeline would otherwise discover only
// after paying for a spawn.
func RepoRootCheck(name, repoRoot string) LocalConfigCheck {
	return LocalConfigCheck{
		Name: name,
		Check: func(context.Context) error {
			repoRoot = strings.TrimSpace(repoRoot)
			if repoRoot == "" {
				return fmt.Errorf("repo root is not configured")
			}
			info, err := os.Stat(repoRoot)
			if err != nil {
				return fmt.Errorf("cannot stat %s: %w", repoRoot, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", repoRoot)
			}
			// .git is a directory in a clone and a file in a linked worktree;
			// both are valid roots for Mills work.
			if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
				return fmt.Errorf("%s is not a git working tree: %w", repoRoot, err)
			}
			return nil
		},
	}
}

// RequiredValueCheck verifies a configuration value is set. The remediation is
// folded into the error so the blocked run's escalation names the knob to fix.
func RequiredValueCheck(name, value, remediation string) LocalConfigCheck {
	return LocalConfigCheck{
		Name: name,
		Check: func(context.Context) error {
			if strings.TrimSpace(value) != "" {
				return nil
			}
			if remediation = strings.TrimSpace(remediation); remediation != "" {
				return fmt.Errorf("value is not configured (%s)", remediation)
			}
			return fmt.Errorf("value is not configured")
		},
	}
}

// LocalConfigFunc adapts a plain function to LocalConfigPreflight.
type LocalConfigFunc func(ctx context.Context) (LocalConfigResult, error)

// PreflightLocalConfig satisfies LocalConfigPreflight.
func (f LocalConfigFunc) PreflightLocalConfig(ctx context.Context) (LocalConfigResult, error) {
	return f(ctx)
}
