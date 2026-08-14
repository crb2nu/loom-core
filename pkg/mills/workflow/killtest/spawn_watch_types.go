package killtest

import "time"

// SpawnPodWatchEvent is the bounded, audit-grade record of one relevant Pod
// object delivered by the pre-launch resourceVersion stream. SpawnIDLabel is
// a pointer so evidence distinguishes an absent label from a present empty
// label, which is identity drift once the canonical spawn id is bound.
type SpawnPodWatchEvent struct {
	Type            string      `json:"type"`
	ResourceVersion string      `json:"resource_version"`
	ObservedAt      time.Time   `json:"observed_at"`
	Pod             PodIdentity `json:"pod"`
	SpawnIDLabel    *string     `json:"spawn_id_label,omitempty"`
}
