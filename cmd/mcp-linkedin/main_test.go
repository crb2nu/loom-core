package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/mcptest"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcpotel"
)

func TestUnconfiguredLinkedInToolReturnsMCPError(t *testing.T) {
	errConfig := mcperror.NotConfigured("LINKEDIN_ACCESS_TOKEN or LINKEDIN_SESSION_COOKIE", "set credentials")
	h := requireConfigured(errConfig, func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		t.Fatal("backend handler must not run")
		return nil, nil
	})
	result, err := h(context.Background(), nil)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content[0].Text, "LINKEDIN_ACCESS_TOKEN") || !strings.Contains(result.Content[0].Text, "LINKEDIN_SESSION_COOKIE") {
		t.Fatalf("expected missing-config MCP error, result=%+v err=%v", result, err)
	}
}

func TestLinkedInConfigErrorNamesExactlyTheMissingVariables(t *testing.T) {
	cases := []struct {
		name, mode, access, session string
		bootstrap                   bool
		wantMissing                 string
		wantText                    []string
		rejectText                  string
	}{
		{"auto missing both", linkedinModeAuto, "", "", false, "LINKEDIN_ACCESS_TOKEN,LINKEDIN_SESSION_COOKIE", []string{"LINKEDIN_ACCESS_TOKEN", "LINKEDIN_SESSION_COOKIE"}, ""},
		{"auto access token", linkedinModeAuto, "tok", "", false, "", nil, ""},
		{"auto session cookie", linkedinModeAuto, "", "li_at", false, "", nil, ""},
		{"auto bootstrap", linkedinModeAuto, "", "", true, "", nil, ""},
		{"official missing token", linkedinModeOfficial, "", "li_at", false, "LINKEDIN_ACCESS_TOKEN", []string{"LINKEDIN_ACCESS_TOKEN"}, "LINKEDIN_SESSION_COOKIE"},
		{"official with token", linkedinModeOfficial, "tok", "", false, "", nil, ""},
		{"experimental missing cookie", linkedinModeExperimental, "tok", "", false, "LINKEDIN_SESSION_COOKIE", []string{"LINKEDIN_SESSION_COOKIE"}, "LINKEDIN_ACCESS_TOKEN"},
		{"experimental bootstrap", linkedinModeExperimental, "", "", true, "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, err := linkedInConfigError(tc.mode, tc.access, tc.session, tc.bootstrap)
			if missing != tc.wantMissing {
				t.Fatalf("missing_env = %q, want %q", missing, tc.wantMissing)
			}
			if tc.wantMissing == "" {
				if err != nil {
					t.Fatalf("unexpected config error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a NotConfigured error")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q should name %s", err, want)
				}
			}
			if tc.rejectText != "" && strings.Contains(err.Error(), tc.rejectText) {
				t.Fatalf("error %q must not blame %s in %s mode", err, tc.rejectText, tc.mode)
			}
		})
	}
}

func unconfiguredLinkedInEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LINKEDIN_MODE", "LINKEDIN_ACCESS_TOKEN", "LINKEDIN_TOKEN",
		"LINKEDIN_SESSION_COOKIE", "LINKEDIN_LI_AT", "LI_AT", "LINKEDIN_JSESSIONID", "JSESSIONID",
		"LINKEDIN_LOGIN_USERNAME", "LINKEDIN_USERNAME", "LINKEDIN_LOGIN_PASSWORD", "LINKEDIN_PASSWORD",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("LINKEDIN_BROWSERKIT_MODE", linkedInBrowserKitModeOff)
	t.Setenv("LINKEDIN_BROWSERKIT_STORAGE_DIR", t.TempDir())
}

func newTestLinkedInMCPServer(t *testing.T) *mcp.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tp, shutdown, err := mcpotel.InitTracer(context.Background(), "mcp-linkedin-test", logger)
	if err != nil {
		t.Fatalf("tracer: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	server, err := newLinkedInServer(logger, mcpotel.Tracer(tp, "mcp-linkedin-test"), nil)
	if err != nil {
		t.Fatalf("server must start without credentials: %v", err)
	}
	return server
}

// TestLinkedInServesHandshakeWithoutCredentials pins the degraded-start
// contract at the server level: with no credential set the server answers
// initialize and tools/list, and a credential-bound tool call returns an
// MCP error naming both variables, without exiting the process.
func TestLinkedInServesHandshakeWithoutCredentials(t *testing.T) {
	unconfiguredLinkedInEnv(t)
	server := newTestLinkedInMCPServer(t)

	res := mcptest.Drive(t, server, "linkedin_get_profile", map[string]any{})
	if names := mcptest.ToolNames(t, res.ToolsList); len(names) == 0 {
		t.Fatal("tools/list must not be empty while unconfigured")
	}
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "LINKEDIN_ACCESS_TOKEN") || !strings.Contains(text, "LINKEDIN_SESSION_COOKIE") {
		t.Fatalf("expected NotConfigured naming both credentials, got isError=%v text=%q", result.IsError, text)
	}
}

