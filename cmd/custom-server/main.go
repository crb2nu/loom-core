package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin:      func(r *http.Request) bool { return true },
}

// shuttingDown flips to true on SIGTERM. While set, the readiness probe fails
// (so the Service removes this pod from its endpoints) and new WS/SSE sessions
// are rejected, so the pod stops taking work before it stops serving.
var shuttingDown atomic.Bool

// drainer tracks every active WS/SSE session so a graceful shutdown can close
// each one cleanly — a WebSocket close frame or an SSE stream-end — instead of
// letting process exit reset every socket. Clients then reconnect to the
// already-Ready surged-in replacement pod, so a rollout no longer kills
// in-flight proxied requests (the whole fleet shares one image and rolls on
// every server-code change; see .gitlab-ci.yml build:image:custom-server).
var drainer = newDrainRegistry()

type wsMessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

func writeWS(mu *sync.Mutex, conn wsMessageWriter, messageType int, data []byte) error {
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteMessage(messageType, data)
}

type sseSession struct {
	id        string
	createdAt time.Time

	cancel context.CancelFunc

	cmd       *exec.Cmd
	transport *mcp.StdioTransport

	sendMu sync.Mutex

	closeOnce sync.Once
	done      chan struct{}
}

func (s *sseSession) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.transport != nil {
			_ = s.transport.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		close(s.done)
	})
}

func (s *sseSession) Send(ctx context.Context, msg *mcp.Message) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if msg.JSONRPC == "" {
		msg.JSONRPC = mcp.JSONRPCVersion
	}
	return s.transport.Send(ctx, msg)
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*sseSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*sseSession)}
}

func (st *sessionStore) Get(id string) *sseSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessions[id]
}

func (st *sessionStore) Put(s *sseSession) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[s.id] = s
}

func (st *sessionStore) Delete(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
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

func writeSSE(w http.ResponseWriter, event string, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	// data may contain newlines; each line must be prefixed with "data: "
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEComment(w http.ResponseWriter, comment string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", comment); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
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

	serverName := strings.TrimSpace(os.Getenv("MCP_SERVER_NAME"))
	if serverName == "" {
		serverName = "custom-server"
	}

	sessions := newSessionStore()

	// Liveness stays green throughout drain (so Kubernetes does not kill the pod
	// mid-drain); readiness fails once shutting down so the Service deregisters
	// this pod from its endpoints.
	http.HandleFunc("/health", okHandler)
	http.HandleFunc("/ready", readyHandler)

	// SSE transport (MCP SSE spec):
	// - client connects to GET /sse (text/event-stream)
	// - server emits an "endpoint" event containing the POST URL to send messages
	// - client POSTs JSON-RPC messages to /messages?session_id=<hex>
	http.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if shuttingDown.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}

		sessionID, err := newSessionID()
		if err != nil {
			http.Error(w, "failed to generate session id", http.StatusInternalServerError)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())

		cmd, transport, closeProc, err := startMCPProcess(ctx, serverName, command)
		if err != nil {
			cancel()
			http.Error(w, "failed to start mcp server", http.StatusInternalServerError)
			return
		}

		sess := &sseSession{
			id:        sessionID,
			createdAt: time.Now().UTC(),
			cancel:    cancel,
			cmd:       cmd,
			transport: transport,
			done:      make(chan struct{}),
		}

		sessions.Put(sess)
		// Register for graceful drain: closing the session cancels its context
		// and ends the SSE stream, so the client sees a clean EOF and reconnects.
		drainer.Add(sessionID, sess.Close)
		defer func() {
			drainer.Remove(sessionID)
			sessions.Delete(sessionID)
			sess.Close()
			closeProc()
		}()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		} else {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// Build a relative endpoint for the client to POST messages.
		// Keep it consistent with the Python MCP SSE server transport.
		postPath := "/messages"
		q := url.Values{}
		q.Set("session_id", sessionID)
		postURI := postPath + "?" + q.Encode()

		if err := writeSSE(w, "endpoint", postURI); err != nil {
			return
		}

		backendCh := make(chan *mcp.Message, 8)
		go func() {
			defer close(backendCh)
			for {
				msg, err := transport.Recv(ctx)
				if err != nil {
					return
				}
				select {
				case backendCh <- msg:
				case <-ctx.Done():
					return
				}
			}
		}()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.done:
				return
			case <-keepalive.C:
				_ = writeSSEComment(w, "ping")
			case msg, ok := <-backendCh:
				if !ok {
					return
				}
				b, err := json.Marshal(msg)
				if err != nil {
					continue
				}
				if err := writeSSE(w, "message", string(b)); err != nil {
					return
				}
			}
		}
	})

	http.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}

		sess := sessions.Get(sessionID)
		if sess == nil {
			http.Error(w, "unknown session_id", http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		var msg mcp.Message
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if msg.JSONRPC == "" {
			msg.JSONRPC = mcp.JSONRPCVersion
		}

		if err := sess.Send(r.Context(), &msg); err != nil {
			http.Error(w, "send failed", http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("OK"))
	})

	http.HandleFunc(wsPath, func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, serverName, command)
	})

	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "custom-server listening on %s (ws=%s, server=%s)\n", addr, wsPath, serverName)
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
	// then close every active WS/SSE session cleanly so proxied clients
	// reconnect to the already-Ready surged-in replacement rather than seeing an
	// abrupt reset. Pairs with the deployment preStop hook and the
	// maxUnavailable:0/maxSurge:1 rollout strategy. Both waits are env-tunable
	// so ops can adjust without a rebuild.
	drainDelay := envDuration("MCP_SHUTDOWN_DRAIN", 3*time.Second)
	shutdownTimeout := envDuration("MCP_SHUTDOWN_TIMEOUT", 10*time.Second)

	shuttingDown.Store(true)
	fmt.Fprintf(os.Stderr, "custom-server draining: readiness failing, %d active session(s), settling for %s\n", drainer.Len(), drainDelay)
	time.Sleep(drainDelay)

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
