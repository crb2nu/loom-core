package secrets

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

const commandWaitDelay = time.Second

// CommandExecutor is an interface for executing external commands.
// This allows mocking command execution in tests.
type CommandExecutor interface {
	// Run executes a command and returns stdout, stderr, and any error.
	Run(name string, args ...string) (stdout, stderr []byte, err error)
	// LookPath checks if a command is available in PATH.
	LookPath(file string) (string, error)
}

// ContextCommandExecutor is the optional cancellation-aware extension to
// CommandExecutor. Keeping this separate preserves compatibility with existing
// embedders and test doubles while allowing long-running credential helpers to
// be interrupted during daemon shutdown.
type ContextCommandExecutor interface {
	RunContext(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// RealCommandExecutor executes commands using os/exec.
type RealCommandExecutor struct{}

// Run executes a command and returns its output.
func (r *RealCommandExecutor) Run(name string, args ...string) (stdout, stderr []byte, err error) {
	return r.RunContext(context.Background(), name, args...)
}

// RunContext executes a command and kills it if ctx is canceled.
func (r *RealCommandExecutor) RunContext(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	configureCommandCancellation(cmd)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// runCommandContext uses cancellation when the executor supports it and falls
// back to the legacy synchronous contract for third-party implementations.
func runCommandContext(ctx context.Context, executor CommandExecutor, name string, args ...string) (stdout, stderr []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if contextual, ok := executor.(ContextCommandExecutor); ok {
		return contextual.RunContext(ctx, name, args...)
	}
	return executor.Run(name, args...)
}

// LookPath checks if a command is available in PATH.
func (r *RealCommandExecutor) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

// defaultExecutor is the global command executor.
// In tests, this can be replaced with a mock.
var defaultExecutor CommandExecutor = &RealCommandExecutor{}

// SetExecutor sets the command executor for testing.
// Returns the previous executor for restoration.
func SetExecutor(e CommandExecutor) CommandExecutor {
	prev := defaultExecutor
	defaultExecutor = e
	return prev
}
