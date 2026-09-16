package health

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const flightdeckBatchPath = "/api/v1/events/batch"

type ErrorCounter interface{ Inc() }

// FactoryEvent is the Mills lifecycle contract carried through Flightdeck's
// generic event ingest. CostUSD is cumulative and is populated on terminals.
type FactoryEvent struct {
	Type       string    `json:"event_type"`
	RunID      string    `json:"run_id"`
	BacklogID  string    `json:"backlog_id"`
	Stage      string    `json:"stage,omitempty"`
	Attempt    int       `json:"attempt"`
	Class      string    `json:"class,omitempty"`
	CostUSD    float64   `json:"cost_usd,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

type flightdeckEnvelope struct {
	Version    int          `json:"v"`
	EventUUID  string       `json:"event_uuid"`
	Platform   string       `json:"platform"`
	Origin     string       `json:"origin"`
	EventType  string       `json:"event_type"`
	OccurredAt string       `json:"occurred_at"`
	Payload    FactoryEvent `json:"payload"`
}

type flightdeckBatch struct {
	Events []flightdeckEnvelope `json:"events"`
}

type FlightdeckClientConfig struct {
	Endpoint    string
	Token       string
	Timeout     time.Duration
	QueueSize   int
	MaxAttempts int
	HTTPClient  *http.Client
	Errors      ErrorCounter
}

// FlightdeckClient is deliberately fail-open: Emit only attempts a bounded
// enqueue, while its single worker owns all network waits and retries.
type FlightdeckClient struct {
	endpoint    string
	token       string
	http        *http.Client
	queue       chan FactoryEvent
	maxAttempts int
	errors      ErrorCounter
	done        chan struct{}
	once        sync.Once
}

func NewFlightdeckClient(cfg FlightdeckClientConfig) *FlightdeckClient {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Token) == "" {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 128
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	c := &FlightdeckClient{endpoint: strings.TrimRight(cfg.Endpoint, "/") + flightdeckBatchPath, token: cfg.Token, http: hc, queue: make(chan FactoryEvent, cfg.QueueSize), maxAttempts: cfg.MaxAttempts, errors: cfg.Errors, done: make(chan struct{})}
	go c.run()
	return c
}

func (c *FlightdeckClient) Emit(event FactoryEvent) {
	if c == nil {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	select {
	case c.queue <- event:
	default:
		c.countError()
	}
}

func (c *FlightdeckClient) Close() {
	if c != nil {
		c.once.Do(func() { close(c.queue); <-c.done })
	}
}

func (c *FlightdeckClient) run() {
	defer close(c.done)
	for event := range c.queue {
		if err := c.deliver(event); err != nil {
			c.countError()
		}
	}
}

func (c *FlightdeckClient) deliver(event FactoryEvent) error {
	stamp := event.OccurredAt.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(event.Type + "\x00" + event.RunID + "\x00" + event.Stage + "\x00" + fmt.Sprint(event.Attempt) + "\x00" + stamp))
	env := flightdeckEnvelope{Version: 1, EventUUID: hex.EncodeToString(sum[:16]), Platform: "loom-mills", Origin: "factory", EventType: event.Type, OccurredAt: stamp, Payload: event}
	body, err := json.Marshal(flightdeckBatch{Events: []flightdeckEnvelope{env}})
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodPost, c.endpoint, bytes.NewReader(body))
		if reqErr != nil {
			return reqErr
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := c.http.Do(req)
		if doErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			doErr = fmt.Errorf("flightdeck ingest returned %s", resp.Status)
		}
		last = doErr
	}
	return last
}

func (c *FlightdeckClient) countError() {
	if c.errors != nil {
		c.errors.Inc()
	}
}
