package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/transport/muxstdio"
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin:      func(r *http.Request) bool { return true },
}

// shuttingDown flips to true on SIGTERM. While set, the readiness probe fails
// (so the Service removes this pod from its endpoints) and new WS/HTTP sessions
// are rejected, so the pod stops taking work before it stops serving.
var shuttingDown atomic.Bool

// drainer tracks every active WS/Streamable-HTTP session so a graceful
// shutdown can close each one cleanly — a WebSocket close frame or an HTTP
// stream end — instead of letting process exit reset every socket. Clients then reconnect to the
// already-Ready surged-in replacement pod, so a rollout no longer kills
// in-flight proxied requests (the whole fleet shares one image and rolls on
// every server-code change; see .gitlab-ci.yml build:image:custom-server).
var drainer = newDrainRegistry()

// sharedWSChild is installed by main only when MCP_SHARED_CHILD=1. Keeping the
// default nil preserves the historical one-child-per-WebSocket behavior.
var sharedWSChild *sharedChildSupervisor

type sharedChild struct {
	cmd       *exec.Cmd
	transport *mcp.StdioTransport
	mux       *muxstdio.Transport
	done      chan struct{}
	closeOnce sync.Once
}

func (c *sharedChild) close() {
	c.closeOnce.Do(func() {
		_ = c.mux.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
	})
}

type sharedChildSupervisor struct {
	serverName string
	command    string
	draining   bool
	deliveries lifecycle.Drain

	mu             sync.Mutex
	initializeMu   sync.Mutex
	child          *sharedChild
	generation     uint64
	nextRequestID  uint64
	initializeResp *mcp.Message
	initialized    bool
	subscribers    map[string]chan *mcp.Message
}

func newSharedChildSupervisor(serverName, command string) *sharedChildSupervisor {
	return &sharedChildSupervisor{serverName: serverName, command: command, subscribers: make(map[string]chan *mcp.Message)}
}

func (s *sharedChildSupervisor) childForRequest() (*sharedChild, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child != nil {
		select {
		case <-s.child.done:
			s.child.close()
			s.child = nil
			s.initializeResp = nil
			s.initialized = false
		default:
			return s.child, s.generation, nil
		}
	}
	if s.draining {
		return nil, 0, fmt.Errorf("server draining")
	}
	child, err := startSharedChild(s.command)
	if err != nil {
		return nil, 0, err
	}
	s.generation++
	s.child = child
	fmt.Fprintf(os.Stderr, "custom-server shared child started server=%s pid=%d generation=%d\n", s.serverName, child.cmd.Process.Pid, s.generation)
	go s.forwardNotifications(child, s.generation)
	return child, s.generation, nil
}

func startSharedChild(command string) (*sharedChild, error) {
	cmdName, cmdArgs, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	// Deliberately not bound to any request context: the shared child must
	// outlive every WebSocket session that uses it. Lifetime is owned by
	// sharedChild.close (Kill + Wait), not by a ctx.
	cmd := exec.CommandContext(context.Background(), cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), "MCP_TRANSPORT=stdio")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe error: %w", err)
	}
	// Own the read end: exec.Cmd.Wait must not close stdout before the mux
	// consumes the child's final response on exit.
	stdout, childOut, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe error: %w", err)
	}
	cmd.Stdout = childOut
	if err := cmd.Start(); err != nil {
		_ = childOut.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start error: %w", err)
	}
	_ = childOut.Close()
	transport := mcp.NewStdioTransport(stdout, stdin)
	c := &sharedChild{cmd: cmd, transport: transport, done: make(chan struct{})}
	c.mux = muxstdio.New(transport)
	go func() { _ = cmd.Wait(); close(c.done) }()
	return c, nil
}

func cloneMessage(msg *mcp.Message) *mcp.Message {
	if msg == nil {
		return nil
	}
	copy := *msg
	copy.Params = append(json.RawMessage(nil), msg.Params...)
	copy.Result = append(json.RawMessage(nil), msg.Result...)
	return &copy
}

