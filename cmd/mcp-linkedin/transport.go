package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mcperror"
)

func (s *linkedInServer) requestJSON(ctx context.Context, method, path string, body any) (any, error) {
	raw, err := s.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}

	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, mcperror.ParseError("LinkedIn API response JSON", err)
	}
	return out, nil
}

func (s *linkedInServer) request(ctx context.Context, method, path string, body any) ([]byte, error) {
	if s.shouldUseBrowserKitTransport(path) {
		payload, err := s.requestViaBrowserKit(ctx, method, path, body)
		if err == nil {
			return payload, nil
		}
		s.logger.Warn("linkedin browserkit primary transport failed; falling back to http", "path", path, "error", err)
	}
	return s.requestWithRecovery(ctx, method, path, body, true)
}

func (s *linkedInServer) requestWithRecovery(ctx context.Context, method, path string, body any, allowRecovery bool) ([]byte, error) {
	payload, err := s.doRequest(ctx, method, path, body)
	if err == nil {
		return payload, nil
	}

	if (!allowRecovery || !isAuthChallengeErr(err)) && isAuthChallengeErr(err) && s.shouldUseBrowserKitTransport(path) {
		s.logger.Warn("linkedin auth challenge detected; attempting browserkit transport fallback", "path", path)
		return s.requestViaBrowserKit(ctx, method, path, body)
	}

	if !allowRecovery || !isAuthChallengeErr(err) {
		return nil, err
	}

	s.logger.Warn("linkedin auth challenge detected; attempting one-time recovery", "path", path)
	if recErr := s.maybeRecoverAfterChallenge(ctx, path); recErr != nil {
		return nil, recErr
	}
	return s.requestWithRecovery(ctx, method, path, body, false)
}

func (s *linkedInServer) doRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reqBody *bytes.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, mcperror.ParseError("request body", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reqBody)
	if err != nil {
		return nil, mcperror.OperationFailed("create request", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
	// LinkedIn's anti-abuse layer revokes sessions presented by clients that
	// don't look like the browser that minted them: Go's default
	// "Go-http-client" user agent (or no browser headers at all) on the
	// messaging endpoints gets the whole session invalidated server-side, not
	// just the request rejected. Always present browser-consistent headers.
	req.Header.Set("User-Agent", s.httpUserAgent())
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
	}
	if cookieValue := s.cookieHeader(); cookieValue != "" {
		req.Header.Set("Cookie", cookieValue)
	}
	if csrfToken := csrfTokenFromJSessionID(s.jsessionID); csrfToken != "" {
		req.Header.Set("csrf-token", csrfToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "too many redirects") || strings.Contains(msg, "stopped after 10 redirects") {
			return nil, &authChallengeError{statusCode: 0, body: err.Error()}
		}
		return nil, mcperror.WrapAPI("LinkedIn", err)
	}
	defer resp.Body.Close()

	payload, truncated, err := httpclient.ReadBodyWithLimit(resp.Body, maxResponseBytes)
	if err != nil {
		return nil, mcperror.OperationFailed("read LinkedIn API response", err)
	}
	if truncated {
		return nil, mcperror.ServerError("LinkedIn response exceeded 2MB limit")
	}
	if isLinkedInSessionInvalidation(resp, payload) {
		return nil, &authChallengeError{statusCode: resp.StatusCode, body: "LinkedIn invalidated session cookies"}
	}
	if resp.StatusCode >= 400 {
		if isLinkedInAuthChallenge(resp.StatusCode, payload) {
			return nil, &authChallengeError{statusCode: resp.StatusCode, body: string(payload)}
		}
		return nil, mcperror.APIError("LinkedIn", resp.StatusCode, string(payload))
	}

	return payload, nil
}

func (s *linkedInServer) httpUserAgent() string {
	if ua := strings.TrimSpace(s.userAgent); ua != "" {
		return ua
	}
	return defaultLinkedInHTTPUserAgent
}

// cookieHeader assembles the Cookie header for Voyager calls. A session
// cookie alone is not enough for LinkedIn's fraud checks: a li_at presented
// without the device cookies (bcookie/bscookie/lidc/...) that accompanied it
// at login looks stolen and gets the session revoked. When
// LINKEDIN_COOKIE_BUNDLE provides those companion cookies, they are merged
// in, with the dedicated li_at/JSESSIONID values taking precedence.
func (s *linkedInServer) cookieHeader() string {
	if strings.TrimSpace(s.sessionToken) == "" {
		return ""
	}

	order := []string{}
	values := map[string]string{}
	set := func(name, value string) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			return
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}

	for _, pair := range strings.Split(s.cookieBundle, ";") {
		name, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		set(name, value)
	}
	set("li_at", s.sessionToken)
	set("JSESSIONID", normalizeJSessionID(s.jsessionID))

	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

func normalizeJSessionID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	trimmed := strings.Trim(v, "\"")
	return `"` + trimmed + `"`
}

func csrfTokenFromJSessionID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return strings.Trim(v, "\"")
}
