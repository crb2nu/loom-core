package audit

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/crb2nu/loom/internal/contracts"
)

func TestClassifyIncidentMessage_RecurringExternalDependencyFixtures(t *testing.T) {
	cases := []struct {
		name       string
		message    string
		dependency contracts.IncidentDependency
		reason     string
		retryable  bool
		labels     []string
	}{
		{
			name:       "kubernetes pod gc race",
			message:    "pod not found during reconciliation",
			dependency: contracts.IncidentDependencyKubernetes,
			reason:     "kubernetes-control-plane",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/kubernetes",
				"incident_reason/kubernetes-control-plane",
				"retryable/true",
			},
		},
		{
			name:       "kubelet exec dial",
			message:    "agent CLI exited 1: exec error: error dialing backend: proxy error from 127.0.0.1:6443 while dialing 192.168.50.213:10250, code 502: 502 Bad Gateway",
			dependency: contracts.IncidentDependencyKubernetes,
			reason:     "kubernetes-control-plane",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/kubernetes",
				"incident_reason/kubernetes-control-plane",
				"retryable/true",
			},
		},
		{
			name:       "mcp websocket close",
			message:    "devbox quality_gate: mcphub: recv devbox/devbox_quality_gate: read message: websocket: close 1006 (abnormal closure): unexpected EOF",
			dependency: contracts.IncidentDependencyMCPGateway,
			reason:     "mcp-gateway-transport",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/mcp_gateway",
				"incident_reason/mcp-gateway-transport",
				"retryable/true",
			},
		},
		{
			name:       "flexinfer timeout",
			message:    `flexinfer chat: Post "http://flexinfer-proxy.flexinfer-system.svc.cluster.local/v1/chat/completions": context deadline exceeded`,
			dependency: contracts.IncidentDependencyFlexInfer,
			reason:     "flexinfer-timeout",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/flexinfer",
				"incident_reason/flexinfer-timeout",
				"retryable/true",
			},
		},
		{
			name:       "gitlab service unavailable",
			message:    "gitlab: GET /projects/47/merge_requests/12: status 503: service unavailable",
			dependency: contracts.IncidentDependencyGitLab,
			reason:     "gitlab-service-unavailable",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/gitlab",
				"incident_reason/gitlab-service-unavailable",
				"retryable/true",
			},
		},
		{
			name:       "gitlab merge configuration",
			message:    `gitlab: PUT /projects/services%2Floom-core/merge_requests/598/merge: status 405: {"message":"405 Method Not Allowed"}`,
			dependency: contracts.IncidentDependencyGitLab,
			reason:     "gitlab-merge-config",
			retryable:  false,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/gitlab",
				"incident_reason/gitlab-merge-config",
				"retryable/false",
			},
		},
		{
			name:       "spawn pool saturation",
			message:    `hud spawn: POST status 400: {"ok":false,"error":{"code":"spawn_error","message":"max concurrent spawns reached (6)"}}`,
			dependency: contracts.IncidentDependencySpawnPool,
			reason:     "spawn-pool-saturated",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/spawn_pool",
				"incident_reason/spawn-pool-saturated",
				"retryable/true",
			},
		},
		{
			name:       "sandbox infrastructure",
			message:    "stage=plan_slice attempt=1: image build failed: create buildah pod: pods already exists",
			dependency: contracts.IncidentDependencySandbox,
			reason:     "sandbox-infrastructure",
			retryable:  false,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/sandbox",
				"incident_reason/sandbox-infrastructure",
				"retryable/false",
			},
		},
		{
			name:       "generic network",
			message:    "clone repo: dial tcp: lookup gitlab.flexinfer.ai: no such host",
			dependency: contracts.IncidentDependencyNetwork,
			reason:     "network-transport",
			retryable:  true,
			labels: []string{
				"kind/incident",
				"incident/external-dependency",
				"dependency/network",
				"incident_reason/network-transport",
				"retryable/true",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyIncidentMessage(tc.message)
			if !got.Matched {
				t.Fatalf("Matched = false, want true")
			}
			if got.Classifier != contracts.IncidentClassifierExternalDependency {
				t.Fatalf("Classifier = %q, want %q", got.Classifier, contracts.IncidentClassifierExternalDependency)
			}
			if got.Category != contracts.IncidentCategoryExternalDependency {
				t.Fatalf("Category = %q, want %q", got.Category, contracts.IncidentCategoryExternalDependency)
			}
			if got.Dependency != tc.dependency {
				t.Fatalf("Dependency = %q, want %q", got.Dependency, tc.dependency)
			}
			if got.Reason != tc.reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.reason)
			}
			if got.Retryable != tc.retryable {
				t.Fatalf("Retryable = %v, want %v", got.Retryable, tc.retryable)
			}
			if !equalStrings(got.Labels, tc.labels) {
				t.Fatalf("Labels = %#v, want %#v", got.Labels, tc.labels)
			}
		})
	}
}

func TestClassifyIncident_UnknownAndNilDoNotMatch(t *testing.T) {
	for _, got := range []contracts.IncidentClassification{
		ClassifyIncident(nil),
		ClassifyIncidentMessage(""),
		ClassifyIncidentMessage("go test FAIL: TestFoo not equal to bar"),
	} {
		if got.Classifier != contracts.IncidentClassifierExternalDependency {
			t.Fatalf("Classifier = %q, want %q", got.Classifier, contracts.IncidentClassifierExternalDependency)
		}
		if got.Matched {
			t.Fatalf("Matched = true for %#v, want false", got)
		}
		if len(got.Labels) != 0 {
			t.Fatalf("Labels = %#v, want none", got.Labels)
		}
	}
}

func TestClassifyIncident_EOFUsesNetworkContract(t *testing.T) {
	got := ClassifyIncident(io.ErrUnexpectedEOF)
	if got.Dependency != contracts.IncidentDependencyNetwork {
		t.Fatalf("Dependency = %q, want %q", got.Dependency, contracts.IncidentDependencyNetwork)
	}
	if got.Reason != "network-eof" {
		t.Fatalf("Reason = %q, want network-eof", got.Reason)
	}
	if !got.Retryable {
		t.Fatal("Retryable = false, want true")
	}
}

func TestIncidentClassificationJSONContract(t *testing.T) {
	got := ClassifyIncident(errors.New("flexinfer chat: status 429: too many requests"))
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"classifier":"incident-classifier","matched":true,"category":"external_dependency","dependency":"flexinfer","reason":"flexinfer-rate-limit","retryable":true,"labels":["kind/incident","incident/external-dependency","dependency/flexinfer","incident_reason/flexinfer-rate-limit","retryable/true"]}`
	if string(data) != want {
		t.Fatalf("JSON = %s, want %s", data, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