// beginDrain forwards SIGTERM while keeping the mux and client transports alive.
// Child exit alone is insufficient: response goroutines must finish writing too.
func (s *sharedChildSupervisor) beginDrain(ctx context.Context) {
	s.mu.Lock()
	s.draining = true
	child := s.child
	if child != nil {
		_ = child.cmd.Process.Signal(syscall.SIGTERM)
	}
	s.mu.Unlock()
	if child != nil {
		select {
		case <-child.done:
		case <-ctx.Done():
			return
		}
	}
	_, done := s.deliveries.Begin()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *sharedChildSupervisor) admitDelivery(msg *mcp.Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var params struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	if s.draining && (msg.Method == "initialize" || (msg.Method == "tools/call" && params.Name != "devbox_exec_poll")) {
		return false
	}
	return s.deliveries.Admit()
}

func drainingResponse(msg *mcp.Message) *mcp.Message {
	return &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: msg.ID,
		Error: &mcp.Error{Code: -32000, Message: "devbox is draining; retry on another server", Data: map[string]any{"retryable": true}}}
}

func (s *sharedChildSupervisor) call(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if msg.Method == "initialize" {
		s.initializeMu.Lock()
		defer s.initializeMu.Unlock()
	}
	s.mu.Lock()
	if msg.Method == "initialize" && s.initializeResp != nil {
		resp := cloneMessage(s.initializeResp)
		resp.ID = msg.ID
		s.mu.Unlock()
		return resp, nil
	}
	s.mu.Unlock()

	child, generation, err := s.childForRequest()
	if err != nil {
		return nil, err
	}
	originalID := msg.ID
	request := cloneMessage(msg)
	request.ID = fmt.Sprintf("ws-%d-%d", generation, atomic.AddUint64(&s.nextRequestID, 1))
	resp, err := child.mux.Call(ctx, request)
	if err != nil {
		// A WebSocket disconnect cancels only that caller. It must not tear down
		// the shared process or erase state needed by the reconnecting client.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.discard(child)
		}
		return nil, err
	}
	resp = cloneMessage(resp)
	resp.ID = originalID
	if msg.Method == "initialize" {
		s.mu.Lock()
		if s.child == child {
			s.initializeResp = cloneMessage(resp)
		}
		s.mu.Unlock()
	}
	return resp, nil
}

func (s *sharedChildSupervisor) notify(ctx context.Context, msg *mcp.Message) error {
	// initialized belongs to the single child MCP session; only the first
	// WebSocket client forwards it.
	if msg.Method == "notifications/initialized" {
		s.mu.Lock()
		if s.initialized {
			s.mu.Unlock()
			return nil
		}
		s.initialized = true
		s.mu.Unlock()
	}
	child, _, err := s.childForRequest()
	if err != nil {
		return err
	}
	if err := child.transport.Send(ctx, msg); err != nil {
		s.discard(child)
		return err
	}
	return nil
}

func (s *sharedChildSupervisor) discard(child *sharedChild) {
	s.mu.Lock()
	if s.child == child {
		s.child = nil
		s.initializeResp = nil
		s.initialized = false
	}
	s.mu.Unlock()
	child.close()
}

func (s *sharedChildSupervisor) subscribe(id string) (<-chan *mcp.Message, func()) {
	ch := make(chan *mcp.Message, 16)
	s.mu.Lock()
	s.subscribers[id] = ch
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.subscribers, id); s.mu.Unlock() }
}

