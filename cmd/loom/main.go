// loom is the CLI for interacting with the Loom daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	ci "github.com/crb2nu/loom/cmd/loom/internal"
	render "github.com/crb2nu/loom/cmd/loom/internal/render"
	"github.com/crb2nu/loom/pkg/mills/audit"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

func init() {
	// Lock the main goroutine to the OS thread it started on (thread 0).
	// macOS requires all AppKit/Cocoa operations — including [NSApp run] —
	// to execute on the process's initial thread. Without this, Go's
	// scheduler may migrate goroutine 1 to a different OS thread before
	// we reach the overlay code path, causing a SIGTRAP crash.
	//
	// This is a no-op performance-wise for non-overlay invocations: it
	// only prevents goroutine 1 from migrating threads, and the main
	// goroutine blocks on cobra command execution regardless.
	runtime.LockOSThread()
}

var version = "0.9.7"

func main() {
	var socketPath string
	defaultSocket := defaultSocketPath()

	rootCmd := &cobra.Command{
		Use:     "loom",
		Short:   "Loom CLI - unified MCP hub management",
		Version: version,
	}

	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "Daemon socket path (env: LOOM_SOCKET)")

	rootCmd.AddCommand(
		// Daemon lifecycle
		newStatusCmd(socketPath),
		newStartCmd(socketPath),
		newStopCmd(socketPath),
		newRestartCmd(socketPath),
		newInstallCmd(),
		newUninstallCmd(),
		newDaemonGroupCmd(socketPath),
		newServersCmd(socketPath),
		newReloadCmd(socketPath),

		// Diagnostics
		newDoctorCmd(),
		newCheckCmd(socketPath),
		newVendorSpecsCmd(),

		// Proxy
		newProxyCmd(socketPath),
		newResponsesCmd(socketPath),

		// Config management
		newGenerateCmd(),
		newCodeAPICmd(socketPath),
		newSyncCmd(),
		newPullCmd(),
		newBackupCmd(),
		newCatalogCmd(),
		newValidateCmd(),
		newProfileCmd(),
		newContextCmd(),
		newSchemasCmd(),
		newRBACCmd(),

		// Tools
		newToolsCmd(socketPath),
		newReplCmd(socketPath),

		// Secrets
		newSecretsCmd(socketPath),

		// Operational
		newTunnelCmd(socketPath),
		newCacheCmd(socketPath),
		newCostCmd(socketPath),
		newHealthCmd(socketPath),
		newPresenceCmd(socketPath),
		newSessionsCmd(socketPath),
		newTasksCmd(socketPath),
		newWorktreeCmd(),
		newCICmd(),

		// Agent
		newAgentCmd(),

		// Auth
		newAuthCmd(socketPath),

		// HUD
		newHudCmd(socketPath),

		// Mills (cluster operator client)
		newMillsCmd(),
		newAuditAdvisorySweepCmd(),

		// Codex Desktop session tail (cross-agent integration slice 1a)
		newCodexWatchCmd(),

		// Shell completion
		newCompletionCmd(rootCmd),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type gitLabAuditAdvisoryClient struct {
	client *clients.GitLabClient
}

func (c *gitLabAuditAdvisoryClient) ListOpenIssues(ctx context.Context) ([]audit.DigestIssue, error) {
	items, err := c.client.ListAllIssues(ctx, clients.ListIssuesOpts{
		Labels: []string{audit.AuditAdvisoryDigestLabel}, State: "opened", PerPage: 100,
	})
	if err != nil {
		return nil, err
	}
	issues := make([]audit.DigestIssue, 0, len(items))
	for _, item := range items {
		createdAt, err := time.Parse(time.RFC3339, item.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("audit advisory sweep: parse issue %d created_at: %w", item.IID, err)
		}
		updatedAt, _ := time.Parse(time.RFC3339, item.UpdatedAt)
		issues = append(issues, audit.DigestIssue{
			IID: item.IID, Title: item.Title, Description: item.Description,
			Author: item.Author.Username, State: item.State, Labels: item.Labels, CreatedAt: createdAt, UpdatedAt: updatedAt,
		})
	}
	return issues, nil
}

func (c *gitLabAuditAdvisoryClient) CloseIssue(ctx context.Context, iid int64) error {
	return c.client.CloseIssue(ctx, iid)
}

var newAuditAdvisoryClient = func(apiURL, token, project, author string) (audit.DigestIssueClient, error) {
	client, err := clients.NewGitLabClient(clients.GitLabConfig{APIURL: apiURL, Token: token, Project: project})
	if err != nil {
		return nil, err
	}
	return &gitLabAuditAdvisoryClient{client: client}, nil
}

func newAuditAdvisorySweepCmd() *cobra.Command {
	var apply, dryRun bool
	var cutoff, author, apiURL, project, token string
	cmd := &cobra.Command{
		Use:   "audit-advisory-sweep",
		Short: "Preview or close stale audit advisory digest issues",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && dryRun {
				return errors.New("--dry-run and --apply are mutually exclusive")
			}
			if project == "" {
				project = strings.TrimSpace(os.Getenv("GITLAB_PROJECT"))
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("GITLAB_PERSONAL_ACCESS_TOKEN"))
				if token == "" {
					token = strings.TrimSpace(os.Getenv("GITLAB_TOKEN"))
				}
			}
			if project == "" {
				return errors.New("audit advisory sweep: GitLab project required (set --project or GITLAB_PROJECT)")
			}
			if token == "" {
				return errors.New("audit advisory sweep: GitLab token required (set --token, GITLAB_PERSONAL_ACCESS_TOKEN, or GITLAB_TOKEN)")
			}
			opts, err := audit.ParseAdvisorySweepFlags(nil, time.Now())
			if err != nil {
				return err
			}
			opts.Apply, opts.Author = apply, author
			if cutoff != "" {
				opts.Cutoff, err = time.Parse(time.RFC3339, cutoff)
				if err != nil {
					return fmt.Errorf("audit advisory sweep: invalid --cutoff: %w", err)
				}
			}
			client, err := newAuditAdvisoryClient(apiURL, token, project, author)
			if err != nil {
				return fmt.Errorf("audit advisory sweep: configure GitLab: %w", err)
			}
			result, err := audit.SweepAuditAdvisories(cmd.Context(), client, opts)
			mode := "dry-run"
			if apply {
				mode = "apply"
			}
			if _, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "mode=%s cutoff=%s selected=%d closed=%d\n", mode, result.Cutoff.Format(time.RFC3339), len(result.Selected), len(result.Closed)); writeErr != nil {
				return writeErr
			}
			for _, issue := range result.Digests {
				if _, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "- #%d %s age=%s recently_modified=%t\n", issue.IID, issue.Title, issue.Age, issue.RecentlyModified); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report every open digest without mutations")
	cmd.Flags().BoolVar(&apply, "apply", false, "close selected issues (default: dry-run)")
	cmd.Flags().StringVar(&cutoff, "cutoff", "", "select issues created strictly before this RFC3339 time")
	cmd.Flags().StringVar(&author, "author", audit.AuditAdvisoryDigestAuthor, "exact digest bot username")
	cmd.Flags().StringVar(&apiURL, "gitlab-api-url", "https://gitlab.flexinfer.ai/api/v4", "GitLab API base URL")
	cmd.Flags().StringVar(&project, "project", "", "GitLab project path or ID (default: GITLAB_PROJECT)")
	cmd.Flags().StringVar(&token, "token", "", "GitLab token (default: GITLAB_PERSONAL_ACCESS_TOKEN or GITLAB_TOKEN)")
	return cmd
}

func newCICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Inspect CI logs and results",
	}
	cmd.AddCommand(newCIClassifyCmd())
	return cmd
}

func newCIClassifyCmd() *cobra.Command {
	var (
		filePath   string
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "classify [file]",
		Short: "Classify CI logs by failure type",
		Long: `Classify a CI log using the same closed failure taxonomy Mills uses
for retry and escalation decisions. Read from --file, a positional file, or
stdin when no file is provided.`,
		Example: `  loom ci classify --file job.log
  cat job.log | loom ci classify
  loom ci classify job.log --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filePath != "" && len(args) > 0 {
				return fmt.Errorf("provide either --file or a positional file, not both")
			}
			source := filePath
			if source == "" && len(args) > 0 {
				source = args[0]
			}
			return runCIClassifyCommand(cmd.OutOrStdout(), cmd.InOrStdin(), source, jsonOutput)
		},
	}

	cmd.Flags().StringVarP(&filePath, "file", "f", "", "Read CI log from file instead of stdin")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON instead of a text summary")
	return cmd
}

func runCIClassifyCommand(out io.Writer, in io.Reader, filePath string, jsonOutput bool) error {
	data, err := readCIClassifyInput(in, filePath)
	if err != nil {
		return err
	}
	result := ci.ClassifyCILog(data)
	if jsonOutput {
		return render.JSON(out, result)
	}
	_, err = fmt.Fprintf(out, "CI classification: %s\nSummary: %s\nRetryable: %t\nFree retry: %t\nTerminal: %t\nInput: %d bytes, %d lines\n",
		result.Class, result.Summary, result.Retryable, result.FreeRetry, result.Terminal, result.Bytes, result.Lines)
	if err != nil {
		return err
	}
	if len(result.Evidence) > 0 {
		if _, err := fmt.Fprintln(out, "Evidence:"); err != nil {
			return err
		}
		for _, line := range result.Evidence {
			if _, err := fmt.Fprintf(out, "- %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}

func readCIClassifyInput(in io.Reader, filePath string) ([]byte, error) {
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read CI log %q: %w", filePath, err)
		}
		return data, nil
	}
	if in == nil {
		in = os.Stdin
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("read CI log from stdin: %w", err)
	}
	return data, nil
}
