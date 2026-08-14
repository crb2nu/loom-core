package transport_test

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hubproto"
	"github.com/crb2nu/loom/pkg/transport"
)

func TestLivenessRequiresMatchingPongAndGatesMisses(t *testing.T) {
	var l transport.Liveness
	first, ok := l.Ping(2)
	if !ok || first == "" {
		t.Fatal("first ping should be healthy")
	}
	if l.Pong("stale") {
		t.Fatal("mismatched pong was accepted")
	}
	second, ok := l.Ping(2)
	if !ok || second == first {
		t.Fatal("first miss should allow a new ping")
	}
	if l.Pong(first) {
		t.Fatal("stale pong was accepted")
	}
	if _, ok := l.Ping(2); ok {
		t.Fatal("second miss should mark connection unhealthy")
	}
	if !l.Pong(second) {
		t.Fatal("current pong should be accepted")
	}
	if _, ok := l.Ping(2); !ok {
		t.Fatal("pong should reset missed count")
	}
}

func TestBackoffCappedJitteredAndDeterministic(t *testing.T) {
	b := transport.Backoff{Initial: time.Second, Max: 4 * time.Second, Jitter: .25, Rand: func() float64 { return 0 }}
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 3 * time.Second}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Fatalf("Delay(%d)=%s want %s", i, got, w)
		}
	}
	b.Rand = func() float64 { return .999999 }
	if got := b.Delay(20); got != 4*time.Second {
		t.Fatalf("upper jitter delay %s exceeds 4s cap", got)
	}
}

func TestNewBackoffSeedIsRepeatable(t *testing.T) {
	c := transport.DefaultKeepaliveConfig()
	a := transport.NewBackoff(c, rand.NewSource(7))
	b := transport.NewBackoff(c, rand.NewSource(7))
	for i := 0; i < 5; i++ {
		if a.Delay(i) != b.Delay(i) {
			t.Fatal("seeded jitter is not deterministic")
		}
	}
}

func TestKeepaliveConfigValidation(t *testing.T) {
	c := transport.DefaultKeepaliveConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.MaxMissedPongs = 0
	if err := c.Validate(); err == nil {
		t.Fatal("zero missed-pong threshold accepted")
	}
}

func TestLivenessControlEnvelopeValidation(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.FixedZone("test", 3600))
	ping := hubproto.NewPing("ping-1", "hud", now)
	pong, err := hubproto.NewPong(ping, "hub", now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := hubproto.ParsePong(pong); err != nil || got != "ping-1" {
		t.Fatalf("ParsePong() = %q, %v", got, err)
	}
	if !pong.Timestamp.Equal(now.UTC()) || pong.Timestamp.Location() != time.UTC {
		t.Fatalf("pong timestamp = %v, want UTC", pong.Timestamp)
	}

	invalid := []*hubproto.Envelope{
		nil,
		{Domain: hubproto.DomainMCP, Method: hubproto.MethodPong},
		{Domain: hubproto.DomainControl, Method: hubproto.MethodPong, Payload: json.RawMessage(`{`)},
		{Domain: hubproto.DomainControl, Method: hubproto.MethodPong, Payload: json.RawMessage(`{}`)},
		{Domain: hubproto.DomainControl, Method: hubproto.MethodPong, RequestID: "one", Payload: json.RawMessage(`{"ping_id":"two"}`)},
	}
	for i, env := range invalid {
		if _, err := hubproto.ParsePong(env); err == nil {
			t.Fatalf("invalid envelope %d was accepted", i)
		}
	}
}