func (s *sharedChildSupervisor) forwardNotifications(child *sharedChild, generation uint64) {
	for msg := range child.mux.NotificationCh() {
		s.mu.Lock()
		if s.child != child || s.generation != generation {
			s.mu.Unlock()
			return
		}
		for _, ch := range s.subscribers {
			select {
			case ch <- cloneMessage(msg):
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *sharedChildSupervisor) close() {
	s.mu.Lock()
	child := s.child
	s.child = nil
	s.initializeResp = nil
	s.initialized = false
	s.mu.Unlock()
	if child != nil {
		child.close()
	}
}

type wsMessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

func writeWS(mu *sync.Mutex, conn wsMessageWriter, messageType int, data []byte) error {
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteMessage(messageType, data)
}

// writeWSWithDeadline is writeWS for the drain path only: a peer that has
// stopped reading must not hold shutdown open, so this write carries a bounded
// deadline, cleared again under the same lock so ordinary writes stay
// deadline-free. Kept off writeWS itself — the per-write type assertion it
// needs cost the fleet write benchmark 11% (reliability gate, 2026-09-13).
func writeWSWithDeadline(mu *sync.Mutex, conn *websocket.Conn, messageType int, data []byte, d time.Duration) error {
	mu.Lock()
	defer mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(d))
	err := conn.WriteMessage(messageType, data)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// drainRegistry holds a close function per active connection so shutdown can
// tear them all down cleanly and concurrently.
type drainRegistry struct {
	mu    sync.Mutex
	conns map[string]func()
}

func newDrainRegistry() *drainRegistry {
	return &drainRegistry{conns: make(map[string]func())}
}

func (d *drainRegistry) Add(id string, closeFn func()) {
	d.mu.Lock()
	d.conns[id] = closeFn
	d.mu.Unlock()
}

func (d *drainRegistry) Remove(id string) {
	d.mu.Lock()
	delete(d.conns, id)
	d.mu.Unlock()
}

func (d *drainRegistry) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

// DrainAll invokes every registered close function concurrently and waits for
// them, so total drain time is bounded by the slowest single connection rather
// than their sum. Each closeFn signals a clean shutdown to its client and tears
// the connection down; handlers unregister themselves via Remove on return.
func (d *drainRegistry) DrainAll() {
	d.mu.Lock()
	fns := make([]func(), 0, len(d.conns))
	for _, fn := range d.conns {
		fns = append(fns, fn)
	}
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(f func()) {
			defer wg.Done()
			f()
		}(fn)
	}
	wg.Wait()
}

// envDuration reads a time.Duration from an environment variable, falling back
// to def when unset or unparseable.
func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Fprintf(os.Stderr, "invalid %s=%q; using default %s\n", key, v, def)
	}
	return def
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}

// legacyTransportRemovedHandler answers the retired MCP HTTP+SSE transport
// routes (GET /sse, POST /messages) with a 404 whose body names the
// replacements, so a straggler configured against the old endpoint sees why
// it fails instead of a bare not-found. The transport was removed once every
// mcpo consumer ran Streamable HTTP and a 45h soak showed zero /sse traffic
// (.loom/197 §4, backlog item bl-mcpo-sse-removal-endgame-20260829).
func legacyTransportRemovedHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "the MCP HTTP+SSE transport (/sse, /messages) was removed; use Streamable HTTP at POST /mcp or WebSocket at /ws", http.StatusNotFound)
}

// registerLegacyTransportRemoved mounts the explanatory 404 on the retired
// routes. Registered on the same mux as the live transports so the answer is
// deterministic regardless of the default mux's not-found behaviour.
func registerLegacyTransportRemoved(mux *http.ServeMux) {
	mux.HandleFunc("/sse", legacyTransportRemovedHandler)
	mux.HandleFunc("/messages", legacyTransportRemovedHandler)
}

