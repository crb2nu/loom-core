package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

const (
	mcpProtocolVersionHeader = "MCP-Protocol-Version"
	mcpSessionIDHeader       = "Mcp-Session-Id"
	maxStreamableRequestBody = 10 << 20
)

// streamableHTTPHandler implements Streamable HTTP's JSON response mode. The
// SDK's StreamableHTTPServer cannot wrap a stateful child stdio process because
// its callback is message-scoped and does not expose session lifecycle hooks.
// Keeping the process here lets initialize and later calls share one child and
// lets the existing drain registry close it during rollout.
type streamableHTTPHandler struct {
	serverName string
	command    string
	drainer    *drainRegistry

	mu       sync.Mutex
	sessions map[string]*streamableHTTPSession
}

type streamableHTTPSession struct {
	id        string
	transport *mcp.StdioTransport
	closeProc func()
	shared    *sharedChildSupervisor
	mu        sync.Mutex
	closeOnce sync.Once
}

func newStreamableHTTPHandler(serverName, command string, drainer *drainRegistry) http.Handler {
	return &streamableHTTPHandler{
		serverName: serverName,
		command:    command,
		drainer:    drainer,
		sessions:   make(map[string]*streamableHTTPSession),
	}
}

func (h *streamableHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	version := r.Header.Get(mcpProtocolVersionHeader)
	if !mcp.IsSupportedProtocolVersion(version) {
		version = mcp.ProtocolVersion20250618
	}
	w.Header().Set(mcpProtocolVersionHeader, version)

	// An established devbox session keeps its shared child reachable through
	// the drain window so in-flight quality gates can deliver their result.
	keepServingDuringDrain := h.serverName == "devbox" && sharedWSChild != nil && r.Header.Get(mcpSessionIDHeader) != ""
	if shuttingDown.Load() && !keepServingDuringDrain {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.handlePOST(w, r)
	case http.MethodDelete:
		h.handleDELETE(w, r)
	case http.MethodOptions:
		w.Header().Set("Allow", "POST, DELETE, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *streamableHTTPHandler) handlePOST(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" && ct != "application/json; charset=utf-8" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	var msg mcp.Message
	r.Body = http.MaxBytesReader(w, r.Body, maxStreamableRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if msg.JSONRPC == "" {
		msg.JSONRPC = mcp.JSONRPCVersion
	}

	var sess *streamableHTTPSession
	if msg.Method == "initialize" {
		if r.Header.Get(mcpSessionIDHeader) != "" {
			http.Error(w, "initialize must not include Mcp-Session-Id", http.StatusBadRequest)
			return
		}
		var err error
		sess, err = h.startSession()
		if err != nil {
			http.Error(w, "failed to start mcp server", http.StatusBadGateway)
			return
		}
		// Register the child before waiting for initialize so DrainAll cannot
		// snapshot the registry while a newly spawned process is untracked.
		h.put(sess)
		if shuttingDown.Load() {
			h.removeAndClose(sess.id)
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
	} else {
		sess = h.get(r.Header.Get(mcpSessionIDHeader))
		if sess == nil {
			http.Error(w, "Mcp-Session-Id header required or session not found", http.StatusBadRequest)
			return
		}
	}

	if sess.shared != nil && !msg.IsNotification() {
		if !sess.shared.admitDelivery(&msg) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(drainingResponse(&msg))
			return
		}
		defer sess.shared.deliveries.Release()
	}
	resp, err := sess.roundTrip(r.Context(), &msg)
	if err != nil {
		if msg.Method == "initialize" {
			h.removeAndClose(sess.id)
		}
		http.Error(w, "child mcp request failed", http.StatusBadGateway)
		return
	}
	if msg.Method == "initialize" {
		w.Header().Set(mcpSessionIDHeader, sess.id)
		var result mcp.InitializeResult
		if resp != nil && json.Unmarshal(resp.Result, &result) == nil && mcp.IsSupportedProtocolVersion(result.ProtocolVersion) {
			w.Header().Set(mcpProtocolVersionHeader, result.ProtocolVersion)
		}
	}

	if msg.IsNotification() || resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	_ = http.NewResponseController(w).Flush()
}

func (h *streamableHTTPHandler) handleDELETE(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(mcpSessionIDHeader)
	if id == "" {
		http.Error(w, "Mcp-Session-Id header required", http.StatusBadRequest)
		return
	}
	if !h.removeAndClose(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *streamableHTTPHandler) startSession() (*streamableHTTPSession, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	if h.serverName == "devbox" && sharedWSChild != nil {
		return &streamableHTTPSession{id: "http-" + id, shared: sharedWSChild, closeProc: func() {}}, nil
	}
	// The child must outlive the request that initialized it. Its explicit
	// close path is registered with the shared graceful drainer.
	_, transport, closeProc, err := startMCPProcess(context.Background(), h.serverName, h.command)
	if err != nil {
		return nil, err
	}
	return &streamableHTTPSession{id: "http-" + id, transport: transport, closeProc: closeProc}, nil
}

func (s *streamableHTTPSession) roundTrip(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if s.shared != nil {
		if msg.IsNotification() {
			return nil, s.shared.notify(ctx, msg)
		}
		return s.shared.call(ctx, msg)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.transport.Send(ctx, msg); err != nil {
		return nil, err
	}
	if msg.IsNotification() {
		return nil, nil
	}
	for {
		resp, err := s.transport.Recv(ctx)
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(resp.ID, msg.ID) {
			return resp, nil
		}
	}
}

func (s *streamableHTTPSession) close() {
	s.closeOnce.Do(s.closeProc)
}

func (h *streamableHTTPHandler) get(id string) *streamableHTTPSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *streamableHTTPHandler) put(sess *streamableHTTPSession) {
	h.mu.Lock()
	h.sessions[sess.id] = sess
	h.mu.Unlock()
	h.drainer.Add(sess.id, func() { h.removeAndClose(sess.id) })
}

func (h *streamableHTTPHandler) removeAndClose(id string) bool {
	h.mu.Lock()
	sess, ok := h.sessions[id]
	if ok {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	h.drainer.Remove(id)
	sess.close()
	return true
}
