// loom is the CLI for interacting with the Loom daemon.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	ci "github.com/crb2nu/loom/cmd/loom/internal"
	render "github.com/crb2nu/loom/cmd/loom/internal/render"
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

		// Codex Desktop session tail (cross-agent integration slice 1a)
		newCodexWatchCmd(),

		// Shell completion
		newCompletionCmd(rootCmd),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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
