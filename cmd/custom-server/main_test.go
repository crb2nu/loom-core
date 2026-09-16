package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

func TestSharedChildFixture(t *testing.T) {
	if os.Getenv("CUSTOM_SERVER_SHARED_TEST_CHILD") != "1" {
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	scanner := bufio.NewScanner(os.Stdin)
	state := 0
	for scanner.Scan() {
		var msg mcp.Message
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		if msg.Method == "test/drain" {
			_ = os.WriteFile(os.Getenv("CUSTOM_SERVER_DRAIN_MARKER"), []byte("started"), 0600)
			<-sig
		}
		if msg.Method == "test/exit" {
			os.Exit(0)
		}
		if msg.Method == "test/wait" {
			time.Sleep(100 * time.Millisecond)
		}
		if msg.Method == "test/slow" {
			// Long enough for several client keepalive pings to fall due.
			time.Sleep(1500 * time.Millisecond)
		}
		if msg.Method == "test/increment" {
			state++
		}
		result, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "state": state, "method": msg.Method})
		response, _ := json.Marshal(&mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: msg.ID, Result: result})
		_, _ = fmt.Fprintln(os.Stdout, string(response))
		if msg.Method == "test/drain" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func newTestSharedSupervisor(t *testing.T) *sharedChildSupervisor {
	t.Helper()
	t.Setenv("CUSTOM_SERVER_SHARED_TEST_CHILD", "1")
	s := newSharedChildSupervisor("fixture", os.Args[0]+" -test.run=TestSharedChildFixture --")
	t.Cleanup(s.close)
	return s
}

func sharedCall(t *testing.T, s *sharedChildSupervisor, id any, method string) map[string]any {
	t.Helper()
	resp, err := s.call(context.Background(), &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: id, Method: method})
	if err != nil {
		t.Fatalf("call %s: %v", method, err)
	}
	if fmt.Sprint(resp.ID) != fmt.Sprint(id) {
		t.Fatalf("response id=%v, want %v", resp.ID, id)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSharedChildReusesProcessAndState(t *testing.T) {
	s := newTestSharedSupervisor(t)
	first := sharedCall(t, s, 1, "initialize")
	sharedCall(t, s, 2, "test/increment")
	second := sharedCall(t, s, 3, "test/get")
	if first["pid"] != second["pid"] {
		t.Fatalf("pid changed: %v -> %v", first["pid"], second["pid"])
	}
	if second["state"] != float64(1) {
		t.Fatalf("state=%v, want 1", second["state"])
	}
}

func TestSharedChildIsolatesConcurrentRequestIDs(t *testing.T) {
	s := newTestSharedSupervisor(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := sharedCall(t, s, 7, fmt.Sprintf("test/%d", i))
			if result["method"] != fmt.Sprintf("test/%d", i) {
				t.Errorf("misrouted response: %+v", result)
			}
		}()
	}
	wg.Wait()
}

func TestSharedChildRestartsAfterExit(t *testing.T) {
	s := newTestSharedSupervisor(t)
	first := sharedCall(t, s, 1, "test/get")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.call(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: 2, Method: "test/exit"})
	second := sharedCall(t, s, 3, "test/get")
	if first["pid"] == second["pid"] {
		t.Fatalf("pid did not change after child exit: %v", first["pid"])
	}
}

func TestSharedChildSurvivesDisconnectedCaller(t *testing.T) {
	s := newTestSharedSupervisor(t)
	first := sharedCall(t, s, 1, "test/get")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.call(ctx, &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: 2, Method: "test/wait"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call error=%v", err)
	}
	second := sharedCall(t, s, 3, "test/get")
	if first["pid"] != second["pid"] {
		t.Fatalf("disconnect restarted child: %v -> %v", first["pid"], second["pid"])
	}
}

