package killtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func canaryRunResponse(runID, agentType string) string {
	return fmt.Sprintf(`{"run":{"id":%q,"engine":"imperative","template":%q,"template_version":%q,"interpreter_version":%q,"state":"running","agent_type":%q},"steps":[]}`,
		runID, workflow.CanaryTemplateName, workflow.CanaryTemplateVersion, workflow.HostInterpreterVersion, agentType)
}

func TestLaunchCanaryRecoversExactRunAfterLostResponse(t *testing.T) {
	const runID = "wf-canary-lost-response"
	h := New(Config{OperatorURL: "https://operator.example"})
	h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodPost:
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil ||
				payload["run_id"] != runID || payload["agent_type"] != AgentTypeCodex ||
				payload["merging"] != false {
				t.Fatalf("launch payload=%v err=%v", payload, err)
			}
			return nil, errors.New("connection reset after commit")
		case http.MethodGet:
			return jsonResponse(http.StatusOK, canaryRunResponse(runID, AgentTypeCodex)), nil
		default:
			t.Fatalf("unexpected method %s", request.Method)
			return nil, nil
		}
	})}
	got, err := h.LaunchCanary(t.Context(), runID, AgentTypeCodex)
	if err != nil || got != runID {
		t.Fatalf("LaunchCanary() = %q, %v", got, err)
	}
}

func TestLaunchCanaryRecoveryRejectsAgentIdentityDrift(t *testing.T) {
	const runID = "wf-canary-agent-drift"
	h := New(Config{OperatorURL: "https://operator.example"})
	h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			return nil, errors.New("connection reset after commit")
		}
		return jsonResponse(http.StatusOK, canaryRunResponse(runID, AgentTypeClaudeCode)), nil
	})}
	if _, err := h.LaunchCanary(t.Context(), runID, AgentTypeCodex); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("LaunchCanary() accepted recovered agent drift: %v", err)
	}
}

func TestLaunchCanaryRecoveryRejectsVersionIdentityDrift(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"template version": func(body string) string {
			return strings.Replace(body, `"template_version":"`+workflow.CanaryTemplateVersion+`"`, `"template_version":"v-next"`, 1)
		},
		"interpreter version": func(body string) string {
			return strings.Replace(body, `"interpreter_version":"`+workflow.HostInterpreterVersion+`"`, `"interpreter_version":"starlark-next"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example"})
			h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodPost {
					return nil, errors.New("connection reset after commit")
				}
				return jsonResponse(http.StatusOK, mutate(canaryRunResponse("wf-canary-version-drift", AgentTypeCodex))), nil
			})}
			if _, err := h.LaunchCanary(t.Context(), "wf-canary-version-drift", AgentTypeCodex); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("LaunchCanary() accepted recovered version drift: %v", err)
			}
		})
	}
}

func TestLaunchCanaryRejectsSuccessfulResponseAgentIdentityDrift(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example"})
	h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"id":"wf-canary-1","agent_type":"claude-code"}`), nil
	})}
	if _, err := h.LaunchCanary(t.Context(), "wf-canary-1", AgentTypeCodex); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("LaunchCanary() accepted successful response agent drift: %v", err)
	}
}

