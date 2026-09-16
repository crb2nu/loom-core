package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

// VendorErrorKind is a bounded classification shared by metered vendor callers.
type VendorErrorKind string

const (
	VendorBilling        VendorErrorKind = "billing"
	VendorAuth           VendorErrorKind = "auth"
	VendorRateLimit      VendorErrorKind = "rate_limit"
	VendorOverloaded     VendorErrorKind = "overloaded"
	VendorInvalidRequest VendorErrorKind = "invalid_request"
	VendorRefusal        VendorErrorKind = "refusal"
	VendorTransport      VendorErrorKind = "transport"
	VendorUnknown        VendorErrorKind = "unknown"
)

// VendorError preserves the vendor response metadata and the original cause.
type VendorError struct {
	Vendor    string
	Status    int
	Code      string
	Kind      VendorErrorKind
	RequestID string
	Retryable bool
	Err       error
}

func (e *VendorError) Error() string { return fmt.Sprintf("%s %s: %v", e.Vendor, e.Kind, e.Err) }
func (e *VendorError) Unwrap() error { return e.Err }

// AsVendorError finds a typed vendor failure through any wrapping layers.
func AsVendorError(err error) (*VendorError, bool) {
	var e *VendorError
	ok := errors.As(err, &e)
	return e, ok && e != nil
}

// IsVendorBilling reports whether the failure is a terminal billing rejection.
func IsVendorBilling(err error) bool {
	e, ok := AsVendorError(err)
	return ok && e.Kind == VendorBilling
}

func classifyVendorError(vendor string, err error) *VendorError {
	if e, ok := AsVendorError(err); ok {
		return e
	}
	e := &VendorError{Vendor: vendor, Kind: VendorUnknown, Err: err}
	message := ""
	var ae *anthropic.Error
	var oe *openairesponses.APIError
	switch {
	case vendor == "anthropic" && errors.As(err, &ae):
		e.Status, e.Code, e.RequestID = ae.StatusCode, string(ae.Type()), ae.RequestID
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ae.RawJSON()), &body) == nil {
			message = body.Error.Message
		}
	case (vendor == "openai" || vendor == "openrouter") && errors.As(err, &oe):
		e.Status, e.Code, e.RequestID, message = oe.Status, oe.Code, oe.RequestID, oe.Message
	}
	switch {
	case vendor == "anthropic" && e.Status == 400 && strings.Contains(strings.ToLower(message), "credit balance"):
		e.Kind = VendorBilling
	case vendor == "openai" && e.Status == 429 && e.Code == "insufficient_quota":
		e.Kind = VendorBilling
	case vendor == "openrouter" && e.Status == 402:
		e.Kind = VendorBilling
	case e.Status == 401 || e.Status == 403:
		e.Kind = VendorAuth
	case e.Status == 429:
		e.Kind, e.Retryable = VendorRateLimit, true
	case e.Status >= 500 && e.Status <= 599:
		e.Kind, e.Retryable = VendorOverloaded, true
	case e.Status == 400:
		e.Kind = VendorInvalidRequest
	default:
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e.Kind = VendorTransport
			e.Retryable = !errors.Is(err, context.Canceled)
		}
	}
	return e
}

// ErrVendorBreakerOpen identifies a local rejection with no vendor request.
var ErrVendorBreakerOpen = errors.New("vendor breaker open")

// VendorBreakerState is a live, credential-free view of a vendor's circuit.
// Since and Kind describe the original trip and disappear after reset/expiry.
type VendorBreakerState struct {
	Breaker string          `json:"breaker"`
	Since   *time.Time      `json:"since,omitempty"`
	Kind    VendorErrorKind `json:"kind,omitempty"`
	until   time.Time
}

// VendorBreaker is safe for concurrent use. Its zero value uses the real clock.
// An open circuit retains its original trip; rejected calls never extend it.
type VendorBreaker struct {
	mu     sync.Mutex
	now    func() time.Time
	states map[string]VendorBreakerState
}

// NewVendorBreaker accepts an optional clock for deterministic expiry tests.
func NewVendorBreaker(now func() time.Time) *VendorBreaker { return &VendorBreaker{now: now} }
func (b *VendorBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}
func (b *VendorBreaker) State(vendor string) VendorBreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[vendor]
	if !ok || !b.clock().Before(s.until) {
		delete(b.states, vendor)
		return VendorBreakerState{Breaker: "closed"}
	}
	// Do not expose the stored timestamp pointer to callers.
	since := *s.Since
	s.Since = &since
	return s
}
func (b *VendorBreaker) Open(vendor string) bool { return b.State(vendor).Breaker == "open" }
func (b *VendorBreaker) Trip(vendor string, kind VendorErrorKind, ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if s, ok := b.states[vendor]; ok && now.Before(s.until) {
		return
	}
	if b.states == nil {
		b.states = make(map[string]VendorBreakerState)
	}
	since := now.UTC()
	b.states[vendor] = VendorBreakerState{Breaker: "open", Since: &since, Kind: kind, until: now.Add(ttl)}
}
func (b *VendorBreaker) Reset(vendor string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, vendor)
}

// DefaultVendorBreaker is shared by all hosted Anthropic/OpenAI Mills clients.
// Inject a separate breaker in client config for isolation in tests.
var DefaultVendorBreaker = NewVendorBreaker(nil)

func vendorBreaker(b *VendorBreaker) *VendorBreaker {
	if b != nil {
		return b
	}
	return DefaultVendorBreaker
}
func (b *VendorBreaker) check(vendor string) error {
	s := b.State(vendor)
	if s.Breaker != "open" {
		return nil
	}
	// Report the kind that tripped the circuit: a 2m rate-limit/overloaded trip
	// must not read as billing to callers that route on IsVendorBilling, nor
	// inflate the billing counter for every rejected call in the window.
	kind := s.Kind
	if kind == "" {
		kind = VendorBilling
	}
	e := &VendorError{Vendor: vendor, Kind: kind, Err: ErrVendorBreakerOpen}
	mills.VendorErrorsTotal.WithLabelValues(vendor, string(e.Kind)).Inc()
	return e
}
func (b *VendorBreaker) failure(vendor string, err error) error {
	e := classifyVendorError(vendor, err)
	mills.VendorErrorsTotal.WithLabelValues(vendor, string(e.Kind)).Inc()
	if errors.Is(e, ErrVendorBreakerOpen) {
		return e
	}
	switch e.Kind {
	case VendorBilling, VendorAuth:
		b.Trip(vendor, e.Kind, 30*time.Minute)
	case VendorRateLimit, VendorOverloaded:
		b.Trip(vendor, e.Kind, 2*time.Minute)
	}
	return e
}
func vendorKnownZero(err error) bool {
	e, ok := AsVendorError(err)
	return errors.Is(err, ErrVendorBreakerOpen) || (ok && (e.Kind == VendorBilling || e.Kind == VendorAuth))
}
func init() {
	for _, vendor := range []string{"anthropic", "openai", "openrouter"} {
		mills.RegisterVendorBreakerGauge(vendor, func() bool { return DefaultVendorBreaker.Open(vendor) })
	}
}