func TestDefaultWebSocketModeSpawnsPerSession(t *testing.T) {
	shuttingDown.Store(false)
	t.Setenv("CUSTOM_SERVER_SHARED_TEST_CHILD", "1")
	command := os.Args[0] + " -test.run=TestSharedChildFixture --"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleWS(w, r, "fixture", command) }))
	defer server.Close()
	call := func() float64 {
		conn, dialResp, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
		if dialResp != nil && dialResp.Body != nil {
			_ = dialResp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.WriteJSON(&mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: 1, Method: "test/get"}); err != nil {
			t.Fatal(err)
		}
		var resp mcp.Message
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result["pid"].(float64)
	}
	if first, second := call(), call(); first == second {
		t.Fatalf("default mode reused pid %v", first)
	}
}

func TestSharedWebSocketSessionsReuseState(t *testing.T) {
	shuttingDown.Store(false)
	s := newTestSharedSupervisor(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleSharedWS(w, r, s) }))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]
	call := func(id int, method string) map[string]any {
		conn, dialResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if dialResp != nil && dialResp.Body != nil {
			_ = dialResp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.WriteJSON(&mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: id, Method: method}); err != nil {
			t.Fatal(err)
		}
		var resp mcp.Message
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := call(1, "test/increment")
	second := call(1, "test/get")
	if first["pid"] != second["pid"] || second["state"] != float64(1) {
		t.Fatalf("sessions did not share child state: first=%+v second=%+v", first, second)
	}
}

// TestSharedWebSocketAnswersPingsDuringLongCall pins the 2026-09-06
// regression: with the supervisor call made inline in the read goroutine, no
// control frames were processed for the whole call, so a client that pings
// and waits for pongs (the Mills operator's mcp-go transport) tore the
// connection down at its 60s pong wait — every quality-gate call died with
// "websocket: close 1006". Pongs must keep flowing while a call is in flight,
// and the response must still arrive.
func TestSharedWebSocketAnswersPingsDuringLongCall(t *testing.T) {
	shuttingDown.Store(false)
	s := newTestSharedSupervisor(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleSharedWS(w, r, s) }))
	defer server.Close()

	conn, dialResp, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var pongs atomic.Int32
	conn.SetPongHandler(func(string) error { pongs.Add(1); return nil })
	var writeMu sync.Mutex
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
				writeMu.Unlock()
			}
		}
	}()

	writeMu.Lock()
	err = conn.WriteJSON(&mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: 1, Method: "test/slow"})
	writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp mcp.Message
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("response never arrived (connection closed?): %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["method"] != "test/slow" {
		t.Fatalf("unexpected response: %+v", result)
	}
	if got := pongs.Load(); got < 3 {
		t.Fatalf("only %d pongs arrived during a 1.5s call; the read loop was blocked", got)
	}
}

type fakeWSWriter struct {
	active    atomic.Int32
	maxActive atomic.Int32
	writes    atomic.Int32
}

