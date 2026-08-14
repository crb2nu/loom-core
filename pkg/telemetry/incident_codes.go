package telemetry

// IncidentClass names the stable top-level class used in telemetry, labels,
// and persisted incident metadata.
type IncidentClass string

const (
	IncidentClassExternalDependency IncidentClass = "external_dependency_incident"
)

// IncidentReasonCode is a bounded, stable reason code for recurring external
// dependency incidents. Values are label-safe and should not be derived from
// free-form log text.
type IncidentReasonCode string

const (
	IncidentReasonBlobStorageManifestWrite IncidentReasonCode = "blob-storage-manifest-write"
	IncidentReasonGitLabAuthFailure        IncidentReasonCode = "gitlab-auth-failure"
	IncidentReasonGitLabRateLimit          IncidentReasonCode = "gitlab-rate-limit"
	IncidentReasonGitLabServiceUnavailable IncidentReasonCode = "gitlab-service-unavailable"
)

const (
	IncidentExternalIDBlobStorageManifestWrite = "external_dependency.blob_storage.manifest_write"
	IncidentExternalIDGitLabAuthFailure        = "external_dependency.gitlab.auth_failure"
	IncidentExternalIDGitLabRateLimit          = "external_dependency.gitlab.rate_limit"
	IncidentExternalIDGitLabServiceUnavailable = "external_dependency.gitlab.service_unavailable"
)

// IncidentCode is the structured telemetry contract for one recurring incident
// signature.
type IncidentCode struct {
	ID         string             `json:"id"`
	Class      IncidentClass      `json:"class"`
	Reason     IncidentReasonCode `json:"reason"`
	Kind       string             `json:"kind"`
	Dependency string             `json:"dependency"`
	Summary    string             `json:"summary"`
	Retryable  bool               `json:"retryable"`
	Terminal   bool               `json:"terminal"`
}

// LookupIncidentCode returns the structured incident contract for a stable
// reason code.
func LookupIncidentCode(reason IncidentReasonCode) (IncidentCode, bool) {
	code, ok := incidentCodesByReason[reason]
	return code, ok
}

// LookupIncidentCodeByID returns the structured incident contract for a stable
// external incident ID.
func LookupIncidentCodeByID(id string) (IncidentCode, bool) {
	code, ok := incidentCodesByID[id]
	return code, ok
}

var incidentCodes = []IncidentCode{
	{
		ID:         IncidentExternalIDBlobStorageManifestWrite,
		Class:      IncidentClassExternalDependency,
		Reason:     IncidentReasonBlobStorageManifestWrite,
		Kind:       "blob_storage",
		Dependency: "container_registry_blob_storage",
		Summary:    "container registry blob storage rejected manifest/cache writes",
		Retryable:  false,
		Terminal:   true,
	},
	{
		ID:         IncidentExternalIDGitLabAuthFailure,
		Class:      IncidentClassExternalDependency,
		Reason:     IncidentReasonGitLabAuthFailure,
		Kind:       "gitlab_auth",
		Dependency: "gitlab",
		Summary:    "GitLab API authentication failed",
		Retryable:  false,
		Terminal:   true,
	},
	{
		ID:         IncidentExternalIDGitLabRateLimit,
		Class:      IncidentClassExternalDependency,
		Reason:     IncidentReasonGitLabRateLimit,
		Kind:       "gitlab_ci",
		Dependency: "gitlab",
		Summary:    "GitLab API rate limit blocked CI status checks",
		Retryable:  true,
		Terminal:   false,
	},
	{
		ID:         IncidentExternalIDGitLabServiceUnavailable,
		Class:      IncidentClassExternalDependency,
		Reason:     IncidentReasonGitLabServiceUnavailable,
		Kind:       "gitlab_ci",
		Dependency: "gitlab",
		Summary:    "GitLab API or CI service was unavailable",
		Retryable:  true,
		Terminal:   false,
	},
}

var (
	incidentCodesByReason = incidentCodeReasonIndex()
	incidentCodesByID     = incidentCodeIDIndex()
)

func incidentCodeReasonIndex() map[IncidentReasonCode]IncidentCode {
	out := make(map[IncidentReasonCode]IncidentCode, len(incidentCodes))
	for _, code := range incidentCodes {
		out[code.Reason] = code
	}
	return out
}

func incidentCodeIDIndex() map[string]IncidentCode {
	out := make(map[string]IncidentCode, len(incidentCodes))
	for _, code := range incidentCodes {
		out[code.ID] = code
	}
	return out
}
