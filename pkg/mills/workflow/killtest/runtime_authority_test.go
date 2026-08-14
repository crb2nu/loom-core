package killtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAwaitPendingSpawnStopsOnOperatorAuthorityMismatch(t *testing.T) {
	h, requests := newRuntimeAuthorityDriftHarness(Config{
		PollInterval: 200 * time.Millisecond,
		StepTimeout:  5 * time.Second,
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	_, err := h.AwaitPendingSpawn(ctx, "wf-canary-1")
	if !errors.Is(err, ErrOperatorAuthority) {
		t.Fatalf("AwaitPendingSpawn() error = %v, want ErrOperatorAuthority", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("AwaitPendingSpawn() made %d requests, want one terminal authority check", got)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("AwaitPendingSpawn() waited for retry timeout: %v", err)
	}
}

func TestAwaitTerminalStopsOnOperatorAuthorityMismatch(t *testing.T) {
	h, requests := newRuntimeAuthorityDriftHarness(Config{
		PollInterval:    200 * time.Millisecond,
		TerminalTimeout: 5 * time.Second,
	})
	h.kubectlFn = func(_ context.Context, args ...string) (string, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "--field-selector metadata.name=spawn-abc"):
			return `{"items":[]}`, nil
		case strings.Contains(command, "get configmap loom-spawn-state"):
			return testSpawnStateConfigMapJSON(nil), nil
		case strings.Contains(command, "get pods -o json"):
			return `{"items":[]}`, nil
		default:
			return "", fmt.Errorf("unexpected kubectl call: %s", command)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	err := h.awaitTerminal(ctx, "wf-canary-1", "abc", &Evidence{}, nil)
	if !errors.Is(err, ErrOperatorAuthority) {
		t.Fatalf("awaitTerminal() error = %v, want ErrOperatorAuthority", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("awaitTerminal() made %d run requests, want one terminal authority check", got)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("awaitTerminal() waited for retry timeout: %v", err)
	}
}

func newRuntimeAuthorityDriftHarness(cfg Config) (*Harness, *atomic.Int32) {
	report := canonicalTestPreflight(authorityTestTime(), "", testOperatorPod(), testHUDPod())
	cfg.OperatorURL = "https://operator.example"
	cfg.RequireAuthorityBinding = true
	h := New(cfg)
	h.expectedOperatorPod = report.Operator
	h.expectedOperatorDeployment = report.OperatorDeployment
	h.operatorResponseAuthority = report.AuthorityPlane.Operator
	drifted := report.AuthorityPlane.Operator
	drifted.BootID = strings.Repeat("f", 64)

	requests := new(atomic.Int32)
	h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		response := jsonResponse(http.StatusOK, `{}`)
		response.Header = testOperatorAuthorityHeaders(drifted)
		return response, nil
	})}
	return h, requests
}