func startMCPProcess(ctx context.Context, serverName, command string) (*exec.Cmd, *mcp.StdioTransport, func(), error) {
	cmdName, cmdArgs, err := splitCommand(command)
	if err != nil {
		return nil, nil, nil, err
	}

	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), "MCP_TRANSPORT=stdio")
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdin pipe error: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("start error: %w", err)
	}

	transport := mcp.NewStdioTransport(stdout, stdin)

	closeOnce := sync.Once{}
	closeAll := func() {
		closeOnce.Do(func() {
			_ = stdin.Close()
			_ = stdout.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
	}

	_ = serverName // reserved for future logging / tagging
	return cmd, transport, closeAll, nil
}

func main() {
	command := strings.TrimSpace(os.Getenv("MCP_SERVER_COMMAND"))
	if command == "" {
		fmt.Fprintln(os.Stderr, "MCP_SERVER_COMMAND is required")
		os.Exit(1)
	}

	wsPort := strings.TrimSpace(os.Getenv("MCP_WS_PORT"))
	if wsPort == "" {
		wsPort = "8080"
	}
	addr := wsPort
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}

	wsPath := strings.TrimSpace(os.Getenv("MCP_WS_PATH"))
	if wsPath == "" {
		wsPath = "/ws"
	}
	httpPath := strings.TrimSpace(os.Getenv("MCP_HTTP_PATH"))
	if httpPath == "" {
		httpPath = "/mcp"
	}

	serverName := strings.TrimSpace(os.Getenv("MCP_SERVER_NAME"))
	if serverName == "" {
		serverName = "custom-server"
	}
	if strings.TrimSpace(os.Getenv("MCP_SHARED_CHILD")) == "1" {
		sharedWSChild = newSharedChildSupervisor(serverName, command)
		defer sharedWSChild.close()
	}

	// Liveness stays green throughout drain (so Kubernetes does not kill the pod
	// mid-drain); readiness fails once shutting down so the Service deregisters
	// this pod from its endpoints.
	http.HandleFunc("/health", okHandler)
	http.HandleFunc("/ready", readyHandler)

	// The hub serves exactly two MCP transports: Streamable HTTP at httpPath
	// (POST /mcp) and WebSocket at wsPath (/ws, the spawn pods' and daemon's
	// path). The legacy HTTP+SSE transport (GET /sse + POST /messages) was
	// removed on 2026-09-12 after every mcpo consumer moved to Streamable HTTP
	// (.loom/197 §4); its routes answer with an explanatory 404.
	http.Handle(httpPath, newStreamableHTTPHandler(serverName, command, drainer))
	registerLegacyTransportRemoved(http.DefaultServeMux)

	http.HandleFunc(wsPath, func(w http.ResponseWriter, r *http.Request) {
		if sharedWSChild != nil {
			handleSharedWS(w, r, sharedWSChild)
			return
		}
		handleWS(w, r, serverName, command)
	})

	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "custom-server listening on %s (http=%s, ws=%s, server=%s)\n", addr, httpPath, wsPath, serverName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
	case err := <-listenErr:
		fmt.Fprintf(os.Stderr, "ListenAndServe: %v\n", err)
	}

	// Graceful drain. On SIGTERM: fail readiness (so the Service removes this
	// pod from its endpoints) and reject new sessions, pause briefly to let the
	// deregistration propagate to the gateway and in-flight requests settle,
	// then close every active WS/HTTP session cleanly so proxied clients
	// reconnect to the already-Ready surged-in replacement rather than seeing an
	// abrupt reset. Pairs with the deployment preStop hook and the
	// maxUnavailable:0/maxSurge:1 rollout strategy. Both waits are env-tunable
	// so ops can adjust without a rebuild.
	drainDelay := envDuration("MCP_SHUTDOWN_DRAIN", 3*time.Second)
	shutdownTimeout := envDuration("MCP_SHUTDOWN_TIMEOUT", 10*time.Second)

	shuttingDown.Store(true)
	fmt.Fprintf(os.Stderr, "custom-server draining: readiness failing, %d active session(s), settling for %s\n", drainer.Len(), drainDelay)
	if sharedWSChild != nil && serverName == "devbox" {
		timeout := envDuration("DEVBOX_DRAIN_TIMEOUT", 30*time.Minute)
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout+25*time.Second)
		sharedWSChild.beginDrain(ctx)
		cancel()
	} else {
		time.Sleep(drainDelay)
	}

	drainer.DrainAll()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// readyHandler reports NotReady (503) once the process is draining so the
