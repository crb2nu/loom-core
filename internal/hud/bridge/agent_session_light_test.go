package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sessionListCaller is a Caller that serves agent_session_list from a script:
// each call records the timeout it was given, optionally sleeps, and returns
// either the canned session set or the next scripted error.
type sessionListCaller struct {
	mu       sync.Mutex
	calls    []sessionListCall
	delay    time.Duration
	errs     []error // consumed in order; nil entries mean success
	sessions int
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

type sessionListCall struct {
	args    map[string]any
	timeout time.Duration
}

func (c *sessionListCaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (c *sessionListCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (c *sessionListCaller) CallTool(name string, args map[string]any) (json.RawMessage, error) {
	return c.CallToolWithTimeout(name, args, 0)
}
func (c *sessionListCaller) CircuitOpen() bool { return false }
func (c *sessionListCaller) Close() error      { return nil }

func (c *sessionListCaller) CallToolWithTimeout(name string, args map[string]any, timeout time.Duration) (json.RawMessage, error) {
	if name != "agent_context__agent_session_list" {
		return nil, fmt.Errorf("unexpected tool %s", name)
	}
	n := c.inFlight.Add(1)
	for {
		seen := c.maxSeen.Load()
		if n <= seen || c.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	defer c.inFlight.Add(-1)

	c.mu.Lock()
	c.calls = append(c.calls, sessionListCall{args: args, timeout: timeout})
	var err error
	if len(c.errs) > 0 {
		err = c.errs[0]
		c.errs = c.errs[1:]
	}
	delay := c.delay
	sessions := c.sessions
	c.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	rows := make([]string, 0, sessions)
	for i := 0; i < sessions; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"s%d","agent_id":"agent-%d","status":"active"}`, i, i))
	}
	payload := fmt.Sprintf(`{"ok":true,"sessions":[%s],"count":%d}`, joinStrings(rows, ","), sessions)
	return mcpTextResult(jsonQuote(payload)), nil
}

func (c *sessionListCaller) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *sessionListCaller) call(i int) sessionListCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[i]
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// jsonQuote JSON-quotes a payload so it can sit inside the MCP text envelope.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestLightSessionList_CoalescesConcurrentCallers(t *testing.T) {
	caller := &sessionListCaller{delay: 40 * time.Millisecond, sessions: 2}
	b := NewAgentBridge(caller)

	const callers = 6
	var wg sync.WaitGroup
	results := make([][]SessionInfo, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = b.FleetSessions()
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if len(results[i]) != 2 {
			t.Fatalf("caller %d got %d sessions, want 2", i, len(results[i]))
		}
	}
	if got := caller.callCount(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (singleflight)", got)
	}
	if got := caller.maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent upstream calls = %d, want 1", got)
	}
}

func TestLightSessionList_CachedWithinTTLThenRefetched(t *testing.T) {
	caller := &sessionListCaller{sessions: 1}
	b := NewAgentBridge(caller)
	b.sessionListTTL = 30 * time.Millisecond

	for i := 0; i < 3; i++ {
		if _, err := b.ActiveSessions(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := caller.callCount(); got != 1 {
		t.Fatalf("upstream calls inside TTL = %d, want 1", got)
	}

	time.Sleep(45 * time.Millisecond)
	if _, err := b.ActiveSessions(); err != nil {
		t.Fatal(err)
	}
	if got := caller.callCount(); got != 2 {
		t.Fatalf("upstream calls after TTL = %d, want 2", got)
	}

	b.InvalidateSessionLists()
	if _, err := b.ActiveSessions(); err != nil {
		t.Fatal(err)
	}
	if got := caller.callCount(); got != 3 {
		t.Fatalf("upstream calls after invalidate = %d, want 3", got)
	}
}

func TestLightSessionList_ProjectionsAreCachedSeparately(t *testing.T) {
	caller := &sessionListCaller{sessions: 1}
	b := NewAgentBridge(caller)

	if _, err := b.FleetSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ActiveSessions(); err != nil {
		t.Fatal(err)
	}
	if got := caller.callCount(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per projection)", got)
	}
	if _, ok := caller.call(0).args["status"]; ok {
		t.Fatalf("fleet projection must not filter by status: %v", caller.call(0).args)
	}
	if status := caller.call(1).args["status"]; status != "active" {
		t.Fatalf("active projection status = %v, want active", status)
	}
	for i := 0; i < 2; i++ {
		if light := caller.call(i).args["light"]; light != true {
			t.Fatalf("call %d light = %v, want true", i, light)
		}
		if got := caller.call(i).timeout; got != sessionListBudgetBase {
			t.Fatalf("call %d budget = %v, want %v", i, got, sessionListBudgetBase)
		}
	}
}

// The fleet monitor appends the active set to the list it receives. A shared
// backing array would let that append leak into the cached slice.
func TestLightSessionList_ReturnsIndependentCopies(t *testing.T) {
	caller := &sessionListCaller{sessions: 2}
	b := NewAgentBridge(caller)

	first, err := b.FleetSessions()
	if err != nil {
		t.Fatal(err)
	}
	first = append(first, SessionInfo{ID: "injected"})
	first[0].ID = "mutated"

	second, err := b.FleetSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("second fetch has %d sessions, want 2 (append leaked into cache)", len(second))
	}
	if second[0].ID != "s0" {
		t.Fatalf("second[0].ID = %q, want s0 (mutation leaked into cache)", second[0].ID)
	}
}

func TestLightSessionList_TimeoutRaisesNextBudget(t *testing.T) {
	timeout := errors.New("daemon error (-32603): tools/call timeout during recv after 3s (recoverable)")
	caller := &sessionListCaller{sessions: 1, errs: []error{timeout}}
	b := NewAgentBridge(caller)
	b.sessionListTTL = time.Millisecond

	if _, err := b.FleetSessions(); err == nil {
		t.Fatal("first call should surface the timeout")
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := b.FleetSessions(); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := caller.call(0).timeout; got != 3*time.Second {
		t.Fatalf("first budget = %v, want 3s", got)
	}
	if got := caller.call(1).timeout; got != 6*time.Second {
		t.Fatalf("budget after a timeout = %v, want 6s", got)
	}
}

// A tool error from the store (not a timeout) must not touch the budget, and
// errors must not be cached.
func TestLightSessionList_StoreErrorIsNotCachedAndKeepsBudget(t *testing.T) {
	storeErr := errors.New("list sessions: qdrant HTTP 408")
	caller := &sessionListCaller{sessions: 1, errs: []error{storeErr}}
	b := NewAgentBridge(caller)

	if _, err := b.ActiveSessions(); err == nil {
		t.Fatal("store error should surface")
	}
	if _, err := b.ActiveSessions(); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := caller.callCount(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (errors are not cached)", got)
	}
	if got := caller.call(1).timeout; got != sessionListBudgetBase {
		t.Fatalf("budget after a store error = %v, want unchanged %v", got, sessionListBudgetBase)
	}
}

func TestSessionListBudget_EscalatesCapsAndDecays(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := &sessionListBudget{now: func() time.Time { return now }}
	timeout := errors.New("tools/call timeout during recv after 3s")

	if got := b.current(); got != 3*time.Second {
		t.Fatalf("initial budget = %v, want 3s", got)
	}

	if next, changed := b.observe(timeout); !changed || next != 6*time.Second {
		t.Fatalf("after 1st timeout: %v changed=%v, want 6s changed=true", next, changed)
	}
	if next, changed := b.observe(timeout); !changed || next != 12*time.Second {
		t.Fatalf("after 2nd timeout: %v changed=%v, want 12s changed=true", next, changed)
	}
	if next, changed := b.observe(timeout); changed || next != 12*time.Second {
		t.Fatalf("at cap: %v changed=%v, want 12s changed=false", next, changed)
	}

	// A success inside the quiet window keeps the raised budget.
	now = now.Add(sessionListBudgetDecay / 2)
	if next, changed := b.observe(nil); changed || next != 12*time.Second {
		t.Fatalf("success inside quiet window: %v changed=%v, want 12s unchanged", next, changed)
	}
	// A timeout restarts the quiet window even at the cap.
	if _, changed := b.observe(timeout); changed {
		t.Fatal("timeout at cap must not report a change")
	}
	now = now.Add(sessionListBudgetDecay - time.Second)
	if _, changed := b.observe(nil); changed {
		t.Fatal("decay fired before the quiet window elapsed after the last timeout")
	}
	now = now.Add(time.Second)
	if next, changed := b.observe(nil); !changed || next != 6*time.Second {
		t.Fatalf("after quiet window: %v changed=%v, want 6s changed=true", next, changed)
	}
	// The next step down needs another full quiet window.
	if _, changed := b.observe(nil); changed {
		t.Fatal("decayed twice in one observation")
	}
	now = now.Add(sessionListBudgetDecay)
	if next, changed := b.observe(nil); !changed || next != 3*time.Second {
		t.Fatalf("second decay: %v changed=%v, want 3s changed=true", next, changed)
	}
	if next, changed := b.observe(nil); changed || next != 3*time.Second {
		t.Fatalf("at floor: %v changed=%v, want 3s unchanged", next, changed)
	}
}

func TestIsSessionListTimeout(t *testing.T) {
	cases := map[string]bool{
		"agent tool agent_session_list: daemon error (-32603): tools/call timeout during recv after 3s": true,
		"server unavailable: local: dial agent_context: context deadline exceeded":                      true,
		"agent tool agent_session_list failed: list sessions: qdrant HTTP 408:":                         false,
		"circuit breaker open": false,
	}
	for msg, want := range cases {
		if got := isSessionListTimeout(errors.New(msg)); got != want {
			t.Errorf("isSessionListTimeout(%q) = %v, want %v", msg, got, want)
		}
	}
	if isSessionListTimeout(nil) {
		t.Error("nil must not be a timeout")
	}
}
