package main

import (
	"testing"
	"time"
)

// TestApplyEnv_CIWatchDeadlines: the two ci_watch bounds decide when a run stops
// waiting and escalates, so an operator must be able to widen them for a slow
// runner fleet (or narrow them while draining a wedged backlog) without a
// rebuild. Unset leaves zero, which buildGitLabClient passes through so the
// client's own defaults apply.
func TestApplyEnv_CIWatchDeadlines(t *testing.T) {
	t.Setenv("LOOM_MILLS_GITLAB_HEAD_SHA_DEADLINE", "90s")
	t.Setenv("LOOM_MILLS_GITLAB_BRANCH_PIPELINE_DEADLINE", "3m")

	var c Config
	c.ApplyEnv()

	if c.GitLabHeadSHADeadline != 90*time.Second {
		t.Errorf("GitLabHeadSHADeadline = %s, want 1m30s", c.GitLabHeadSHADeadline)
	}
	if c.GitLabBranchPipelineDeadline != 3*time.Minute {
		t.Errorf("GitLabBranchPipelineDeadline = %s, want 3m", c.GitLabBranchPipelineDeadline)
	}
}

// TestApplyEnv_CIWatchDeadlinesRejectNonPositive: durationEnv honours a negative
// duration as "no bound" for council stage budgets, but a negative deadline here
// would fail every poll on its first observation. Unparseable and non-positive
// values must leave the default in place rather than wedge every run.
func TestApplyEnv_CIWatchDeadlinesRejectNonPositive(t *testing.T) {
	for _, raw := range []string{"-5m", "0s", "banana"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("LOOM_MILLS_GITLAB_HEAD_SHA_DEADLINE", raw)
			t.Setenv("LOOM_MILLS_GITLAB_BRANCH_PIPELINE_DEADLINE", raw)

			var c Config
			c.ApplyEnv()

			if c.GitLabHeadSHADeadline != 0 || c.GitLabBranchPipelineDeadline != 0 {
				t.Errorf("deadlines = %s / %s, want both left at zero (client default)",
					c.GitLabHeadSHADeadline, c.GitLabBranchPipelineDeadline)
			}
		})
	}
}

// TestBuildGitLabClient_AppliesDeadlineOverrides closes the loop: the config
// values must actually reach the client, and an override wider than the poll
// deadline must still clamp.
func TestBuildGitLabClient_AppliesDeadlineOverrides(t *testing.T) {
	cli := buildGitLabClient(Config{
		GitLabAPIURL:                 "https://gitlab.example/api/v4",
		GitLabToken:                  "tok",
		GitLabProject:                "services/loom-core",
		GitLabHeadSHADeadline:        2 * time.Minute,
		GitLabBranchPipelineDeadline: time.Hour, // wider than the 30m poll deadline
	}, discardLogger())
	if cli == nil {
		t.Fatal("client not built")
	}
	if got := cli.HeadSHADeadline(); got != 2*time.Minute {
		t.Errorf("HeadSHADeadline = %s, want 2m", got)
	}
	if got := cli.BranchPipelineDeadline(); got != 30*time.Minute {
		t.Errorf("BranchPipelineDeadline = %s, want it clamped to the 30m poll deadline", got)
	}

	// No overrides: the client's own defaults survive.
	def := buildGitLabClient(Config{
		GitLabAPIURL:  "https://gitlab.example/api/v4",
		GitLabToken:   "tok",
		GitLabProject: "services/loom-core",
	}, discardLogger())
	if def == nil {
		t.Fatal("client not built")
	}
	if def.HeadSHADeadline() == 0 || def.BranchPipelineDeadline() == 0 {
		t.Errorf("unset overrides must leave the client defaults, got %s / %s",
			def.HeadSHADeadline(), def.BranchPipelineDeadline())
	}
}