// Service removes this pod from its endpoints before sessions are closed.
func readyHandler(w http.ResponseWriter, r *http.Request) {
	if shuttingDown.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func handleSharedWS(w http.ResponseWriter, r *http.Request, supervisor *sharedChildSupervisor) {
	if shuttingDown.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	var writeMu sync.Mutex
	ctx, cancel := context.WithCancel(r.Context())
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { cancel(); _ = conn.Close() }) }
	defer closeConn()

	wsID, err := newSessionID()
	if err != nil {
		wsID = fmt.Sprintf("ws-%d", time.Now().UnixNano())
	}
	notifications, unsubscribe := supervisor.subscribe(wsID)
	defer unsubscribe()
	drainer.Add(wsID, func() {
		_ = writeWS(&writeMu, conn, websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseServiceRestart, "server draining"))
		closeConn()
	})
	defer drainer.Remove(wsID)

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil })

	var wg sync.WaitGroup
	// calls tracks request goroutines dispatched off the read loop.
	var calls sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if writeWS(&writeMu, conn, websocket.PingMessage, nil) != nil {
					closeConn()
					return
				}
			case msg := <-notifications:
				b, err := json.Marshal(msg)
				if err == nil && writeWS(&writeMu, conn, websocket.TextMessage, b) != nil {
					closeConn()
					return
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				closeConn()
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			var msg mcp.Message
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.JSONRPC == "" {
				msg.JSONRPC = mcp.JSONRPCVersion
			}
			if msg.IsNotification() {
				if supervisor.notify(ctx, &msg) != nil {
					closeConn()
					return
				}
				continue
			}
			// Dispatch the call off the read loop. A devbox quality-gate call
			// runs for minutes, and WebSocket control frames — the client's
			// keepalive pings, our own pongs — are only processed inside
			// ReadMessage. Calling the supervisor inline here starved them:
			// every call longer than the client's pong wait died with
			// "websocket: close 1006 (abnormal closure)" at exactly 60s, and
			// no Mills tests-stage attempt passed for the six hours the first
			// shared-child image was live (2026-09-06, 04:14–10:50Z). The
			// per-session handler never had this problem because it forwards
			// to the child and reads responses on a separate goroutine.
			if !supervisor.admitDelivery(&msg) {
				b, _ := json.Marshal(drainingResponse(&msg))
				if writeWSWithDeadline(&writeMu, conn, websocket.TextMessage, b, 5*time.Second) != nil {
					closeConn()
					return
				}
				continue
			}
			calls.Add(1)
			go func(msg mcp.Message) {
				defer calls.Done()
				defer supervisor.deliveries.Release()
				resp, err := supervisor.call(ctx, &msg)
				if err != nil {
					// A cancelled ctx means this connection is already closing.
					if ctx.Err() == nil {
						closeConn()
					}
					return
				}
				b, err := json.Marshal(resp)
				if err == nil && writeWS(&writeMu, conn, websocket.TextMessage, b) != nil {
					closeConn()
				}
			}(msg)
		}
	}()
	wg.Wait()
	// closeConn cancelled ctx, so in-flight calls return promptly; wait for
	// them so no goroutine outlives the handler.
	calls.Wait()
}

func handleWS(w http.ResponseWriter, r *http.Request, serverName, command string) {
	if shuttingDown.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var writeMu sync.Mutex

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmdName, cmdArgs, err := splitCommand(command)
	if err != nil {
		_ = writeWS(&writeMu, conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, err.Error()))
		return
	}

	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), "MCP_TRANSPORT=stdio")
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = writeWS(&writeMu, conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stdin pipe error"))
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = writeWS(&writeMu, conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stdout pipe error"))
		return
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = writeWS(&writeMu, conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "start error"))
		return
	}

	transport := mcp.NewStdioTransport(stdout, stdin)

	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			cancel()
			_ = conn.Close()
			_ = stdin.Close()
			_ = stdout.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
	}
	defer closeAll()

	// Register for graceful drain: send a WebSocket close frame with code 1012
	// (service restart) so the client reconnects to the replacement pod, then
	// tear the connection down.
	wsID, idErr := newSessionID()
	if idErr != nil {
		wsID = fmt.Sprintf("ws-%d", time.Now().UnixNano())
	}
	drainer.Add(wsID, func() {
		_ = writeWS(&writeMu, conn, websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseServiceRestart, "server draining"))
		closeAll()
	})
	defer drainer.Remove(wsID)

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := writeWS(&writeMu, conn, websocket.PingMessage, nil); err != nil {
					closeAll()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			msg, err := transport.Recv(ctx)
			if err != nil {
				closeAll()
				return
			}
			b, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			err = writeWS(&writeMu, conn, websocket.TextMessage, b)
			if err != nil {
				closeAll()
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				closeAll()
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			var msg mcp.Message
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.JSONRPC == "" {
				msg.JSONRPC = mcp.JSONRPCVersion
			}
			if err := transport.Send(ctx, &msg); err != nil {
				closeAll()
				return
			}
		}
	}()

	wg.Wait()
}

func splitCommand(s string) (string, []string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("empty MCP_SERVER_COMMAND")
	}
	return fields[0], fields[1:], nil
}
