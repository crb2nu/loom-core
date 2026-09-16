package reliability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

func reliabilityMixedLoadRatio(t *testing.T) float64 {
	t.Helper()
	value := os.Getenv("LOOM_RELIABILITY_MIXED_LOAD_RATIO")
	if value == "" {
		return 0.5
	}
	ratio, err := parseMixedLoadRatio(value)
	if err != nil {
		t.Fatal(err)
	}
	return ratio
}

func parseMixedLoadRatio(value string) (float64, error) {
	ratio, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(ratio) || ratio <= 0 || ratio > 1 {
		return 0, fmt.Errorf("invalid LOOM_RELIABILITY_MIXED_LOAD_RATIO %q (want > 0 and <= 1)", value)
	}
	return ratio, nil
}

func TestFleetReliabilityMixedLoad(t *testing.T) {
	if testing.Short() || os.Getenv("LOOM_RUN_RELIABILITY") != "1" {
		t.Skip("mixed-load soak runs only in the fleet reliability gate")
	}

	duration := reliabilityLoadDuration(t)
	calibrationDuration := duration / 5
	if calibrationDuration > 2*time.Second {
		calibrationDuration = 2 * time.Second
	}
	if calibrationDuration <= 0 {
		calibrationDuration = time.Millisecond
	}

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
	eventReceiverDone := make(chan struct{})
	go func() {
		defer close(eventReceiverDone)
		for range events {
			eventReceived.Add(1)
		}
	}()

	// Measure each operation alone immediately before the mixed phase. The
	// operation pacing is identical in both phases, so host scheduling and I/O
	// contention affect the comparison without imposing a machine-speed floor.
	baselineCounts := map[string]int64{}
	baselineCounts["event_publish"] = calibrateWorkload(calibrationDuration, 5*time.Millisecond, func() error {
		bus.Publish(daemon.EventType("fleet.baseline"), nil)
		return nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for eventReceived.Load() < baselineCounts["event_publish"] && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if received := eventReceived.Load(); received != baselineCounts["event_publish"] {
		t.Fatalf("baseline received events = %d, published events = %d", received, baselineCounts["event_publish"])
	}
	baselineCounts["event_receive"] = eventReceived.Swap(0)
	baselineCounts["mux_call"] = calibrateWorkload(calibrationDuration, 10*time.Millisecond, func() error {
		message := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: time.Now().UnixNano(), Method: "tools/list"}
		_, callErr := mux.Call(context.Background(), message)
		return callErr
	})
	baselineCounts["store_write"] = calibrateWorkload(calibrationDuration, 20*time.Millisecond, func() error {
		return millsStore.Events.Append(context.Background(), &store.Event{
			Actor: "fleet-baseline", Kind: "baseline.append", SubjectKind: "iteration", SubjectID: fmt.Sprint(time.Now().UnixNano()),
		})
	})
	baselineCounts["pool_cycle"] = calibrateWorkload(calibrationDuration, 5*time.Millisecond, func() error {
		conn, getErr := connectionPool.Get(context.Background(), "fleet")
		if getErr == nil {
			connectionPool.Put(conn)
		}
		return getErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	errCh := make(chan error, 1)
	reportError := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	var producers sync.WaitGroup
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
	ratioFloor := reliabilityMixedLoadRatio(t)
	ratios, err := assertMixedLoadRatios(counts, baselineCounts, duration, calibrationDuration, ratioFloor)
	if err != nil {
		t.Error(err)
	}
	t.Logf("RELIABILITY_SCENARIO advisory throughput expectations: event_publish=200/s event_receive=200/s mux_call=100/s store_write=50/s pool_cycle=200/s")

	var durableEvents int64
	if err := millsStore.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE actor = 'fleet-load' AND kind = 'mixed.append'`).Scan(&durableEvents); err != nil {
		t.Fatalf("count durable events: %v", err)
	}
	if durableEvents != storeWrites.Load() {
		t.Fatalf("durable events = %d, successful writes = %d", durableEvents, storeWrites.Load())
	}

	snapshot, err := json.Marshal(map[string]any{
		"duration":              duration.String(),
		"calibration_duration":  calibrationDuration.String(),
		"scenario_count":        5,
		"counts":                counts,
		"baseline_counts":       baselineCounts,
		"mixed_baseline_ratios": ratios,
		"ratio_floor":           ratioFloor,
	})
	if err != nil {
		t.Fatalf("marshal load snapshot: %v", err)
	}
	t.Logf("RELIABILITY_SCENARIO %s", snapshot)
}

func calibrateWorkload(duration, interval time.Duration, operation func() error) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var count int64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return count
		case <-ticker.C:
			if operation() != nil {
				return count
			}
			count++
		}
	}
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

func assertMixedLoadRatios(mixed, baseline map[string]int64, mixedDuration, baselineDuration time.Duration, floor float64) (map[string]float64, error) {
	ratios := make(map[string]float64, len(baseline))
	names := make([]string, 0, len(baseline))
	for name := range baseline {
		names = append(names, name)
	}
	sort.Strings(names)
	var failures []error
	for _, name := range names {
		baselineCount := baseline[name]
		if baselineCount == 0 {
			failures = append(failures, fmt.Errorf("%s baseline count is zero; calibration is inconclusive", name))
			continue
		}
		ratio := (float64(mixed[name]) / mixedDuration.Seconds()) / (float64(baselineCount) / baselineDuration.Seconds())
		ratios[name] = ratio
		if ratio < floor {
			failures = append(failures, fmt.Errorf("%s mixed/baseline throughput ratio = %.3f (mixed=%d baseline=%d), want at least %.3f", name, ratio, mixed[name], baselineCount, floor))
		}
	}
	return ratios, errors.Join(failures...)
}

func TestAssertMixedLoadRatios(t *testing.T) {
	tests := []struct {
		name        string
		mixed       int64
		baseline    int64
		wantMessage string
		wantErr     bool
	}{
		{name: "boundary passes", mixed: 50, baseline: 100},
		{name: "below threshold", mixed: 49, baseline: 100, wantErr: true, wantMessage: "ratio = 0.490"},
		{name: "zero baseline", mixed: 50, baseline: 0, wantErr: true, wantMessage: "calibration is inconclusive"},
		{name: "zero mixed progress", mixed: 0, baseline: 100, wantErr: true, wantMessage: "mixed=0 baseline=100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := assertMixedLoadRatios(map[string]int64{"scenario": tt.mixed}, map[string]int64{"scenario": tt.baseline}, time.Second, time.Second, 0.5)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assertMixedLoadRatios() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantMessage != "" && !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("error %q does not contain %q", err, tt.wantMessage)
			}
		})
	}
}

func TestParseMixedLoadRatioRejectsNaN(t *testing.T) {
	if _, err := parseMixedLoadRatio("NaN"); err == nil {
		t.Fatal("parseMixedLoadRatio(NaN) succeeded, want error")
	}
}

func TestAssertMixedLoadRatiosReportsAllWorkloads(t *testing.T) {
	ratios, err := assertMixedLoadRatios(
		map[string]int64{"event_publish": 0, "store_write": 0},
		map[string]int64{"event_publish": 100, "store_write": 100},
		time.Second,
		time.Second,
		0.5,
	)
	if err == nil {
		t.Fatal("assertMixedLoadRatios() succeeded, want error")
	}
	for _, name := range []string{"event_publish", "store_write"} {
		if ratios[name] != 0 || !strings.Contains(err.Error(), name) {
			t.Errorf("missing diagnostics for %s: ratios=%v error=%q", name, ratios, err)
		}
	}
}
