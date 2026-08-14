package killtest

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type endpointReadError struct {
	err error
}

func (reader endpointReadError) Read([]byte) (int, error) {
	return 0, reader.err
}

func TestReadSafetyEndpointBodyEnforcesExactLimit(t *testing.T) {
	exact := strings.Repeat("x", int(maxSafetyEndpointResponseBytes))
	if body, err := readSafetyEndpointBody("test", strings.NewReader(exact)); err != nil || len(body) != len(exact) {
		t.Fatalf("exact-limit body = %d bytes, %v", len(body), err)
	}
	if _, err := readSafetyEndpointBody("test", strings.NewReader(exact+"x")); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body accepted: %v", err)
	}
}

func TestSafetyEndpointsPropagateBodyReadErrors(t *testing.T) {
	readErr := errors.New("injected response read failure")
	for name, call := range map[string]func(*Harness) error{
		"policy": func(h *Harness) error {
			_, _, _, _, err := h.effectivePolicy(t.Context())
			return err
		},
		"quiescence": func(h *Harness) error {
			_, err := h.quiescence(t.Context())
			return err
		},
		"renewed lease": func(h *Harness) error {
			_, err := h.RenewCrashLease(t.Context(), "lease-token")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
			h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Cache-Control": []string{"no-store"}},
					Body:       io.NopCloser(endpointReadError{err: readErr}),
				}, nil
			})}
			if err := call(h); !errors.Is(err, readErr) {
				t.Fatalf("body read error was not propagated: %v", err)
			}
		})
	}
}

func TestSafetyEndpointsRejectOversizedBodies(t *testing.T) {
	oversized := strings.Repeat("x", int(maxSafetyEndpointResponseBytes)+1)
	for name, call := range map[string]func(*Harness) error{
		"policy": func(h *Harness) error {
			_, _, _, _, err := h.effectivePolicy(t.Context())
			return err
		},
		"quiescence": func(h *Harness) error {
			_, err := h.quiescence(t.Context())
			return err
		},
		"renewed lease": func(h *Harness) error {
			_, err := h.RenewCrashLease(t.Context(), "lease-token")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
			h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Cache-Control": []string{"no-store"}},
					Body:       io.NopCloser(strings.NewReader(oversized)),
				}, nil
			})}
			if err := call(h); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("oversized response accepted: %v", err)
			}
		})
	}
}

func TestRemainingGateRESTEndpointsUseStrictBodyReader(t *testing.T) {
	readErr := errors.New("injected response read failure")
	oversized := strings.Repeat("x", int(maxSafetyEndpointResponseBytes)+1)
	faults := map[string]struct {
		body  func() io.ReadCloser
		match func(error) bool
	}{
		"read error": {
			body:  func() io.ReadCloser { return io.NopCloser(endpointReadError{err: readErr}) },
			match: func(err error) bool { return errors.Is(err, readErr) },
		},
		"oversized": {
			body: func() io.ReadCloser { return io.NopCloser(strings.NewReader(oversized)) },
			match: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "exceeds")
			},
		},
	}
	callers := map[string]func(*Harness, func() io.ReadCloser) error{
		"acquire crash lease": func(h *Harness, body func() io.ReadCloser) error {
			h.http = endpointFaultClient(http.StatusOK, body)
			_, err := h.AcquireCrashLease(t.Context(), "wf-canary-1", "spawn-1")
			return err
		},
		"release crash lease": func(h *Harness, body func() io.ReadCloser) error {
			h.http = endpointFaultClient(http.StatusBadRequest, body)
			return h.ReleaseCrashLease(t.Context(), "lease-token")
		},
		"canary launch and recovery": func(h *Harness, body func() io.ReadCloser) error {
			h.http = endpointFaultClient(http.StatusOK, body)
			_, err := h.LaunchCanary(t.Context(), "wf-canary-1", AgentTypeCodex)
			return err
		},
		"run detail": func(h *Harness, body func() io.ReadCloser) error {
			h.http = endpointFaultClient(http.StatusOK, body)
			_, err := h.GetRun(t.Context(), "wf-canary-1")
			return err
		},
		"fail run": func(h *Harness, body func() io.ReadCloser) error {
			h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet {
					return jsonResponse(http.StatusOK, canaryRunResponse("wf-canary-1", AgentTypeCodex)), nil
				}
				return endpointFaultResponse(http.StatusOK, body), nil
			})}
			return h.FailRun(t.Context(), "wf-canary-1", "cleanup")
		},
		"stop spawn": func(h *Harness, body func() io.ReadCloser) error {
			h.http = endpointFaultClient(http.StatusOK, body)
			return h.StopSpawn(t.Context(), "spawn-1")
		},
	}
	for callerName, call := range callers {
		t.Run(callerName, func(t *testing.T) {
			for faultName, fault := range faults {
				t.Run(faultName, func(t *testing.T) {
					h := New(Config{
						OperatorURL: "https://operator.example", AdminToken: "admin",
						HudURL: "https://hud.example", HudAdminToken: "hud-admin",
					})
					if err := call(h, fault.body); !fault.match(err) {
						t.Fatalf("response fault was not rejected: %v", err)
					}
				})
			}
		})
	}
}

func endpointFaultClient(status int, body func() io.ReadCloser) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return endpointFaultResponse(status, body), nil
	})}
}

func endpointFaultResponse(status int, body func() io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Cache-Control": []string{"no-store"}},
		Body:       body(),
	}
}
