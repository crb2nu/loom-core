package telemetry

import "testing"

func TestLookupIncidentCode_ExternalDependencyCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		reason     IncidentReasonCode
		id         string
		dependency string
		retryable  bool
		terminal   bool
	}{
		{
			reason:     IncidentReasonBlobStorageManifestWrite,
			id:         IncidentExternalIDBlobStorageManifestWrite,
			dependency: "container_registry_blob_storage",
			retryable:  false,
			terminal:   true,
		},
		{
			reason:     IncidentReasonGitLabAuthFailure,
			id:         IncidentExternalIDGitLabAuthFailure,
			dependency: "gitlab",
			retryable:  false,
			terminal:   true,
		},
		{
			reason:     IncidentReasonGitLabRateLimit,
			id:         IncidentExternalIDGitLabRateLimit,
			dependency: "gitlab",
			retryable:  true,
			terminal:   false,
		},
		{
			reason:     IncidentReasonGitLabServiceUnavailable,
			id:         IncidentExternalIDGitLabServiceUnavailable,
			dependency: "gitlab",
			retryable:  true,
			terminal:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			got, ok := LookupIncidentCode(tc.reason)
			if !ok {
				t.Fatalf("LookupIncidentCode(%q) returned ok=false", tc.reason)
			}
			if got.ID != tc.id {
				t.Fatalf("ID = %q, want %q", got.ID, tc.id)
			}
			if got.Class != IncidentClassExternalDependency {
				t.Fatalf("Class = %q, want %q", got.Class, IncidentClassExternalDependency)
			}
			if got.Dependency != tc.dependency {
				t.Fatalf("Dependency = %q, want %q", got.Dependency, tc.dependency)
			}
			if got.Retryable != tc.retryable {
				t.Fatalf("Retryable = %v, want %v", got.Retryable, tc.retryable)
			}
			if got.Terminal != tc.terminal {
				t.Fatalf("Terminal = %v, want %v", got.Terminal, tc.terminal)
			}
			byID, ok := LookupIncidentCodeByID(tc.id)
			if !ok {
				t.Fatalf("LookupIncidentCodeByID(%q) returned ok=false", tc.id)
			}
			if byID != got {
				t.Fatalf("Lookup by ID = %+v, want %+v", byID, got)
			}
		})
	}
}
