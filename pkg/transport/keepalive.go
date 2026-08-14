// Package transport contains transport-independent connection policies.
package transport

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// KeepaliveConfig controls application-level ping/pong liveness and reconnects.
type KeepaliveConfig struct {
	PingInterval   time.Duration
	MaxMissedPongs int
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	Jitter         float64
}

// DefaultKeepaliveConfig returns conservative defaults for long-lived sockets.
func DefaultKeepaliveConfig() KeepaliveConfig {
	return KeepaliveConfig{PingInterval: 30 * time.Second, MaxMissedPongs: 2, BackoffInitial: time.Second, BackoffMax: 30 * time.Second, Jitter: .2}
}

// Validate rejects configurations which could create busy loops or invalid jitter.
func (c KeepaliveConfig) Validate() error {
	if c.PingInterval <= 0 {
		return fmt.Errorf("ping interval must be positive")
	}
	if c.MaxMissedPongs <= 0 {
		return fmt.Errorf("max missed pongs must be positive")
	}
	if c.BackoffInitial <= 0 {
		return fmt.Errorf("initial backoff must be positive")
	}
	if c.BackoffMax < c.BackoffInitial {
		return fmt.Errorf("maximum backoff must be at least initial backoff")
	}
	if c.Jitter < 0 || c.Jitter > 1 {
		return fmt.Errorf("jitter must be between zero and one")
	}
	return nil
}

// Liveness tracks one outstanding application ping. It is safe for concurrent use.
type Liveness struct {
	mu          sync.Mutex
	next        uint64
	outstanding string
	missed      int
}

// Ping records a new ping. healthy is false once the configured miss threshold
// has been reached; callers should close the stale connection in that case.
func (l *Liveness) Ping(maxMissed int) (id string, healthy bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.outstanding != "" {
		l.missed++
	}
	if l.missed >= maxMissed {
		return "", false
	}
	l.next++
	l.outstanding = fmt.Sprintf("%d", l.next)
	return l.outstanding, true
}

// Pong acknowledges only the current ping. Stale or mismatched pongs cannot
// make a dead connection appear healthy.
func (l *Liveness) Pong(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == "" || id != l.outstanding {
		return false
	}
	l.outstanding = ""
	l.missed = 0
	return true
}

// Reset clears connection-local liveness state while retaining monotonic IDs.
func (l *Liveness) Reset() { l.mu.Lock(); l.outstanding = ""; l.missed = 0; l.mu.Unlock() }

// Backoff computes capped exponential reconnect delays with symmetric jitter.
// The random callback must return a value in [0,1); nil disables jitter.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Jitter  float64
	Rand    func() float64
}

// Delay returns the delay for a zero-based failed reconnect attempt.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := float64(b.Initial)
	for i := 0; i < attempt && d < float64(b.Max); i++ {
		d = math.Min(d*2, float64(b.Max))
	}
	if d > float64(b.Max) {
		d = float64(b.Max)
	}
	if b.Jitter > 0 && b.Rand != nil {
		d *= 1 - b.Jitter + 2*b.Jitter*b.Rand()
	}
	if d < 0 {
		return 0
	}
	if d > float64(b.Max) {
		d = float64(b.Max)
	}
	return time.Duration(d)
}

// NewBackoff creates a concurrency-safe jitter source.
func NewBackoff(c KeepaliveConfig, source rand.Source) Backoff {
	var mu sync.Mutex
	if source == nil {
		source = rand.NewSource(time.Now().UnixNano())
	}
	r := rand.New(source) // #nosec G404 -- jitter is deliberately non-cryptographic
	return Backoff{Initial: c.BackoffInitial, Max: c.BackoffMax, Jitter: c.Jitter, Rand: func() float64 { mu.Lock(); defer mu.Unlock(); return r.Float64() }}
}