// TestLinkedInOfficialModeNamesOnlyTheAccessToken pins the per-mode verdict
// through the real tool surface.
func TestLinkedInOfficialModeNamesOnlyTheAccessToken(t *testing.T) {
	unconfiguredLinkedInEnv(t)
	t.Setenv("LINKEDIN_MODE", linkedinModeOfficial)
	server := newTestLinkedInMCPServer(t)

	res := mcptest.Drive(t, server, "linkedin_get_profile", map[string]any{})
	result := mcptest.ToolResult(t, res.ToolCall)
	text := mcptest.Text(result)
	if !result.IsError || !strings.Contains(text, "LINKEDIN_ACCESS_TOKEN") || strings.Contains(text, "LINKEDIN_SESSION_COOKIE") {
		t.Fatalf("expected NotConfigured naming only LINKEDIN_ACCESS_TOKEN, got isError=%v text=%q", result.IsError, text)
	}
}

type mockSecretStore struct {
	values map[string]string
	err    error
}

func (m *mockSecretStore) Set(key, value string) error {
	if m.err != nil {
		return m.err
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[key] = value
	return nil
}

func newTestLinkedInServer(baseURL string) *linkedInServer {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &linkedInServer{
		baseURL:    baseURL,
		mode:       linkedinModeAuto,
		httpClient: httpclient.NewDefault(),
		logger:     logger,
		browserKit: linkedInBrowserKitConfig{
			mode:             linkedInBrowserKitModeAuto,
			python:           "python3",
			storageDir:       "/tmp",
			sessionID:        "test",
			healthTTL:        1 * time.Second,
			recoveryCooldown: 1 * time.Second,
		},
		lastSessionState: linkedInSessionStateUnknown,
		now:              time.Now,
	}
	s.browserKitRunner = func(_ context.Context, _ linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		return &linkedInBrowserKitResponse{
			OK:          true,
			State:       linkedInSessionStateHealthy,
			HasLIAt:     s.sessionToken != "",
			HasJSession: s.jsessionID != "",
			LIAt:        s.sessionToken,
			JSessionID:  s.jsessionID,
		}, nil
	}
	return s
}

func TestParseLinkedInMode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default auto", input: "", want: linkedinModeAuto},
		{name: "official", input: "official", want: linkedinModeOfficial},
		{name: "experimental", input: "experimental", want: linkedinModeExperimental},
		{name: "trim + case", input: "  AuTo  ", want: linkedinModeAuto},
		{name: "invalid", input: "bad-mode", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLinkedInMode(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mode=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseLinkedInMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLinkedInBrowserKitMode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default auto", input: "", want: linkedInBrowserKitModeAuto},
		{name: "required", input: "required", want: linkedInBrowserKitModeRequired},
		{name: "off", input: "off", want: linkedInBrowserKitModeOff},
		{name: "trim + case", input: "  AuTo  ", want: linkedInBrowserKitModeAuto},
		{name: "invalid", input: "bad-mode", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLinkedInBrowserKitMode(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mode=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseLinkedInBrowserKitMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsLinkedInAuthChallenge(t *testing.T) {
	if !isLinkedInAuthChallenge(http.StatusForbidden, []byte(`{"message":"checkpoint required"}`)) {
		t.Fatal("expected checkpoint payload to be detected as challenge")
	}
	if isLinkedInAuthChallenge(http.StatusTooManyRequests, []byte(`{"message":"challenge"}`)) {
		t.Fatal("did not expect 429 to be treated as auth challenge")
	}
}

func TestIsMessagingPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/messaging/conversations?start=0&count=1", want: true},
		{path: "/voyagerMessagingGraphQL/graphql?queryId=abc", want: true},
		{path: "/voyagerMessagingDash/conversations?foo=bar", want: true},
		{path: "/voyager/api/me", want: false},
	}
	for _, tt := range tests {
		if got := isMessagingPath(tt.path); got != tt.want {
			t.Fatalf("isMessagingPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNormalizeJSessionID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"quoted", "\"ajax:123\"", "\"ajax:123\""},
		{"unquoted", "ajax:123", "\"ajax:123\""},
		{"trim", "  ajax:123  ", "\"ajax:123\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeJSessionID(tt.input)
			if got != tt.want {
				t.Fatalf("normalizeJSessionID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestReadStringSliceArg(t *testing.T) {
	got := readStringSliceArg([]any{" urn:1 ", 12, "", "urn:2"})
	if len(got) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(got))
	}
	if got[0] != "urn:1" || got[1] != "urn:2" {
		t.Fatalf("unexpected values: %#v", got)
	}
}

func TestHandleGetConversationMessages_MissingConversationURN(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.sessionToken = "cookie"

	result, err := s.handleGetConversationMessages(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected MCP error result")
	}
}

func TestHandleSendMessage_RequiresTarget(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeExperimental
	s.sessionToken = "cookie"
	s.jsessionID = "ajax:123"

	result, err := s.handleSendMessage(context.Background(), map[string]any{
		"text": "hello",
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected MCP error result")
	}
}

func TestHandleAuthStatus(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeAuto
	s.accessToken = "token-123"
	s.sessionToken = "cookie-123"
	s.jsessionID = "ajax:123"

	result, err := s.handleAuthStatus(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "browserkit:") {
		t.Fatalf("expected browserkit block in response, got: %+v", result.Content)
	}
}

func TestHandleSessionRecover_BrowserKitOff(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.browserKit.mode = linkedInBrowserKitModeOff

	result, err := s.handleSessionRecover(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected MCP error result")
	}
}

func TestEnsureMessagingAllowed_AllowsCredentialBootstrap(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeExperimental
	s.sessionToken = ""
	s.loginUsername = "user@example.com"
	s.loginPassword = "password"
	s.browserKit.mode = linkedInBrowserKitModeAuto

	if err := s.ensureMessagingAllowed(); err != nil {
		t.Fatalf("expected credential bootstrap to allow messaging, got: %v", err)
	}
}

func TestEnsureFreshSession_BootstrapsWithCredentials(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeExperimental
	s.sessionToken = ""
	s.jsessionID = ""
	s.loginUsername = "user@example.com"
	s.loginPassword = "password"
	s.browserKit.mode = linkedInBrowserKitModeAuto

	calls := 0
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		calls++
		if req.Action != "recover" {
			t.Fatalf("expected recover action, got %q", req.Action)
		}
		return &linkedInBrowserKitResponse{
			OK:          true,
			State:       linkedInSessionStateHealthy,
			HasLIAt:     true,
			HasJSession: true,
			LIAt:        "cookie-from-recovery",
			JSessionID:  "ajax:123",
		}, nil
	}

	if err := s.ensureFreshSession(context.Background()); err != nil {
		t.Fatalf("ensureFreshSession returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one recovery call, got %d", calls)
	}
	if s.sessionToken != "cookie-from-recovery" {
		t.Fatalf("expected session token update from recovery, got %q", s.sessionToken)
	}
	if s.jsessionID != "ajax:123" {
		t.Fatalf("expected jsession update from recovery, got %q", s.jsessionID)
	}
}

func TestRecoverSessionPersistsSecrets(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	store := &mockSecretStore{values: map[string]string{}}
	s.secretStore = store
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		if req.Action != "recover" {
			t.Fatalf("expected recover action, got %q", req.Action)
		}
		return &linkedInBrowserKitResponse{
			OK:          true,
			State:       linkedInSessionStateHealthy,
			HasLIAt:     true,
			HasJSession: true,
			LIAt:        "new-li-at",
			JSessionID:  "ajax:new",
		}, nil
	}

	res, err := s.recoverSession(context.Background(), linkedInRecoveryModeSilent, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.State != linkedInSessionStateHealthy {
		t.Fatalf("expected healthy state, got %q", res.State)
	}
	if got := store.values["LINKEDIN_SESSION_COOKIE"]; got != "new-li-at" {
		t.Fatalf("expected secret LINKEDIN_SESSION_COOKIE, got %q", got)
	}
	if got := store.values["LINKEDIN_JSESSIONID"]; got != "ajax:new" {
		t.Fatalf("expected secret LINKEDIN_JSESSIONID, got %q", got)
	}
	if s.sessionToken != "new-li-at" {
		t.Fatalf("expected in-memory session token to update")
	}
	if s.jsessionID != "ajax:new" {
		t.Fatalf("expected in-memory jsession to update")
	}
}

func TestRecoverSessionCooldown(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.browserKit.recoveryCooldown = 10 * time.Second
	now := time.Date(2026, 2, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.browserKitRunner = func(_ context.Context, _ linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		return &linkedInBrowserKitResponse{OK: true, State: linkedInSessionStateHealthy}, nil
	}

	if _, err := s.recoverSession(context.Background(), linkedInRecoveryModeSilent, false); err != nil {
		t.Fatalf("first recovery failed: %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := s.recoverSession(context.Background(), linkedInRecoveryModeSilent, false); err == nil {
		t.Fatal("expected cooldown error on second recovery")
	}
}

func TestBrowserKitHelperContractNonDestructiveSilentRecovery(t *testing.T) {
	// Pins the embedded helper's recovery contract: silent recovery must probe the
	// persisted session before clearing it, and any clear must reference a backup.
	for _, marker := range []string{
		"def _attempt_persisted_session(",
		"def _backup_storage_state(",
		"backed up persisted browser session state to",
		"silent recovery reused persisted browser session state",
		"persisted browser session state failed voyager probe",
		"persisted browser session state left in place: no backup available",
		"cleared persisted browser session state before recovery (backup:",
	} {
		if !strings.Contains(browserKitHelperPy, marker) {
			t.Errorf("helper contract marker missing: %q", marker)
		}
	}
	if strings.Contains(browserKitHelperPy, `cleared persisted browser session state before recovery"`) {
		t.Error("helper clears persisted session state without referencing a backup")
	}
}

func TestBrowserKitHelperRecoverBacksUpStorageStateBeforeClearing(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := t.TempDir()
	helperPath := filepath.Join(dir, "browserkit_helper.py")
	if err := os.WriteFile(helperPath, []byte(browserKitHelperPy), 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	storageDir := filepath.Join(dir, "storage")
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		t.Fatalf("mkdir storage dir: %v", err)
	}
	statePath := filepath.Join(storageDir, "primary.json")
	stateContent := []byte(`{"cookies":[{"name":"li_at","value":"still-valid"}]}`)
	if err := os.WriteFile(statePath, stateContent, 0o600); err != nil {
		t.Fatalf("write storage state: %v", err)
	}

	payload, err := json.Marshal(linkedInBrowserKitRequest{
		Action:     "recover",
		Mode:       linkedInRecoveryModeSilent,
		URL:        "https://www.linkedin.com/feed/",
		StorageDir: storageDir,
		SessionID:  "primary",
		Stealth:    false,
		Headless:   true,
		TimeoutMS:  1000,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	// -I -S isolates the interpreter from site-packages so the browser_kit import
	// fails deterministically right after the backup step this test exercises.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, runErr := exec.CommandContext(ctx, python, "-I", "-S", helperPath, string(payload)).Output()
	line := lastNonEmptyLine(string(stdout))
	if strings.TrimSpace(line) == "" {
		t.Fatalf("helper produced no stdout (run error: %v)", runErr)
	}
	var out linkedInBrowserKitResponse
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("parse helper output %q: %v", line, err)
	}
	if out.OK {
		t.Fatalf("expected dependency-import failure payload, got ok=true: %q", line)
	}

	if got, err := os.ReadFile(statePath); err != nil || string(got) != string(stateContent) {
		t.Fatalf("persisted storage state must survive the backup step (err=%v content=%q)", err, got)
	}

	backups, err := filepath.Glob(filepath.Join(storageDir, "primary.json.*.bak"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected exactly one storage state backup, got %v", backups)
	}
	if got, err := os.ReadFile(backups[0]); err != nil || string(got) != string(stateContent) {
		t.Fatalf("backup content mismatch (err=%v content=%q)", err, got)
	}

	backupWarned := false
	for _, warning := range out.Warnings {
		if strings.Contains(warning, "backed up persisted browser session state to") &&
			strings.Contains(warning, filepath.Base(backups[0])) {
			backupWarned = true
		}
	}
	if !backupWarned {
		t.Fatalf("expected a warning naming backup %s, got %v", filepath.Base(backups[0]), out.Warnings)
	}
}

func TestRequestRetriesAfterChallenge(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"checkpoint challenge required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.mode = linkedinModeExperimental
	s.sessionToken = "li-at-cookie"
	s.jsessionID = "ajax:old"
	s.browserKit.recoveryCooldown = 0
	recoverCalls := 0
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		switch req.Action {
		case "voyager_request":
			return nil, &authChallengeError{statusCode: http.StatusForbidden, body: "checkpoint"}
		case "recover":
			recoverCalls++
			return &linkedInBrowserKitResponse{
				OK:          true,
				State:       linkedInSessionStateHealthy,
				HasLIAt:     true,
				HasJSession: true,
				LIAt:        "li-at-new",
				JSessionID:  "ajax:new",
			}, nil
		default:
			t.Fatalf("unexpected action %q", req.Action)
			return nil, nil
		}
	}

	_, err := s.requestJSON(context.Background(), http.MethodGet, "/messaging/conversations?start=0&count=1", nil)
	if err != nil {
		t.Fatalf("expected successful retry, got error: %v", err)
	}
	if recoverCalls != 1 {
		t.Fatalf("expected 1 recovery call, got %d", recoverCalls)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (challenge + retry), got %d", calls)
	}
}

func TestRequestFallsBackToBrowserKitTransportAfterRetryChallenge(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"checkpoint challenge required"}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.mode = linkedinModeExperimental
	s.sessionToken = "li-at-cookie"
	s.jsessionID = "ajax:old"
	s.browserKit.recoveryCooldown = 0
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		switch req.Action {
		case "recover":
			return &linkedInBrowserKitResponse{
				OK:          true,
				State:       linkedInSessionStateHealthy,
				HasLIAt:     true,
				HasJSession: true,
				LIAt:        "li-at-new",
				JSessionID:  "ajax:new",
			}, nil
		case "voyager_request":
			return &linkedInBrowserKitResponse{
				OK:           true,
				State:        linkedInSessionStateHealthy,
				HTTPStatus:   http.StatusOK,
				ResponseJSON: map[string]any{"elements": []any{}},
			}, nil
		default:
			t.Fatalf("unexpected browserkit action %q", req.Action)
			return nil, nil
		}
	}

	data, err := s.requestJSON(context.Background(), http.MethodGet, "/messaging/conversations?start=0&count=1", nil)
	if err != nil {
		t.Fatalf("expected browserkit fallback success, got error: %v", err)
	}
	out, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object from fallback, got %#v", data)
	}
	if _, ok := out["elements"]; !ok {
		t.Fatalf("expected elements key in fallback response, got %#v", out)
	}
	if calls != 0 {
		t.Fatalf("expected browserkit primary transport to avoid HTTP calls, got %d", calls)
	}
}

func TestIsLinkedInSessionInvalidation(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusFound,
		Header: http.Header{
			"Set-Cookie":      []string{"li_at=delete me; Max-Age=0; Path=/"},
			"Clear-Site-Data": []string{`"storage"`},
			"Location":        []string{"https://www.linkedin.com/login"},
		},
	}
	if !isLinkedInSessionInvalidation(resp, nil) {
		t.Fatal("expected invalidation response to be detected")
	}
}

func TestSanitizeBrowserKitMessage_RedirectLoop(t *testing.T) {
	raw := "Error: apiRequestContext.fetch: Max redirect count exceeded.\nCall log:\n  - GET https://www.linkedin.com/voyager/api/messaging/conversations"
	got := sanitizeBrowserKitMessage(raw)
	want := "voyager request redirect loop (max redirect count exceeded)"
	if got != want {
		t.Fatalf("sanitizeBrowserKitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeBrowserKitWarnings_Deduplicates(t *testing.T) {
	warnings := []string{
		"  warning one  ",
		"warning one",
		"",
		"warning two",
		"playwright-stealth not installed; skipping stealth patches",
	}
	got := sanitizeBrowserKitWarnings(warnings)
	if len(got) != 2 {
		t.Fatalf("expected 2 warnings after dedupe, got %d (%#v)", len(got), got)
	}
	if got[0] != "warning one" || got[1] != "warning two" {
		t.Fatalf("unexpected warning set: %#v", got)
	}
}

func TestDoRequest_RedirectLoopClassifiedAsChallenge(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	_, err := s.doRequest(context.Background(), http.MethodGet, "/loop", nil)
	if err == nil {
		t.Fatal("expected error for redirect loop")
	}
	if !isAuthChallengeErr(err) {
		t.Fatalf("expected auth challenge error, got: %v", err)
	}
}

func TestRequestChallengeNoRetryOutsideMessaging(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"checkpoint challenge required"}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.mode = linkedinModeExperimental
	s.sessionToken = "li-at-cookie"
	s.jsessionID = "ajax:old"
	s.browserKitRunner = func(_ context.Context, _ linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		return &linkedInBrowserKitResponse{OK: true, State: linkedInSessionStateHealthy}, nil
	}

	_, err := s.requestJSON(context.Background(), http.MethodGet, "/me", nil)
	if err == nil {
		t.Fatal("expected error for non-messaging challenge")
	}
	if calls != 1 {
		t.Fatalf("expected no retry for non-messaging path, got %d calls", calls)
	}
}

func TestMessagingConversationsPath_UsesBrowserKitMailboxLookup(t *testing.T) {
	httpCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"unexpected http /me call"}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.mode = linkedinModeExperimental
	s.conversationsQID = defaultMessengerConversationsQueryID
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		if req.Action != "voyager_request" {
			t.Fatalf("unexpected action: %q", req.Action)
		}
		if req.Path != "/me" {
			t.Fatalf("unexpected path: %q", req.Path)
		}
		return &linkedInBrowserKitResponse{
			OK:         true,
			State:      linkedInSessionStateHealthy,
			HTTPStatus: http.StatusOK,
			ResponseJSON: map[string]any{
				"miniProfile": map[string]any{
					"dashEntityUrn": "urn:li:fsd_profile:testMailbox",
				},
			},
		}, nil
	}

	path, err := s.messagingConversationsPath(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(path, "queryId="+defaultMessengerConversationsQueryID) {
		t.Fatalf("unexpected query id in path: %q", path)
	}
	if !strings.Contains(path, "mailboxUrn:urn%3Ali%3Afsd_profile%3AtestMailbox") {
		t.Fatalf("mailbox URN was not URL-encoded in path: %q", path)
	}
	if httpCalls != 0 {
		t.Fatalf("expected browserkit mailbox lookup to avoid HTTP /me call, got %d HTTP calls", httpCalls)
	}
}

func TestMessagingConversationsPath_FallsBackToHTTPMailboxLookup(t *testing.T) {
	httpCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if r.URL.Path != "/me" {
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"miniProfile":{"dashEntityUrn":"urn:li:fsd_profile:httpMailbox"}}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.mode = linkedinModeExperimental
	s.conversationsQID = defaultMessengerConversationsQueryID
	s.browserKitRunner = func(_ context.Context, req linkedInBrowserKitRequest) (*linkedInBrowserKitResponse, error) {
		if req.Action == "voyager_request" && req.Path == "/me" {
			return nil, fmt.Errorf("browserkit /me failed")
		}
		return &linkedInBrowserKitResponse{
			OK:          true,
			State:       linkedInSessionStateHealthy,
			HasLIAt:     s.sessionToken != "",
			HasJSession: s.jsessionID != "",
			LIAt:        s.sessionToken,
			JSessionID:  s.jsessionID,
		}, nil
	}

	path, err := s.messagingConversationsPath(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(path, "mailboxUrn:urn%3Ali%3Afsd_profile%3AhttpMailbox") {
		t.Fatalf("expected HTTP mailbox URN in path, got: %q", path)
	}
	if httpCalls != 1 {
		t.Fatalf("expected one HTTP /me fallback call, got %d", httpCalls)
	}
}

func TestFormatConversationsResult_CompactByDefault(t *testing.T) {
	raw := map[string]any{
		"data": map[string]any{
			"messengerConversationsBySyncToken": map[string]any{
				"elements": []any{
					map[string]any{
						"entityUrn":       "urn:li:msg_conversation:1",
						"backendUrn":      "urn:li:messagingThread:1",
						"conversationUrl": "https://www.linkedin.com/messaging/thread/1/",
						"unreadCount":     2,
						"read":            false,
						"lastActivityAt":  1771457331243.0,
						"categories":      []any{"INBOX", "PRIMARY_INBOX"},
						"conversationParticipants": []any{
							map[string]any{
								"entityUrn": "urn:li:msg_messagingParticipant:1",
								"participantType": map[string]any{
									"member": map[string]any{
										"firstName":  map[string]any{"text": "Ada"},
										"lastName":   map[string]any{"text": "Lovelace"},
										"profileUrl": "https://www.linkedin.com/in/ada",
										"headline":   map[string]any{"text": "Engineer"},
									},
								},
							},
						},
						"messages": map[string]any{
							"elements": []any{
								map[string]any{
									"entityUrn":   "urn:li:msg_message:1",
									"body":        map[string]any{"text": "hello from latest message"},
									"deliveredAt": 1771457331243.0,
									"sender":      map[string]any{"entityUrn": "urn:li:msg_messagingParticipant:1"},
								},
							},
						},
					},
				},
			},
			"metadata": map[string]any{"newSyncToken": "token-123"},
		},
	}

	out := formatConversationsResult(raw, 0, 20, false, "graphql")
	if _, ok := out["raw"]; ok {
		t.Fatal("did not expect raw payload when include_raw=false")
	}

	items, ok := out["conversations"].([]map[string]any)
	if !ok {
		t.Fatalf("expected []map for conversations, got %T", out["conversations"])
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(items))
	}
	if items[0]["conversation_urn"] != "urn:li:msg_conversation:1" {
		t.Fatalf("unexpected conversation_urn: %#v", items[0]["conversation_urn"])
	}
	if items[0]["unread_count"] != 2 {
		t.Fatalf("unexpected unread_count: %#v", items[0]["unread_count"])
	}
}

func TestFormatConversationMessagesResult_IncludeRawAndPaginate(t *testing.T) {
	raw := map[string]any{
		"data": map[string]any{
			"messengerMessagesBySyncToken": map[string]any{
				"elements": []any{
					map[string]any{
						"entityUrn":   "urn:li:msg_message:1",
						"body":        map[string]any{"text": "first"},
						"deliveredAt": 1.0,
					},
					map[string]any{
						"entityUrn":   "urn:li:msg_message:2",
						"body":        map[string]any{"text": "second"},
						"deliveredAt": 2.0,
					},
				},
			},
			"metadata": map[string]any{"newSyncToken": "sync-1"},
		},
	}

	out := formatConversationMessagesResult(raw, "urn:li:msg_conversation:abc", 0, 1, true, "graphql")
	if _, ok := out["raw"]; !ok {
		t.Fatal("expected raw payload when include_raw=true")
	}
	if out["sync_token"] != "sync-1" {
		t.Fatalf("unexpected sync_token: %#v", out["sync_token"])
	}

	items, ok := out["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("expected []map for messages, got %T", out["messages"])
	}
	if len(items) != 1 {
		t.Fatalf("expected paginated message count 1, got %d", len(items))
	}
	if items[0]["message_urn"] != "urn:li:msg_message:1" {
		t.Fatalf("unexpected first message_urn: %#v", items[0]["message_urn"])
	}
}

func TestFormatProfileResult_CompactByDefault(t *testing.T) {
	raw := map[string]any{
		"entityUrn": "urn:li:fsd_profile:123",
		"occupation": map[string]any{
			"text": "Software Engineer",
		},
		"miniProfile": map[string]any{
			"firstName":        map[string]any{"text": "Ada"},
			"lastName":         map[string]any{"text": "Lovelace"},
			"headline":         map[string]any{"text": "Builder"},
			"publicIdentifier": "ada-lovelace",
			"profileUrl":       "https://www.linkedin.com/in/ada-lovelace",
			"dashEntityUrn":    "urn:li:fsd_profile:123",
		},
	}

	out := formatProfileResult(raw, false)
	if _, ok := out["raw"]; ok {
		t.Fatal("did not expect raw payload when include_raw=false")
	}
	profile, ok := out["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected profile object, got %T", out["profile"])
	}
	if profile["full_name"] != "Ada Lovelace" {
		t.Fatalf("unexpected full_name: %#v", profile["full_name"])
	}
	if profile["public_identifier"] != "ada-lovelace" {
		t.Fatalf("unexpected public_identifier: %#v", profile["public_identifier"])
	}
}

func TestFormatSendMessageResult_IncludeRawToggle(t *testing.T) {
	raw := map[string]any{
		"event": map[string]any{
			"entityUrn": "urn:li:msg_message:1",
			"createdAt": 1771457331243.0,
			"conversation": map[string]any{
				"entityUrn": "urn:li:msg_conversation:abc",
			},
		},
	}

	out := formatSendMessageResult(raw, "", []string{"urn:li:fs_miniProfile:abc"}, "hello world", "subject", false)
	if _, ok := out["raw"]; ok {
		t.Fatal("did not expect raw payload when include_raw=false")
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", out["result"])
	}
	if result["conversation_urn"] != "urn:li:msg_conversation:abc" {
		t.Fatalf("unexpected conversation_urn: %#v", result["conversation_urn"])
	}
	if result["event_urn"] != "urn:li:msg_message:1" {
		t.Fatalf("unexpected event_urn: %#v", result["event_urn"])
	}

	withRaw := formatSendMessageResult(raw, "", nil, "hello world", "", true)
	if _, ok := withRaw["raw"]; !ok {
		t.Fatal("expected raw payload when include_raw=true")
	}
}

func TestHandleListConversations_OfficialModeBlocked(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeOfficial
	s.accessToken = "token-123"

	result, err := s.handleListConversations(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected MCP error result")
	}
}

func TestHandleSendMessage_RequiresJSessionID(t *testing.T) {
	s := newTestLinkedInServer("http://example.com")
	s.mode = linkedinModeExperimental
	s.sessionToken = "cookie-123"

	result, err := s.handleSendMessage(context.Background(), map[string]any{
		"text":       "hello",
		"recipients": []any{"urn:li:fs_miniProfile:abc"},
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected MCP error result")
	}
}

func TestRequestJSON_SetsBearerToken(t *testing.T) {
	var gotAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.accessToken = "token-123"

	_, err := s.requestJSON(context.Background(), http.MethodGet, "/me", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer token-123" {
		t.Fatalf("expected bearer token header, got %q", gotAuth)
	}
}

func TestRequestJSON_SetsCookieAndCSRFHeaders(t *testing.T) {
	var gotCookie string
	var gotCSRF string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotCSRF = r.Header.Get("csrf-token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.sessionToken = "li-at-cookie"
	s.jsessionID = "ajax:123"

	_, err := s.requestJSON(context.Background(), http.MethodGet, "/me", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotCookie, "li_at=li-at-cookie") {
		t.Fatalf("expected li_at cookie, got %q", gotCookie)
	}
	if !strings.Contains(gotCookie, "JSESSIONID=\"ajax:123\"") {
		t.Fatalf("expected quoted JSESSIONID cookie, got %q", gotCookie)
	}
	if gotCSRF != "ajax:123" {
		t.Fatalf("expected csrf-token header, got %q", gotCSRF)
	}
}

func TestCookieHeaderMergesBundleAndOverrides(t *testing.T) {
	s := newTestLinkedInServer("http://unused")
	s.sessionToken = "li-at-fresh"
	s.jsessionID = "ajax:fresh"
	s.cookieBundle = `bcookie="v=2&abc"; bscookie="v=1&def"; li_at=li-at-stale; lidc="b=OB74"`

	header := s.cookieHeader()
	if !strings.Contains(header, `bcookie="v=2&abc"`) {
		t.Fatalf("expected bundle bcookie in header, got %q", header)
	}
	if !strings.Contains(header, `lidc="b=OB74"`) {
		t.Fatalf("expected bundle lidc in header, got %q", header)
	}
	if !strings.Contains(header, "li_at=li-at-fresh") {
		t.Fatalf("expected dedicated li_at to win, got %q", header)
	}
	if strings.Contains(header, "li-at-stale") {
		t.Fatalf("stale bundle li_at must be overridden, got %q", header)
	}
	if !strings.Contains(header, `JSESSIONID="ajax:fresh"`) {
		t.Fatalf("expected quoted JSESSIONID, got %q", header)
	}
	if strings.Count(header, "li_at=") != 1 {
		t.Fatalf("expected exactly one li_at, got %q", header)
	}
}

func TestCookieHeaderEmptyWithoutSessionToken(t *testing.T) {
	s := newTestLinkedInServer("http://unused")
	s.sessionToken = ""
	s.cookieBundle = `bcookie="v=2&abc"`
	if header := s.cookieHeader(); header != "" {
		t.Fatalf("expected empty cookie header without session token, got %q", header)
	}
}

func TestCookieHeaderWithoutBundleMatchesLegacyShape(t *testing.T) {
	s := newTestLinkedInServer("http://unused")
	s.sessionToken = "li-at-cookie"
	s.jsessionID = `"ajax:123"`
	want := `li_at=li-at-cookie; JSESSIONID="ajax:123"`
	if header := s.cookieHeader(); header != want {
		t.Fatalf("expected %q, got %q", want, header)
	}
}

func TestDoRequestSendsBrowserFidelityHeaders(t *testing.T) {
	var gotUA, gotLang, gotCookie string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotLang = r.Header.Get("Accept-Language")
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.sessionToken = "li-at-cookie"
	s.jsessionID = "ajax:123"
	s.cookieBundle = `bcookie="v=2&abc"`

	if _, err := s.doRequest(context.Background(), http.MethodGet, "/me", nil); err != nil {
		t.Fatalf("doRequest failed: %v", err)
	}
	if gotUA != defaultLinkedInHTTPUserAgent {
		t.Fatalf("expected default browser user agent, got %q", gotUA)
	}
	if gotLang != "en-US,en;q=0.9" {
		t.Fatalf("expected accept-language header, got %q", gotLang)
	}
	if !strings.Contains(gotCookie, `bcookie="v=2&abc"`) || !strings.Contains(gotCookie, "li_at=li-at-cookie") {
		t.Fatalf("expected merged cookie header, got %q", gotCookie)
	}
}

func TestDoRequestHonorsUserAgentOverride(t *testing.T) {
	var gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	s := newTestLinkedInServer(ts.URL)
	s.sessionToken = "li-at-cookie"
	s.userAgent = "custom-agent/1.0"

	if _, err := s.doRequest(context.Background(), http.MethodGet, "/me", nil); err != nil {
		t.Fatalf("doRequest failed: %v", err)
	}
	if gotUA != "custom-agent/1.0" {
		t.Fatalf("expected UA override, got %q", gotUA)
	}
}
