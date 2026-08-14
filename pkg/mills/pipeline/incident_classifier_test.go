package pipeline

import "testing"

func TestClassifyIncident(t *testing.T) {
	tests := []struct {
		name, source, message, dependency, shape string
		retryable                                bool
	}{
		{"gitlab unavailable", "gitlab-ci", "GitLab API status 503: Service Unavailable", "gitlab", "service-unavailable", true},
		{"gitlab runner", "gitlab-ci", "ERROR: Runner system failure", "gitlab-runner", "runner-system-failure", true},
		{"provider quota", "model-provider", "OpenAI provider status 429: rate limit exceeded", "model-provider", "rate-limit", true},
		{"provider envelope", "model-provider", `rubric judge: no parseable score envelope; raw=""`, "model-provider", "ungradeable-response", false},
		{"object storage", "storage", "S3 artifact storage status 503: Service Unavailable", "object-storage", "service-unavailable", true},
		{"registry storage", "storage", "registry cache: error writing manifest blob: blob upload unknown", "container-registry-storage", "manifest-write", false},
		{"capacity", "storage", "Longhorn reports no available disk for replica", "storage", "capacity-exhausted", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ClassifyIncident(tt.source, tt.message)
			if !ok {
				t.Fatal("ClassifyIncident did not match")
			}
			if got.Dependency != tt.dependency || got.Shape != tt.shape || got.Retryable != tt.retryable {
				t.Fatalf("classification = %+v", got)
			}
			again, ok := ClassifyIncident(tt.source, tt.message)
			if !ok || again.ID != got.ID {
				t.Fatalf("classification is not deterministic: %q then %q", got.ID, again.ID)
			}
		})
	}
}

func TestClassifyIncidentNearMisses(t *testing.T) {
	tests := []struct{ source, message string }{
		{"gitlab-ci", "application returned status 503"},
		{"gitlab-ci", "GitLab pipeline failed because go test failed"},
		{"model-provider", "status 429 from GitLab"},
		{"model-provider", "OpenAI request completed"},
		{"storage", "service unavailable while calling GitLab"},
		{"storage", "manifest generated successfully"},
		{"unknown", "GitLab status 503 service unavailable"},
		{"", ""},
	}
	for _, tt := range tests {
		if got, ok := ClassifyIncident(tt.source, tt.message); ok {
			t.Errorf("near miss (%q, %q) classified as %+v", tt.source, tt.message, got)
		}
	}
}
