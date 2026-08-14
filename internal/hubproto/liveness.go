package hubproto

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	MethodPing = "ping"
	MethodPong = "pong"
)

// LivenessPayload correlates a pong with the exact ping it acknowledges.
type LivenessPayload struct {
	PingID string `json:"ping_id"`
}

// NewPing creates the application-level keepalive envelope understood by the hub.
func NewPing(id, source string, now time.Time) *Envelope {
	payload, _ := json.Marshal(LivenessPayload{PingID: id})
	return &Envelope{Domain: DomainControl, Method: MethodPing, RequestID: id, Payload: payload, Source: source, Timestamp: now.UTC()}
}

// NewPong creates a response to a ping.
func NewPong(ping *Envelope, source string, now time.Time) (*Envelope, error) {
	id, err := ParsePing(ping)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(LivenessPayload{PingID: id})
	return &Envelope{Domain: DomainControl, Method: MethodPong, RequestID: id, Payload: payload, Source: source, Timestamp: now.UTC()}, nil
}

func ParsePing(env *Envelope) (string, error) { return parseLiveness(env, MethodPing) }
func ParsePong(env *Envelope) (string, error) { return parseLiveness(env, MethodPong) }

func parseLiveness(env *Envelope, method string) (string, error) {
	if env == nil || env.Domain != DomainControl || env.Method != method {
		return "", fmt.Errorf("hubproto: expected control/%s envelope", method)
	}
	var p LivenessPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return "", fmt.Errorf("hubproto: decode %s: %w", method, err)
	}
	if p.PingID == "" || (env.RequestID != "" && env.RequestID != p.PingID) {
		return "", fmt.Errorf("hubproto: invalid %s correlation", method)
	}
	return p.PingID, nil
}
