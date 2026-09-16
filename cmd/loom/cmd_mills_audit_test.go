package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/audit"
)

func TestMillsAuditSweepDryRunAndConfirm(t *testing.T) {
	t.Setenv("GITLAB_PROJECT", "services/loom-core")
	t.Setenv("GITLAB_TOKEN", "test-token")
	now := time.Now().UTC()
	issue := audit.DigestIssue{
		IID: 71, State: "opened", Author: audit.AuditAdvisoryDigestAuthor,
		Labels: []string{audit.AuditAdvisoryDigestLabel}, CreatedAt: now.Add(-31 * 24 * time.Hour),
		Title:       audit.AuditAdvisoryDigestTitlePrefix + "2026-07-01" + audit.AuditAdvisoryDigestTitleSuffix,
		Description: audit.AuditAdvisoryDigestMarkerPrefix + "2026-07-01" + audit.AuditAdvisoryDigestMarkerSuffix,
	}
	original := newAuditAdvisoryClient
	t.Cleanup(func() { newAuditAdvisoryClient = original })

	dryClient := &commandSweepClient{issues: []audit.DigestIssue{issue}}
	newAuditAdvisoryClient = func(_, _, _, _ string) (audit.DigestIssueClient, error) { return dryClient, nil }
	dry := newMillsAuditSweepCmd()
	var dryOut bytes.Buffer
	dry.SetOut(&dryOut)
	if err := dry.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(dryClient.closed) != 0 || !strings.Contains(dryOut.String(), `"mode":"dry-run"`) || strings.Contains(dryOut.String(), `"audit_entry"`) {
		t.Fatalf("dry-run output=%q closed=%v", dryOut.String(), dryClient.closed)
	}

	confirmedClient := &commandSweepClient{issues: []audit.DigestIssue{issue}}
	newAuditAdvisoryClient = func(_, _, _, _ string) (audit.DigestIssueClient, error) { return confirmedClient, nil }
	confirmed := newMillsAuditSweepCmd()
	confirmed.SetArgs([]string{"--confirm"})
	var confirmedOut bytes.Buffer
	confirmed.SetOut(&confirmedOut)
	if err := confirmed.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(confirmedClient.closed) != 1 || !strings.Contains(confirmedOut.String(), `"audit_entry"`) {
		t.Fatalf("confirmed output=%q closed=%v", confirmedOut.String(), confirmedClient.closed)
	}
}

func TestMillsAuditSweepRejectsUnsafeAgeBeforeClientCreation(t *testing.T) {
	called := false
	original := newAuditAdvisoryClient
	newAuditAdvisoryClient = func(_, _, _, _ string) (audit.DigestIssueClient, error) {
		called = true
		return &commandSweepClient{}, nil
	}
	t.Cleanup(func() { newAuditAdvisoryClient = original })
	cmd := newMillsAuditSweepCmd()
	cmd.SetArgs([]string{"--older-than", "6d"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "at least 7d") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("client created before unsafe age was rejected")
	}
}

func TestMillsAuditSweepRejectsPositionalArguments(t *testing.T) {
	cmd := newMillsAuditSweepCmd()
	cmd.SetArgs([]string{"unexpected"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v, want positional argument rejection", err)
	}
}
