package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/secrets"
)

type blockingContextSecretExecutor struct {
	started chan struct{}
	once    sync.Once
}

func (e *blockingContextSecretExecutor) LookPath(file string) (string, error) {
	if file == "op" {
		return "/test/op", nil
	}
	return "", errors.New("command unavailable")
}

func (e *blockingContextSecretExecutor) Run(_ string, _ ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("legacy non-context command path invoked")
}

func (e *blockingContextSecretExecutor) RunContext(ctx context.Context, _ string, _ ...string) ([]byte, []byte, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func TestToolRefreshWorkStopCancelsAndJoins(t *testing.T) {
	d := &Daemon{}
	root, done, err := d.claimCacheRefreshWork()
	if err != nil {
		t.Fatalf("claimCacheRefreshWork() error = %v", err)
	}
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	go func() {
		<-root.Done()
		close(cancelObserved)
		<-release
		done()
	}()

	stopDone := make(chan struct{})
	go func() {
		d.stopCacheRefreshWork()
		close(stopDone)
	}()
	awaitSignal(t, cancelObserved, "tracked tool refresh cancellation")
	select {
	case <-stopDone:
		t.Fatal("stopCacheRefreshWork returned before tracked worker exited")
	default:
	}
	close(release)
	awaitSignal(t, stopDone, "tracked tool refresh join")

	if _, _, err := d.claimCacheRefreshWork(); !errors.Is(err, errCacheRefreshStopped) {
		t.Fatalf("claim after stop error = %v, want errCacheRefreshStopped", err)
	}
	if !errors.Is(root.Err(), context.Canceled) {
		t.Fatalf("refresh root error = %v, want context.Canceled", root.Err())
	}

	// Repeated stop calls remain harmless and return promptly.
	secondStop := make(chan struct{})
	go func() {
		d.stopCacheRefreshWork()
		close(secondStop)
	}()
	select {
	case <-secondStop:
	case <-time.After(time.Second):
		t.Fatal("repeated stopCacheRefreshWork blocked")
	}
}

func TestToolRefreshSecretExpansionStopCancelsBlockingCommand(t *testing.T) {
	t.Setenv("MCP_HUB_TOKEN", "")
	// Prevent the fallback encrypted-file backend from consulting a real user
	// keychain after the intentionally blocked op discovery is canceled.
	t.Setenv("LOOM_MASTER_KEY", "tool-refresh-cancellation-test-key")

	executor := &blockingContextSecretExecutor{started: make(chan struct{})}
	previous := secrets.SetExecutor(executor)
	t.Cleanup(func() { secrets.SetExecutor(previous) })

	d := &Daemon{}
	root, done, err := d.claimCacheRefreshWork()
	if err != nil {
		t.Fatalf("claimCacheRefreshWork() error = %v", err)
	}
	expansionDone := make(chan struct{})
	go func() {
		defer done()
		_, _ = resolveHubToken(root, "", nil)
		close(expansionDone)
	}()
	awaitSignal(t, executor.started, "blocking secret command to start")

	stopDone := make(chan struct{})
	go func() {
		d.stopCacheRefreshWork()
		close(stopDone)
	}()
	awaitSignal(t, stopDone, "tool refresh stop after command cancellation")
	awaitSignal(t, expansionDone, "secret expansion to exit")
	if !errors.Is(root.Err(), context.Canceled) {
		t.Fatalf("refresh root error = %v, want context.Canceled", root.Err())
	}
}

func TestResolveHubTokenEnvironmentFastPathSkipsSecretCommands(t *testing.T) {
	t.Setenv("MCP_HUB_TOKEN", "environment-token")
	executor := &blockingContextSecretExecutor{started: make(chan struct{})}
	previous := secrets.SetExecutor(executor)
	t.Cleanup(func() { secrets.SetExecutor(previous) })

	token, err := resolveHubToken(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("resolveHubToken() error = %v", err)
	}
	if token != "environment-token" {
		t.Fatalf("resolveHubToken() = %q, want environment token", token)
	}
	select {
	case <-executor.started:
		t.Fatal("environment fast path initialized an external secret command")
	default:
	}
}
