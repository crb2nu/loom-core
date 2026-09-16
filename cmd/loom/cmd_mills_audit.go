package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/mills/audit"
)

func newMillsAuditCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Manage Mills audit advisories"}
	cmd.AddCommand(newMillsAuditSweepCmd())
	return cmd
}

func newMillsAuditSweepCmd() *cobra.Command {
	var olderThanText string
	var confirm bool
	var apiURL, token, project, author string
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Find and optionally close stale audit-advisory items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			olderThan, err := parseMillsAuditAge(olderThanText)
			if err != nil {
				return fmt.Errorf("invalid --older-than: %w", err)
			}
			if olderThan < 7*24*time.Hour {
				return fmt.Errorf("--older-than must be at least 7d")
			}
			if strings.TrimSpace(project) == "" {
				return fmt.Errorf("GitLab project required (set --project or GITLAB_PROJECT)")
			}
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("GitLab token required (set --token, GITLAB_PERSONAL_ACCESS_TOKEN, or GITLAB_TOKEN)")
			}
			client, err := newAuditAdvisoryClient(apiURL, token, project, author)
			if err != nil {
				return err
			}
			var auditEntry *audit.SweepEvent
			emitter := audit.SweepEmitterFunc(func(_ context.Context, event audit.SweepEvent) { auditEntry = &event })
			result, sweepErr := audit.SweepAuditAdvisories(cmd.Context(), client, audit.AdvisorySweepOptions{Now: time.Now(), StaleAfter: olderThan, Author: author, Confirm: confirm, Emitter: emitter})
			out := struct {
				Mode       string              `json:"mode"`
				Cutoff     time.Time           `json:"cutoff"`
				Selected   []audit.DigestIssue `json:"selected"`
				Closed     []int64             `json:"closed"`
				AuditEntry *audit.SweepEvent   `json:"audit_entry,omitempty"`
			}{Mode: "dry-run", Cutoff: result.Cutoff, Selected: result.Selected, Closed: result.Closed, AuditEntry: auditEntry}
			if confirm {
				out.Mode = "confirmed"
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(out); err != nil {
				return err
			}
			return sweepErr
		},
	}
	cmd.Flags().StringVar(&olderThanText, "older-than", "30d", "minimum advisory age (minimum 7d)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "close the selected advisories")
	defaultAPIURL := strings.TrimSpace(os.Getenv("GITLAB_API_URL"))
	if defaultAPIURL == "" {
		defaultAPIURL = "https://gitlab.com/api/v4"
	}
	defaultToken := strings.TrimSpace(os.Getenv("GITLAB_PERSONAL_ACCESS_TOKEN"))
	if defaultToken == "" {
		defaultToken = strings.TrimSpace(os.Getenv("GITLAB_TOKEN"))
	}
	cmd.Flags().StringVar(&apiURL, "api-url", defaultAPIURL, "GitLab API URL")
	cmd.Flags().StringVar(&token, "token", defaultToken, "GitLab token")
	cmd.Flags().StringVar(&project, "project", os.Getenv("GITLAB_PROJECT"), "GitLab project path or ID")
	cmd.Flags().StringVar(&author, "author", audit.AuditAdvisoryDigestAuthor, "exact digest bot username")
	return cmd
}

func parseMillsAuditAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(value, "d") + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(value)
}
