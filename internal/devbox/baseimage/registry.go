// Package baseimage provides a registry of pre-built base images for common
// language stacks. These images are pre-pushed to Harbor so Dockerfiles can
// use them as FROM targets, skipping the runtime/tool install layers.
package baseimage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type ProbeOutcome string

const (
	ProbeAvailable    ProbeOutcome = "available"
	ProbeMissing      ProbeOutcome = "missing"
	ProbeUnauthorized ProbeOutcome = "unauthorized"
	ProbeTransport    ProbeOutcome = "transport"
)

var probeOutcomes = []ProbeOutcome{ProbeAvailable, ProbeMissing, ProbeUnauthorized, ProbeTransport}

// ProbeOutcomes returns the bounded values used by the probe metric.
func ProbeOutcomes() []ProbeOutcome { return append([]ProbeOutcome(nil), probeOutcomes...) }

// RegistryCredentials are optional credentials for a Docker Registry v2 probe.
type RegistryCredentials struct{ Username, Password string }

// entry maps a language+version to a pre-built Harbor image tag.
type entry struct {
	Language string
	Version  string
	Image    string
}

// registry holds known pre-built base images.
// Entries are populated by scripts/build-base-images.sh pushing to Harbor.
var registry = []entry{
	{"go", "1.24", "registry.harbor.lan/mcp/devbox-base/go:1.24"},
	{"go", "1.25", "registry.harbor.lan/mcp/devbox-base/go:1.25"},
	{"go", "1.26", "registry.harbor.lan/mcp/devbox-base/go:1.26"},
	{"python", "3.12", "registry.harbor.lan/mcp/devbox-base/python:3.12"},
	{"python", "3.13", "registry.harbor.lan/mcp/devbox-base/python:3.13"},
	{"node", "20", "registry.harbor.lan/mcp/devbox-base/node:20"},
	{"node", "22", "registry.harbor.lan/mcp/devbox-base/node:22"},
}

// Lookup returns the pre-built base image tag for a language and version.
// Returns empty string if no pre-built base exists.
func Lookup(language, version string) string {
	language = strings.ToLower(language)
	for _, candidate := range versionCandidates(version) {
		for _, e := range registry {
			if e.Language == language && e.Version == candidate {
				return e.Image
			}
		}
	}
	return ""
}

func versionCandidates(version string) []string {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	candidates := []string{version}
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		candidates = append(candidates, parts[0]+"."+parts[1])
	}
	if len(parts) >= 1 {
		candidates = append(candidates, parts[0])
	}
	return candidates
}

// Languages returns all registered language+version pairs.
func Languages() []struct{ Language, Version, Image string } {
	result := make([]struct{ Language, Version, Image string }, len(registry))
	for i, e := range registry {
		result[i] = struct{ Language, Version, Image string }{e.Language, e.Version, e.Image}
	}
	return result
}

// ProbeResult reports the classified result for one registered base tag.
// Registry availability is advisory: callers should warn and continue startup.
type ProbeResult struct {
	Language string
	Version  string
	Image    string
	Outcome  ProbeOutcome
	Reason   string
}

// ProbeRegistry checks every mapped image with the Docker Registry v2 API.
// registryURL may override the registry host for tests or alternate deployments.
func ProbeRegistry(ctx context.Context, client *http.Client, registryURL string, credentials RegistryCredentials) []ProbeResult {
	var results []ProbeResult
	for _, item := range Languages() {
		image := item.Image
		slash := strings.IndexByte(image, '/')
		colon := strings.LastIndexByte(image, ':')
		if slash < 0 || colon <= slash {
			continue
		}
		base := registryURL
		if base == "" {
			base = "https://" + image[:slash]
		}
		manifestURL := strings.TrimRight(base, "/") + "/v2/" + image[slash+1:colon] + "/manifests/" + url.PathEscape(image[colon+1:])
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
		if err != nil {
			results = append(results, ProbeResult{item.Language, item.Version, image, ProbeTransport, err.Error()})
			continue
		}
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
		if credentials.Username != "" {
			req.SetBasicAuth(credentials.Username, credentials.Password)
		}
		resp, err := client.Do(req)
		if err != nil {
			results = append(results, ProbeResult{item.Language, item.Version, image, ProbeTransport, err.Error()})
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && credentials.Username != "" {
			resp, err = retryBearer(ctx, client, req, resp.Header.Get("WWW-Authenticate"), credentials)
			if err != nil {
				results = append(results, ProbeResult{item.Language, item.Version, image, ProbeTransport, err.Error()})
				continue
			}
			resp.Body.Close()
		}
		outcome := ProbeTransport
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			outcome = ProbeAvailable
		case resp.StatusCode == http.StatusNotFound:
			outcome = ProbeMissing
		case resp.StatusCode == http.StatusUnauthorized:
			outcome = ProbeUnauthorized
		}
		results = append(results, ProbeResult{item.Language, item.Version, image, outcome, fmt.Sprintf("status %d", resp.StatusCode)})
	}
	return results
}

func retryBearer(ctx context.Context, client *http.Client, manifestReq *http.Request, challenge string, credentials RegistryCredentials) (*http.Response, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return client.Do(manifestReq)
	}
	values := map[string]string{}
	for _, field := range strings.Split(challenge[len("Bearer "):], ",") {
		parts := strings.SplitN(strings.TrimSpace(field), "=", 2)
		if len(parts) == 2 {
			values[parts[0]] = strings.Trim(parts[1], `"`)
		}
	}
	tokenURL, err := url.Parse(values["realm"])
	if err != nil || tokenURL.Scheme == "" {
		return client.Do(manifestReq)
	}
	query := tokenURL.Query()
	for _, key := range []string{"service", "scope"} {
		if values[key] != "" {
			query.Set(key, values[key])
		}
	}
	tokenURL.RawQuery = query.Encode()
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return nil, err
	}
	tokenReq.SetBasicAuth(credentials.Username, credentials.Password)
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return nil, err
	}
	defer tokenResp.Body.Close()
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if tokenResp.StatusCode/100 != 2 || json.NewDecoder(tokenResp.Body).Decode(&payload) != nil {
		return client.Do(manifestReq)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	retry := manifestReq.Clone(ctx)
	retry.Header.Set("Authorization", "Bearer "+token)
	return client.Do(retry)
}
