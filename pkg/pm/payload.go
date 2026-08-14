package pm

import (
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// httpClient returns a shared default HTTP client for the Qdrant transport.
func httpClient() *httpclient.Client {
	return httpclient.NewDefault()
}

// riskToPayload serializes a Risk into a Qdrant payload. Times use
// RFC3339Nano to match sibling collections.
func riskToPayload(r Risk) map[string]any {
	return map[string]any{
		"id":         r.ID,
		"project":    r.Project,
		"title":      r.Title,
		"likelihood": r.Likelihood,
		"impact":     r.Impact,
		"mitigation": r.Mitigation,
		"owner":      r.Owner,
		"status":     r.Status,
		"links":      r.Links,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// riskFromPayload deserializes a Qdrant payload back into a Risk.
func riskFromPayload(payload map[string]any) Risk {
	return Risk{
		ID:         toString(payload["id"]),
		Project:    toString(payload["project"]),
		Title:      toString(payload["title"]),
		Likelihood: toString(payload["likelihood"]),
		Impact:     toString(payload["impact"]),
		Mitigation: toString(payload["mitigation"]),
		Owner:      toString(payload["owner"]),
		Status:     toString(payload["status"]),
		Links:      toStringSlice(payload["links"]),
		CreatedAt:  parseTime(payload["created_at"]),
		UpdatedAt:  parseTime(payload["updated_at"]),
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toStringSlice(v any) []string {
	switch arr := v.(type) {
	case []string:
		return arr
	case []any:
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func parseTime(v any) time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}