func TestLaunchCanaryRejectsSuccessfulResponseVersionIdentityDrift(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"template version": func(body string) string {
			return strings.Replace(body, `"template_version":"`+workflow.CanaryTemplateVersion+`"`, `"template_version":"v-next"`, 1)
		},
		"interpreter version": func(body string) string {
			return strings.Replace(body, `"interpreter_version":"`+workflow.HostInterpreterVersion+`"`, `"interpreter_version":"starlark-next"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			const runID = "wf-canary-success-version-drift"
			requests := 0
			h := New(Config{OperatorURL: "https://operator.example"})
			h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				switch request.Method {
				case http.MethodPost:
					return jsonResponse(http.StatusOK, `{"id":"`+runID+`","agent_type":"codex"}`), nil
				case http.MethodGet:
					return jsonResponse(http.StatusOK, mutate(canaryRunResponse(runID, AgentTypeCodex))), nil
				default:
					t.Fatalf("unexpected method %s", request.Method)
					return nil, nil
				}
			})}
			if _, err := h.LaunchCanary(t.Context(), runID, AgentTypeCodex); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("LaunchCanary() accepted successful version drift: %v", err)
			}
			if requests != 2 {
				t.Fatalf("LaunchCanary() made %d requests, want POST plus validation GET", requests)
			}
		})
	}
}

func TestLaunchCanaryValidatesSuccessfulCommittedRun(t *testing.T) {
	const runID = "wf-canary-success"
	requests := 0
	h := New(Config{OperatorURL: "https://operator.example"})
	h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method == http.MethodPost {
			return jsonResponse(http.StatusOK, `{"id":"`+runID+`","agent_type":"codex"}`), nil
		}
		return jsonResponse(http.StatusOK, canaryRunResponse(runID, AgentTypeCodex)), nil
	})}
	got, err := h.LaunchCanary(t.Context(), runID, AgentTypeCodex)
	if err != nil || got != runID || requests != 2 {
		t.Fatalf("LaunchCanary() = %q, %v requests=%d", got, err, requests)
	}
}

func TestValidateCanaryRunRejectsAttachedAgentIdentityDrift(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example"})
	h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, canaryRunResponse("wf-canary-1", AgentTypeClaudeCode)), nil
	})}
	if err := h.ValidateCanaryRun(t.Context(), "wf-canary-1", AgentTypeCodex); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("ValidateCanaryRun() accepted attached agent drift: %v", err)
	}
}

func TestLaunchCanaryRejectsUnsupportedAgent(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example"})
	if _, err := h.LaunchCanary(t.Context(), "wf-canary-1", "gemini"); err == nil {
		t.Fatal("LaunchCanary() accepted unsupported canary agent")
	}
}

func TestAcquireCrashLeaseRetriesSameRequestIDAfterLostResponse(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
	attempts := 0
	var firstRequestID string
	h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode lease request: %v", err)
		}
		if firstRequestID == "" {
			firstRequestID = payload["request_id"]
		} else if payload["request_id"] != firstRequestID {
			t.Fatalf("retry changed request_id: %q -> %q", firstRequestID, payload["request_id"])
		}
		if attempts == 1 {
			return nil, errors.New("response lost")
		}
		body, _ := json.Marshal(CrashLease{
			Token: "token-1", RequestID: firstRequestID, RunID: "wf-canary-1", SpawnID: "spawn-1",
			ExpiresAt: time.Now().UTC().Add(90 * time.Second),
		})
		return jsonResponse(http.StatusOK, string(body)), nil
	})}
	lease, err := h.AcquireCrashLease(t.Context(), "wf-canary-1", "spawn-1")
	if err != nil || lease.Token != "token-1" || attempts != 2 {
		t.Fatalf("AcquireCrashLease() = %+v, %v attempts=%d", lease, err, attempts)
	}
}

func TestLeaseErrorsNeverExposeBearerTokens(t *testing.T) {
	const requestToken = "S1C-LEASE-SENTINEL-DO-NOT-LOG"
	const responseToken = "S1C-RESPONSE-SENTINEL-DO-NOT-LOG"
	assertRedacted := func(t *testing.T, err error, secrets ...string) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		for _, formatted := range []string{fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			for _, secret := range secrets {
				if strings.Contains(formatted, secret) || strings.Contains(formatted, url.PathEscape(secret)) {
					t.Fatalf("error exposed bearer token: %q", formatted)
				}
			}
		}
	}

	t.Run("acquire transport", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: requestToken})
		h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport copied credential " + requestToken)
		})}
		_, err := h.AcquireCrashLease(t.Context(), "wf-canary-1", "spawn-1")
		assertRedacted(t, err, requestToken)
	})

	t.Run("renew transport", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
		h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset")
		})}
		_, err := h.RenewCrashLease(t.Context(), requestToken)
		assertRedacted(t, err, requestToken)
	})

	t.Run("release transport", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
		h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset")
		})}
		_, err := h.releaseCrashLeaseOnce(t.Context(), requestToken)
		assertRedacted(t, err, requestToken)
	})

	t.Run("acquire invalid response", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
		h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(CrashLease{
				Token: responseToken, RequestID: payload["request_id"], RunID: "wrong-run", SpawnID: "spawn-1",
				ExpiresAt: time.Now().UTC().Add(time.Minute),
			})
			return jsonResponse(http.StatusOK, string(body)), nil
		})}
		_, err := h.AcquireCrashLease(t.Context(), "wf-canary-1", "spawn-1")
		assertRedacted(t, err, responseToken)
	})

	t.Run("renew invalid response", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
		body, _ := json.Marshal(CrashLease{
			Token: responseToken, RequestID: "request-1", RunID: "wf-canary-1", SpawnID: "spawn-1",
			ExpiresAt: time.Now().UTC().Add(time.Minute),
		})
		h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, string(body)), nil
		})}
		_, err := h.RenewCrashLease(t.Context(), requestToken)
		assertRedacted(t, err, requestToken, responseToken)
	})

	t.Run("release response body", func(t *testing.T) {
		h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
		h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusConflict, "active token "+responseToken), nil
		})}
		_, err := h.releaseCrashLeaseOnce(t.Context(), requestToken)
		assertRedacted(t, err, requestToken, responseToken)
	})
}

func TestAuthorityMismatchIsNeverRetriedOrRecovered(t *testing.T) {
	report := canonicalTestPreflight(authorityTestTime(), "", testOperatorPod(), testHUDPod())
	for name, call := range map[string]func(*Harness) error{
		"canary launch": func(h *Harness) error {
			_, err := h.LaunchCanary(t.Context(), "wf-canary-1", AgentTypeCodex)
			return err
		},
		"lease acquire": func(h *Harness) error {
			_, err := h.AcquireCrashLease(t.Context(), "wf-canary-1", "spawn-1")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin", RequireAuthorityBinding: true})
			h.expectedOperatorPod = report.Operator
			h.expectedOperatorDeployment = report.OperatorDeployment
			h.operatorResponseAuthority = report.AuthorityPlane.Operator
			drifted := report.AuthorityPlane.Operator
			drifted.BootID = strings.Repeat("f", 64)
			requests := 0
			h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				response := jsonResponse(http.StatusOK, `{}`)
				response.Header = testOperatorAuthorityHeaders(drifted)
				return response, nil
			})}
			err := call(h)
			if !errors.Is(err, ErrOperatorAuthority) || requests != 1 {
				t.Fatalf("authority mismatch = %v requests=%d, want terminal single request", err, requests)
			}
		})
	}
}

func TestReleaseCrashLeaseRetriesTransientFailureUntilConfirmed(t *testing.T) {
	for name, firstResponse := range map[string]func() (*http.Response, error){
		"transport": func() (*http.Response, error) {
			return nil, errors.New("connection refused during endpoint turnover")
		},
		"server unavailable": func() (*http.Response, error) {
			return jsonResponse(http.StatusServiceUnavailable, "upstream unavailable"), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
			attempts := 0
			h.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				if request.Method != http.MethodDelete || request.URL.Path != "/api/mills/safety/crash-lease/token-1" {
					t.Fatalf("release request = %s %s", request.Method, request.URL.Path)
				}
				if authorization := request.Header.Get("Authorization"); authorization != "Bearer admin" {
					t.Fatalf("Authorization = %q", authorization)
				}
				if attempts == 1 {
					return firstResponse()
				}
				return jsonResponse(http.StatusNoContent, ""), nil
			})}

			if err := h.ReleaseCrashLease(t.Context(), "token-1"); err != nil || attempts != 2 {
				t.Fatalf("ReleaseCrashLease() = %v attempts=%d", err, attempts)
			}
		})
	}
}

func TestReleaseCrashLeaseAcceptsReleasedOrAbsentLease(t *testing.T) {
	for name, status := range map[string]int{
		"released": http.StatusNoContent,
		"absent":   http.StatusNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
			attempts := 0
			h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return jsonResponse(status, ""), nil
			})}

			if err := h.ReleaseCrashLease(t.Context(), "token-1"); err != nil || attempts != 1 {
				t.Fatalf("ReleaseCrashLease() = %v attempts=%d", err, attempts)
			}
		})
	}
}

func TestReleaseCrashLeaseFailsPermanentHTTPErrorImmediately(t *testing.T) {
	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"unauthorized":   {status: http.StatusUnauthorized, body: "invalid token"},
		"token mismatch": {status: http.StatusConflict, body: "active lease uses another token"},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{OperatorURL: "https://operator.example", AdminToken: "bad-token"})
			attempts := 0
			h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return jsonResponse(response.status, response.body), nil
			})}

			err := h.ReleaseCrashLease(t.Context(), "token-1")
			want := fmt.Sprintf("status %d", response.status)
			if err == nil || !strings.Contains(err.Error(), want) || attempts != 1 {
				t.Fatalf("ReleaseCrashLease() = %v attempts=%d", err, attempts)
			}
		})
	}
}

func TestReleaseCrashLeaseStopsAtContextDeadline(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
	attempts := 0
	h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return jsonResponse(http.StatusBadGateway, "route not ready"), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := h.ReleaseCrashLease(ctx, "token-1")
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 {
		t.Fatalf("ReleaseCrashLease() = %v attempts=%d", err, attempts)
	}
}

func TestReleaseCrashLeaseStopsOnContextCancellation(t *testing.T) {
	h := New(Config{OperatorURL: "https://operator.example", AdminToken: "admin"})
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	h.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		cancel()
		return jsonResponse(http.StatusServiceUnavailable, "route not ready"), nil
	})}

	err := h.ReleaseCrashLease(ctx, "token-1")
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("ReleaseCrashLease() = %v attempts=%d", err, attempts)
	}
}
