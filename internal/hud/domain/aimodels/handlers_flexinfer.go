package aimodels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// flexInferModelEntry is the simplified per-model shape served to the
// HUD frontend. The upstream proxy's response carries a richer
// `metadata` blob (aliases, backend, gpu_priority, …); we strip it
// down to the fields panels actually render so the wire payload stays
// small and the frontend doesn't have to know FlexInfer's internal
// schema. Phase / Ready come straight from `metadata.phase` and
// `metadata.ready` on the upstream model entry.
type flexInferModelEntry struct {
	ID    string `json:"id"`
	Ready bool   `json:"ready"`
	Phase string `json:"phase,omitempty"`
}

// upstreamModels mirrors the subset of the FlexInfer proxy's
// /v1/models response we need to compute the simplified shape above.
// Extra fields are tolerated thanks to JSON's default decoder.
type upstreamModels struct {
	Data []struct {
		ID       string `json:"id"`
		Metadata struct {
			Phase string `json:"phase"`
			Ready bool   `json:"ready"`
		} `json:"metadata"`
	} `json:"data"`
}

// handleFlexInferModels fetches the live model list from the daemon-
// configured FlexInfer proxy and returns the simplified per-model shape
// the HUD frontend renders. Empty proxy URL yields an empty list (HTTP
// 200) rather than 500, matching the rest of the aimodels domain's
// "fresh daemon should still load" stance.
//
// Network or parse failures degrade to an empty list + an "error"
// field so the frontend can surface "couldn't reach proxy" without the
// whole panel exploding.
func (d *AIModelsDomain) handleFlexInferModels(w http.ResponseWriter, r *http.Request) {
	proxyURL := strings.TrimRight(d.deps.FlexInferProxyURL(), "/")
	if proxyURL == "" {
		d.deps.WriteJSON(w, http.StatusOK, map[string]any{
			"models": []flexInferModelEntry{},
			"source": "",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	models, fetchErr := fetchFlexInferModels(ctx, proxyURL)
	if fetchErr != nil {
		// Best-effort: surface the error but still return 200 with an
		// empty list so the AuditPanel can render the pool with
		// 'unknown' badges instead of blowing up.
		d.deps.WriteJSON(w, http.StatusOK, map[string]any{
			"models": []flexInferModelEntry{},
			"source": proxyURL,
			"error":  fetchErr.Error(),
		})
		return
	}

	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"source": proxyURL,
	})
}

// fetchFlexInferModels does the actual HTTP GET. Split out so tests can
// drive the handler against a stub upstream without touching net/http
// internals.
func fetchFlexInferModels(ctx context.Context, proxyURL string) ([]flexInferModelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &flexInferUpstreamError{status: resp.StatusCode, body: string(body)}
	}
	var parsed upstreamModels
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]flexInferModelEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		out = append(out, flexInferModelEntry{
			ID:    m.ID,
			Ready: m.Metadata.Ready,
			Phase: m.Metadata.Phase,
		})
	}
	return out, nil
}

type flexInferUpstreamError struct {
	status int
	body   string
}

func (e *flexInferUpstreamError) Error() string {
	if e.body != "" {
		return "flexinfer upstream: status " + http.StatusText(e.status) + ": " + e.body
	}
	return "flexinfer upstream: status " + http.StatusText(e.status)
}
