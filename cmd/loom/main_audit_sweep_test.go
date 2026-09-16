package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/audit"
)

type commandSweepClient struct {
	issues []audit.DigestIssue
	closed []int64
}

func (c *commandSweepClient) ListOpenIssues(context.Context) ([]audit.DigestIssue, error) {
	return c.issues, nil
}

func (c *commandSweepClient) CloseIssue(_ context.Context, iid int64) error {
	c.closed = append(c.closed, iid)
	return nil
}

func TestAuditAdvisorySweepCommandDefaultsToDryRun(t *testing.T) {
	t.Setenv("GITLAB_PROJECT", "services/loom-core")
	t.Setenv("GITLAB_TOKEN", "test-token")
	now := time.Now().UTC()
	client := &commandSweepClient{issues: []audit.DigestIssue{{
		IID: 9, State: "opened", Author: audit.AuditAdvisoryDigestAuthor,
		Labels: []string{audit.AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-31 * 24 * time.Hour),
		Title:       audit.AuditAdvisoryDigestTitlePrefix + "2026-06-01" + audit.AuditAdvisoryDigestTitleSuffix,
		Description: audit.AuditAdvisoryDigestMarkerPrefix + "2026-06-01" + audit.AuditAdvisoryDigestMarkerSuffix,
	}}}
	original := newAuditAdvisoryClient
	newAuditAdvisoryClient = func(_, _, _, _ string) (audit.DigestIssueClient, error) { return client, nil }
	t.Cleanup(func() { newAuditAdvisoryClient = original })

	cmd := newAuditAdvisorySweepCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("dry-run closed %v", client.closed)
	}
	if got := out.String(); !strings.Contains(got, "mode=dry-run") || !strings.Contains(got, "selected=1") {
		t.Fatalf("output = %q", got)
	}
}

func TestAuditAdvisorySweepCommandApplyClosesSelected(t *testing.T) {
	t.Setenv("GITLAB_PROJECT", "services/loom-core")
	t.Setenv("GITLAB_TOKEN", "test-token")
	now := time.Now().UTC()
	client := &commandSweepClient{issues: []audit.DigestIssue{{
		IID: 10, State: "opened", Author: audit.AuditAdvisoryDigestAuthor,
		Labels: []string{audit.AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-31 * 24 * time.Hour),
		Title:       audit.AuditAdvisoryDigestTitlePrefix + "2026-06-02" + audit.AuditAdvisoryDigestTitleSuffix,
		Description: audit.AuditAdvisoryDigestMarkerPrefix + "2026-06-02" + audit.AuditAdvisoryDigestMarkerSuffix,
	}}}
	original := newAuditAdvisoryClient
	newAuditAdvisoryClient = func(_, _, _, _ string) (audit.DigestIssueClient, error) { return client, nil }
	t.Cleanup(func() { newAuditAdvisoryClient = original })

	cmd := newAuditAdvisorySweepCmd()
	cmd.SetArgs([]string{"--apply"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 1 || client.closed[0] != 10 {
		t.Fatalf("apply closed %v", client.closed)
	}
	if got := out.String(); !strings.Contains(got, "mode=apply") || !strings.Contains(got, "closed=1") {
		t.Fatalf("output = %q", got)
	}
}

func TestAuditAdvisorySweepCommandRequiresConfiguration(t *testing.T) {
	t.Setenv("GITLAB_PROJECT", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "")
	err := newAuditAdvisorySweepCmd().Execute()
	if err == nil || !strings.Contains(err.Error(), "GitLab project required") {
		t.Fatalf("error = %v", err)
	}
}
