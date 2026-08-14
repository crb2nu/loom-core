package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/pkg/registry"
)

type processControllerTestTransport struct{}

func (*processControllerTestTransport) Send(context.Context, *mcp.Message) error { return nil }

func (*processControllerTestTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*processControllerTestTransport) Close() error { return nil }

type processControllerTestShard struct {
	dial func(context.Context, string) (mcp.Transport, error)
	stop func(string) error
}

func (s *processControllerTestShard) Dial(ctx context.Context, serverName string) (mcp.Transport, error) {
	if s.dial == nil {
		return &processControllerTestTransport{}, nil
	}
	return s.dial(ctx, serverName)
}

func (s *processControllerTestShard) Stop(serverName string) error {
	if s.stop == nil {
		return nil
	}
	return s.stop(serverName)
}

func newControllerBackedSupervisor(controller localProcessController) *serverSupervisor {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	supervisor := newServerSupervisor(controller, false, logger, nil)
	supervisor.initMCP = func(context.Context, mcp.Transport) error { return nil }
	supervisor.initLimit = time.Second
	return supervisor
}

func TestShardedProcessControllerBlockedDialDoesNotPinAnotherServerGeneration(t *testing.T) {
	alphaDialEntered := make(chan struct{})
	var alphaEnteredOnce sync.Once
	controller := newShardedProcessControllerWithFactory(nil, "codex", "/repo", func(serverName string, _ processControllerConfig) (localProcessController, error) {
		shard := &processControllerTestShard{}
		if serverName == "alpha" {
			shard.dial = func(ctx context.Context, _ string) (mcp.Transport, error) {
				alphaEnteredOnce.Do(func() { close(alphaDialEntered) })
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		return shard, nil
	})
	supervisor := newControllerBackedSupervisor(controller)

	alphaDone := make(chan error, 1)
	go func() {
		_, _, _, err := supervisor.connection(context.Background(), "alpha")
		alphaDone <- err
	}()
	select {
	case <-alphaDialEntered:
	case <-time.After(time.Second):
		t.Fatal("alpha Dial did not enter")
	}

	betaCtx, cancelBeta := context.WithTimeout(context.Background(), time.Second)
	defer cancelBeta()
	_, betaGeneration, _, err := supervisor.connection(betaCtx, "beta")
	if err != nil {
		t.Fatalf("beta connection while alpha Dial blocked: %v", err)
	}
	if betaGeneration != 1 {
		t.Fatalf("beta generation = %d, want 1", betaGeneration)
	}
	if snapshot, ok := supervisor.current("beta"); !ok || snapshot.State != generation.StateReady {
		t.Fatalf("beta snapshot = %+v, found = %v, want Ready", snapshot, ok)
	}
	if snapshot, ok := supervisor.current("alpha"); !ok || snapshot.State != generation.StateConnecting {
		t.Fatalf("alpha snapshot = %+v, found = %v, want Connecting", snapshot, ok)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := supervisor.shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-alphaDone; !errors.Is(err, generation.ErrDraining) {
		t.Fatalf("alpha connection error = %v, want ErrDraining", err)
	}
}

type processConfigObservation struct {
	revision uint64
	command  string
}

func TestShardedProcessControllerRegistryAndExpansionUseOneRevision(t *testing.T) {
	t.Setenv("LOOM_PROCESS_CONTROLLER_OLD", "old-value")
	t.Setenv("LOOM_PROCESS_CONTROLLER_NEW", "new-value")
	oldRegistry := processControllerTestRegistry("old", "LOOM_PROCESS_CONTROLLER_OLD")
	newRegistry := processControllerTestRegistry("new", "LOOM_PROCESS_CONTROLLER_NEW")

	specRead := make(chan struct{})
	continueExpansion := make(chan struct{})
	observations := make(chan processConfigObservation, 4)
	var pauseOldOnce sync.Once
	controller := newShardedProcessControllerWithFactory(oldRegistry, "codex", "/repo", func(serverName string, config processControllerConfig) (localProcessController, error) {
		return &processControllerTestShard{dial: func(context.Context, string) (mcp.Transport, error) {
			spec, err := config.registry.GetServerSpec(serverName, config.target)
			if err != nil {
				return nil, err
			}
			if config.revision == 1 {
				pauseOldOnce.Do(func() {
					close(specRead)
					<-continueExpansion
				})
			}
			observations <- processConfigObservation{
				revision: config.revision,
				command:  config.expand(spec.Command),
			}
			return &processControllerTestTransport{}, nil
		}}, nil
	})

	firstDial := make(chan error, 1)
	go func() {
		_, err := controller.Dial(context.Background(), "alpha")
		firstDial <- err
	}()
	select {
	case <-specRead:
	case <-time.After(time.Second):
		t.Fatal("old shard did not read its TargetSpec")
	}

	updated := make(chan uint64, 1)
	go func() { updated <- controller.UpdateRegistry(newRegistry) }()
	select {
	case revision := <-updated:
		if revision != 2 {
			t.Fatalf("updated revision = %d, want 2", revision)
		}
	case <-time.After(time.Second):
		t.Fatal("registry update blocked behind old shard Dial")
	}
	close(continueExpansion)
	if err := <-firstDial; err != nil {
		t.Fatalf("old shard Dial: %v", err)
	}
	assertProcessConfigObservation(t, <-observations, 1, "old:old-value")

	// A live shard deliberately retains its launch revision after reload.
	if _, err := controller.Dial(context.Background(), "alpha"); err != nil {
		t.Fatalf("second old-shard Dial: %v", err)
	}
	assertProcessConfigObservation(t, <-observations, 1, "old:old-value")

	if err := controller.Stop("alpha"); err != nil {
		t.Fatalf("stop old shard: %v", err)
	}
	if _, err := controller.Dial(context.Background(), "alpha"); err != nil {
		t.Fatalf("new-shard Dial: %v", err)
	}
	assertProcessConfigObservation(t, <-observations, 2, "new:new-value")
	if err := controller.Stop("alpha"); err != nil {
		t.Fatalf("stop new shard: %v", err)
	}
}

func processControllerTestRegistry(prefix, fallback string) *registry.Registry {
	return &registry.Registry{
		EnvAliases: map[string]registry.EnvVar{
			"LOOM_PROCESS_CONTROLLER_VALUE": {Fallbacks: []string{fallback}},
		},
		Servers: []*registry.Server{{
			Name: "alpha",
			Common: &registry.TargetSpec{
				Command: prefix + ":${env:LOOM_PROCESS_CONTROLLER_VALUE}",
			},
		}},
	}
}

func assertProcessConfigObservation(t *testing.T, observation processConfigObservation, revision uint64, command string) {
	t.Helper()
	if observation.revision != revision || observation.command != command {
		t.Fatalf("observation = %+v, want revision=%d command=%q", observation, revision, command)
	}
}

func TestServerSupervisorShutdownClosesIndependentProcessShardsConcurrently(t *testing.T) {
	alphaStopEntered := make(chan struct{})
	allowAlphaStop := make(chan struct{})
	betaStopDone := make(chan struct{})
	var alphaStopOnce sync.Once
	var betaStopOnce sync.Once
	var dialContexts sync.Map

	controller := newShardedProcessControllerWithFactory(nil, "codex", "/repo", func(serverName string, _ processControllerConfig) (localProcessController, error) {
		return &processControllerTestShard{
			dial: func(ctx context.Context, _ string) (mcp.Transport, error) {
				dialContexts.Store(serverName, ctx)
				return &processControllerTestTransport{}, nil
			},
			stop: func(string) error {
				switch serverName {
				case "alpha":
					alphaStopOnce.Do(func() { close(alphaStopEntered) })
					<-allowAlphaStop
				case "beta":
					betaStopOnce.Do(func() { close(betaStopDone) })
				}
				return nil
			},
		}, nil
	})
	supervisor := newControllerBackedSupervisor(controller)

	for _, serverName := range []string{"alpha", "beta"} {
		if _, _, _, err := supervisor.connection(context.Background(), serverName); err != nil {
			t.Fatalf("connect %s: %v", serverName, err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShutdown()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- supervisor.shutdown(shutdownCtx) }()

	select {
	case <-alphaStopEntered:
	case <-time.After(time.Second):
		t.Fatal("alpha Stop did not enter")
	}
	select {
	case <-betaStopDone:
	case <-time.After(time.Second):
		t.Fatal("beta Stop serialized behind blocked alpha shard")
	}

	alphaContextValue, ok := dialContexts.Load("alpha")
	if !ok {
		t.Fatal("alpha Dial context was not recorded")
	}
	alphaContext := alphaContextValue.(context.Context)
	select {
	case <-alphaContext.Done():
	default:
		t.Fatal("alpha process context was not canceled before blocked Stop")
	}

	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not honor its deadline")
	}

	close(allowAlphaStop)
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := supervisor.shutdown(joinCtx); err != nil {
		t.Fatalf("join shutdown after alpha release: %v", err)
	}
}

func TestReloadInvalidationDoesNotWaitForBlockedProcessShard(t *testing.T) {
	stopEntered := make(chan struct{})
	allowStop := make(chan struct{})
	var stopEnteredOnce sync.Once
	controller := newShardedProcessControllerWithFactory(nil, "codex", "/repo", func(serverName string, _ processControllerConfig) (localProcessController, error) {
		shard := &processControllerTestShard{}
		if serverName == "alpha" {
			shard.stop = func(string) error {
				stopEnteredOnce.Do(func() { close(stopEntered) })
				<-allowStop
				return nil
			}
		}
		return shard, nil
	})
	supervisor := newControllerBackedSupervisor(controller)
	_, generationID, _, err := supervisor.connection(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("connect alpha: %v", err)
	}

	oldRegistry := &registry.Registry{Servers: []*registry.Server{{
		Name:   "alpha",
		Common: &registry.TargetSpec{Command: "old-command"},
	}}}
	newRegistry := &registry.Registry{Servers: []*registry.Server{{
		Name:   "alpha",
		Common: &registry.TargetSpec{Command: "new-command"},
	}}}
	controller.UpdateRegistry(newRegistry)
	d := &Daemon{
		cfg:                 Config{Target: "codex"},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		localProcController: controller,
		serverSupervisor:    supervisor,
	}

	invalidatedDone := make(chan []string, 1)
	go func() {
		invalidatedDone <- d.invalidateServersForReload(oldRegistry, newRegistry, map[string]uint64{"alpha": generationID})
	}()
	select {
	case invalidated := <-invalidatedDone:
		if len(invalidated) != 1 || invalidated[0] != "alpha" {
			t.Fatalf("invalidated = %v, want [alpha]", invalidated)
		}
	case <-time.After(time.Second):
		t.Fatal("reload invalidation blocked in alpha Stop")
	}
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("alpha asynchronous Stop did not enter")
	}

	// Another key can publish while alpha remains Closing.
	betaCtx, cancelBeta := context.WithTimeout(context.Background(), time.Second)
	defer cancelBeta()
	if _, _, _, err := supervisor.connection(betaCtx, "beta"); err != nil {
		t.Fatalf("connect beta while alpha Stop blocked: %v", err)
	}

	close(allowStop)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := supervisor.shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown after alpha release: %v", err)
	}
}
