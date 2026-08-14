package reliability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/daemon"
	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

type loadEchoTransport struct {
	responses chan *mcp.Message
	done      chan struct{}
	closeOnce sync.Once
}

func newLoadEchoTransport() *loadEchoTransport {
	return &loadEchoTransport{responses: make(chan *mcp.Message, 8), done: make(chan struct{})}
}

func (t *loadEchoTransport) Send(ctx context.Context, message *mcp.Message) error {
	response := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: message.ID, Result: json.RawMessage(`{"ok":true}`)}
	select {
	case t.responses <- response:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return io.EOF
	}
}

func (t *loadEchoTransport) Recv(ctx context.Context) (*mcp.Message, error) {
	select {
	case response := <-t.responses:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

func (t *loadEchoTransport) Close() error {
	t.closeOnce.Do(func() { close(t.done) })
	return nil
}

func reliabilityLoadDuration(t *testing.T) time.Duration {
	t.Helper()
	value := os.Getenv("LOOM_RELIABILITY_LOAD_DURATION")
	if value == "" {
		return 60 * time.Second
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		t.Fatalf("invalid LOOM_RELIABILITY_LOAD_DURATION %q", value)
	}
	return duration
}

func TestFleetReliabilityMixedLoad(t *testing.T) {
	if testing.Short() || os.Getenv("LOOM_RUN_RELIABILITY") != "1" {
		t.Skip("mixed-load soak runs only in the fleet reliability gate")
	}

	duration := reliabilityLoadDuration(t)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	bus := daemon.NewEventBus(slog.New(slog.DiscardHandler))
	subscriberID, events := bus.SubscribeWithBuffer(4096)

	muxInner := newLoadEchoTransport()
	mux := muxstdio.New(muxInner)
	defer mux.Close()

	millsStore, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "mixed-load.db"),
	})
	if err != nil {
		t.Fatalf("open Mills store: %v", err)
	}
	defer millsStore.Close()

	connectionPool := pool.New(pool.Config{
		MaxIdle:     4,
		MaxOpen:     4,
		IdleTimeout: time.Minute,
		DialFunc: func(context.Context, string) (mcp.Transport, error) {
			return newLoadEchoTransport(), nil
		},
	})
	defer connectionPool.Close()

	var eventPublished atomic.Int64
	var eventReceived atomic.Int64
	var muxCalls atomic.Int64
	var storeWrites atomic.Int64
	var poolCycles atomic.Int64
	errCh := make(chan error, 1)
	reportError := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	var producers sync.WaitGroup
	eventReceiverDone := make(chan struct{})
	go func() {
		defer close(eventReceiverDone)
		for range events {
			eventReceived.Add(1)
		}
	}()
	producers.Add(4)
	go runPaced(ctx, &producers, 5*time.Millisecond, func() error {
		sequence := eventPublished.Add(1)
		bus.Publish(daemon.EventType("fleet.mixed"), map[string]any{"sequence": sequence})
		return nil
	}, reportError)
	go runPaced(ctx, &producers, 10*time.Millisecond, func() error {
		sequence := muxCalls.Add(1)
		message := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: sequence, Method: "tools/list"}
		_, err := mux.Call(context.Background(), message)
		return err
	}, reportError)
	go runPaced(ctx, &producers, 20*time.Millisecond, func() error {
		sequence := storeWrites.Add(1)
		return millsStore.Events.Append(context.Background(), &store.Event{
			Actor:       "fleet-load",
			Kind:        "mixed.append",
			SubjectKind: "iteration",
			SubjectID:   fmt.Sprintf("%d", sequence),
		})
	}, reportError)
	go runPaced(ctx, &producers, 5*time.Millisecond, func() error {
		conn, err := connectionPool.Get(context.Background(), "fleet")
		if err != nil {
			return err
		}
		poolCycles.Add(1)
		connectionPool.Put(conn)
		return nil
	}, reportError)

	<-ctx.Done()
	producers.Wait()
	bus.Unsubscribe(subscriberID)
	<-eventReceiverDone
	select {
	case workerErr := <-errCh:
		t.Fatalf("mixed-load worker failed: %v", workerErr)
	default:
	}

	counts := map[string]int64{
		"event_publish": eventPublished.Load(),
		"event_receive": eventReceived.Load(),
		"mux_call":      muxCalls.Load(),
		"store_write":   storeWrites.Load(),
		"pool_cycle":    poolCycles.Load(),
	}
	if dropped := bus.DroppedCount(); dropped != 0 {
		t.Fatalf("EventBus dropped %d events during mixed load", dropped)
	}
	if counts["event_receive"] != counts["event_publish"] {
		t.Fatalf("received events = %d, published events = %d", counts["event_receive"], counts["event_publish"])
	}
	minimums := map[string]int64{
		"event_publish": minimumLoadCount(duration, 200),
		"event_receive": minimumLoadCount(duration, 200),
		"mux_call":      minimumLoadCount(duration, 100),
		"store_write":   minimumLoadCount(duration, 50),
		"pool_cycle":    minimumLoadCount(duration, 200),
	}
	for name, minimum := range minimums {
		if counts[name] < minimum {
			t.Errorf("%s count = %d, want at least %d", name, counts[name], minimum)
		}
	}

	var durableEvents int64
	if err := millsStore.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE actor = 'fleet-load' AND kind = 'mixed.append'`).Scan(&durableEvents); err != nil {
		t.Fatalf("count durable events: %v", err)
	}
	if durableEvents != storeWrites.Load() {
		t.Fatalf("durable events = %d, successful writes = %d", durableEvents, storeWrites.Load())
	}

	snapshot, err := json.Marshal(map[string]any{
		"duration":       duration.String(),
		"scenario_count": 5,
		"counts":         counts,
	})
	if err != nil {
		t.Fatalf("marshal load snapshot: %v", err)
	}
	t.Logf("RELIABILITY_SCENARIO %s", snapshot)
}

func runPaced(
	ctx context.Context,
	workers *sync.WaitGroup,
	interval time.Duration,
	operation func() error,
	reportError func(error),
) {
	defer workers.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := operation(); err != nil {
				reportError(err)
				return
			}
		}
	}
}

func minimumLoadCount(duration time.Duration, ratePerSecond int64) int64 {
	expected := int64(duration.Seconds() * float64(ratePerSecond))
	minimum := expected * 70 / 100
	if minimum < 1 {
		return 1
	}
	return minimum
}