func (f *fakeWSWriter) WriteMessage(_ int, _ []byte) error {
	active := f.active.Add(1)
	for {
		currentMax := f.maxActive.Load()
		if active <= currentMax {
			break
		}
		if f.maxActive.CompareAndSwap(currentMax, active) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	f.writes.Add(1)
	f.active.Add(-1)
	return nil
}

func TestWriteWSSerializesConcurrentWriters(t *testing.T) {
	var mu sync.Mutex
	writer := &fakeWSWriter{}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeWS(&mu, writer, 1, []byte("x")); err != nil {
				t.Errorf("writeWS() error = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := writer.writes.Load(); got != 32 {
		t.Fatalf("writes = %d, want 32", got)
	}
	if got := writer.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent writes = %d, want 1", got)
	}
}

func TestReadyHandlerFailsWhileDraining(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	rec := httptest.NewRecorder()
	readyHandler(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready before drain = %d, want %d", rec.Code, http.StatusOK)
	}

	shuttingDown.Store(true)
	rec = httptest.NewRecorder()
	readyHandler(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready during drain = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestDrainRegistryDrainAllInvokesEachOnce(t *testing.T) {
	d := newDrainRegistry()
	var count atomic.Int32
	for i := range 8 {
		d.Add(fmt.Sprintf("c%d", i), func() { count.Add(1) })
	}
	if got := d.Len(); got != 8 {
		t.Fatalf("Len() = %d, want 8", got)
	}
	d.DrainAll()
	if got := count.Load(); got != 8 {
		t.Fatalf("DrainAll invoked %d fns, want 8", got)
	}
}

func TestDrainRegistryRemoveSkipsClosed(t *testing.T) {
	d := newDrainRegistry()
	var invoked atomic.Bool
	d.Add("x", func() { invoked.Store(true) })
	d.Remove("x")
	if got := d.Len(); got != 0 {
		t.Fatalf("Len() after Remove = %d, want 0", got)
	}
	d.DrainAll()
	if invoked.Load() {
		t.Fatal("removed closeFn must not be invoked by DrainAll")
	}
}

func TestEnvDurationParsesAndFallsBack(t *testing.T) {
	if got := envDuration("MCP_TEST_DUR_UNSET_XYZ", 3*time.Second); got != 3*time.Second {
		t.Fatalf("unset = %s, want 3s", got)
	}
	t.Setenv("MCP_TEST_DUR", "250ms")
	if got := envDuration("MCP_TEST_DUR", time.Second); got != 250*time.Millisecond {
		t.Fatalf("set = %s, want 250ms", got)
	}
	t.Setenv("MCP_TEST_DUR", "not-a-duration")
	if got := envDuration("MCP_TEST_DUR", time.Second); got != time.Second {
		t.Fatalf("invalid = %s, want 1s fallback", got)
	}
}

// TestLegacyTransportRoutesAnswerExplanatory404 pins the retirement of the
// MCP HTTP+SSE transport: /sse and /messages answer 404 with a body that
// names the live transports, while the Streamable HTTP route stays mounted.
// The WebSocket path is proven by TestDefaultWebSocketModeSpawnsPerSession.
func TestLegacyTransportRoutesAnswerExplanatory404(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", newStreamableHTTPHandler("unused", "unused", newDrainRegistry()))
	registerLegacyTransportRemoved(mux)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/sse"},
		{http.MethodPost, "/messages?session_id=abc"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "/mcp") || !strings.Contains(body, "/ws") {
			t.Fatalf("%s %s body must name the live transports, got %q", tc.method, tc.path, body)
		}
	}

	// The live Streamable HTTP route is untouched: it is mounted and answers
	// something other than the mux's not-found.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("/mcp must remain mounted, got 404")
	}
}

func waitDrainMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child did not admit the request")
}

func TestSharedWebSocketDrainDeliversResult(t *testing.T) {
	shuttingDown.Store(false)
	defer shuttingDown.Store(false)
	s := newTestSharedSupervisor(t)
	marker := filepath.Join(t.TempDir(), "entered")
	t.Setenv("CUSTOM_SERVER_DRAIN_MARKER", marker)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleSharedWS(w, r, s) }))
	defer server.Close()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(&mcp.Message{JSONRPC: "2.0", ID: 1, Method: "test/drain"}); err != nil {
		t.Fatal(err)
	}
	waitDrainMarker(t, marker)
	shuttingDown.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.beginDrain(ctx); close(done) }()
	var result mcp.Message
	if err := conn.ReadJSON(&result); err != nil {
		t.Fatalf("lost drain result: %v", err)
	}
	if result.Error != nil || !strings.Contains(string(result.Result), "test/drain") {
		t.Fatalf("bad response: %+v", result)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("wrapper did not finish drain")
	}
	if s.admitDelivery(&mcp.Message{Method: "tools/call"}) {
		t.Fatal("new call admitted")
	}
	if _, _, err := s.childForRequest(); err == nil {
		t.Fatal("draining child respawned")
	}
	for path, handler := range map[string]http.HandlerFunc{"/ready": readyHandler, "/health": okHandler} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, path, nil))
		want := http.StatusOK
		if path == "/ready" {
			want = http.StatusServiceUnavailable
		}
		if w.Code != want {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
}

func TestSharedDrainBoundsHungChild(t *testing.T) {
	s := newTestSharedSupervisor(t)
	sharedCall(t, s, 1, "test/get")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.beginDrain(ctx) // fixture catches SIGTERM but keeps reading stdin
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("unbounded shutdown: %v", elapsed)
	}
}
