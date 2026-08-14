package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/pkg/secrets"
)

const (
	gitLabRotationStateVersion = 1
	gitLabRotationMaxResponse  = 1 << 20
)

type gitLabRotationToken struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	UserID    int      `json:"user_id"`
	Active    bool     `json:"active"`
	Revoked   bool     `json:"revoked"`
	ExpiresAt string   `json:"expires_at"`
	Token     string   `json:"token,omitempty"`
}

type gitLabRotationState struct {
	Version   int                   `json:"version"`
	APIURL    string                `json:"api_url"`
	CreatedAt time.Time             `json:"created_at"`
	Old       gitLabRotationToken   `json:"old"`
	New       gitLabRotationToken   `json:"new"`
	Targets   gitLabRotationTargets `json:"targets"`
}

type gitLabRotationTargets struct {
	EnvFile  string `json:"env_file,omitempty"`
	SOPSFile string `json:"sops_file,omitempty"`
	GitHost  string `json:"git_host,omitempty"`
}

type gitLabRotationOptions struct {
	APIURL    string
	CurlPath  string
	StateFile string
	EnvFile   string
	SOPSFile  string
	SOPSPath  string
	GitPath   string
	GitHost   string
}

type gitLabRotationClient struct {
	apiURL   string
	curlPath string
	http     *http.Client
}

func newGitLabRotationCmd(socketPath string) *cobra.Command {
	defaults := defaultGitLabRotationOptions()
	cmd := &cobra.Command{
		Use:   "rotate-gitlab",
		Short: "Rotate the shared GitLab PAT without exposing it or causing downtime",
		Long: `Perform a two-phase GitLab personal access token rotation.

The start phase creates an overlapping replacement, writes a mode-0600 recovery
state before changing consumers, updates the Loom Keychain entry, Git HTTPS
credential, optional env file, and optional SOPS GitOps Secret, then verifies the
new token. The old token remains active until finish is explicitly applied after
the GitOps rollout and agent checks are green. Token values are never printed.`,
	}

	cmd.AddCommand(
		newGitLabRotationStartCmd(socketPath, defaults),
		newGitLabRotationResumeCmd(socketPath, defaults),
		newGitLabRotationStatusCmd(defaults),
		newGitLabRotationFinishCmd(defaults),
	)
	return cmd
}

func defaultGitLabRotationOptions() gitLabRotationOptions {
	home, _ := os.UserHomeDir()
	apiURL := strings.TrimSuffix(os.Getenv("GITLAB_API_URL"), "/")
	if apiURL == "" {
		apiURL = "https://gitlab.flexinfer.ai/api/v4"
	}
	curlPath := ""
	gitPath := "git"
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/curl"); err == nil {
			curlPath = "/usr/bin/curl"
		}
		if _, err := os.Stat("/usr/bin/git"); err == nil {
			gitPath = "/usr/bin/git"
		}
	}
	return gitLabRotationOptions{
		APIURL:    apiURL,
		CurlPath:  curlPath,
		StateFile: filepath.Join(home, ".config", "loom", "recovery", "gitlab-rotation.json"),
		EnvFile:   filepath.Join(home, ".config", "secrets", "ai.env"),
		SOPSPath:  "sops",
		GitPath:   gitPath,
		GitHost:   "gitlab.flexinfer.ai",
	}
}

func addGitLabRotationFlags(cmd *cobra.Command, opts *gitLabRotationOptions, includeTargets bool) {
	cmd.Flags().StringVar(&opts.APIURL, "api-url", opts.APIURL, "GitLab API base URL")
	cmd.Flags().StringVar(&opts.CurlPath, "curl-path", opts.CurlPath, "curl binary for secret-safe API calls (empty uses native HTTP)")
	cmd.Flags().StringVar(&opts.StateFile, "state-file", opts.StateFile, "mode-0600 recovery state path")
	if includeTargets {
		cmd.Flags().StringVar(&opts.EnvFile, "env-file", opts.EnvFile, "env file whose GitLab aliases are updated atomically (empty disables)")
		cmd.Flags().StringVar(&opts.SOPSFile, "sops-file", opts.SOPSFile, "SOPS Secret file to update (empty disables)")
		cmd.Flags().StringVar(&opts.SOPSPath, "sops-path", opts.SOPSPath, "sops executable")
		cmd.Flags().StringVar(&opts.GitPath, "git-path", opts.GitPath, "git executable used to update the configured credential helper")
		cmd.Flags().StringVar(&opts.GitHost, "git-host", opts.GitHost, "Git host for the HTTPS credential")
	}
}

func newGitLabRotationStartCmd(socketPath string, defaults gitLabRotationOptions) *cobra.Command {
	opts := defaults
	var apply bool
	var replacementName string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Create and install an overlapping replacement PAT",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGitLabRotationStart(cmd.Context(), socketPath, opts, replacementName, apply)
		},
	}
	addGitLabRotationFlags(cmd, &opts, true)
	cmd.Flags().BoolVar(&apply, "apply", false, "Create and install the replacement token")
	cmd.Flags().StringVar(&replacementName, "name", "", "Replacement token name (default: current name plus UTC timestamp)")
	return cmd
}

func newGitLabRotationResumeCmd(socketPath string, defaults gitLabRotationOptions) *cobra.Command {
	opts := defaults
	var apply bool
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume consumer installation from the recovery state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply {
				return printGitLabRotationStatus(opts.StateFile)
			}
			state, err := readGitLabRotationState(opts.StateFile)
			if err != nil {
				return err
			}
			state.Targets = gitLabRotationTargets{EnvFile: opts.EnvFile, SOPSFile: opts.SOPSFile, GitHost: opts.GitHost}
			if err := installGitLabRotationState(cmd.Context(), opts, &state); err != nil {
				return fmt.Errorf("resume rotation (state retained at %s): %w", opts.StateFile, err)
			}
			reloadDaemonAfterSecretChange(socketPath, "GitLab token rotation")
			fmt.Printf("Replacement PAT %d reinstalled and verified; old PAT %d remains active.\n", state.New.ID, state.Old.ID)
			return nil
		},
	}
	addGitLabRotationFlags(cmd, &opts, true)
	cmd.Flags().BoolVar(&apply, "apply", false, "Install consumers from the saved state")
	return cmd
}

func newGitLabRotationStatusCmd(defaults gitLabRotationOptions) *cobra.Command {
	opts := defaults
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show rotation metadata without revealing token values",
		RunE: func(_ *cobra.Command, _ []string) error {
			return printGitLabRotationStatus(opts.StateFile)
		},
	}
	addGitLabRotationFlags(cmd, &opts, false)
	return cmd
}

func newGitLabRotationFinishCmd(defaults gitLabRotationOptions) *cobra.Command {
	opts := defaults
	var apply bool
	cmd := &cobra.Command{
		Use:   "finish",
		Short: "Verify the replacement and revoke the old PAT",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGitLabRotationFinish(cmd.Context(), opts, apply)
		},
	}
	addGitLabRotationFlags(cmd, &opts, false)
	cmd.Flags().BoolVar(&apply, "apply", false, "Revoke the old token and remove recovery state")
	return cmd
}

func runGitLabRotationStart(ctx context.Context, socketPath string, opts gitLabRotationOptions, replacementName string, apply bool) error {
	if _, err := os.Stat(opts.StateFile); err == nil {
		return fmt.Errorf("rotation state already exists at %s; use resume, finish, or remove it after review", opts.StateFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect rotation state: %w", err)
	}
	client, err := newGitLabRotationClient(opts.APIURL, opts.CurlPath)
	if err != nil {
		return err
	}
	current, source, err := currentGitLabRotationToken(ctx)
	if err != nil {
		return err
	}
	old, err := client.self(ctx, current)
	if err != nil {
		return fmt.Errorf("verify current GitLab token from %s: %w", source, err)
	}
	if !old.Active || old.Revoked || old.ID <= 0 || old.UserID <= 0 || len(old.Scopes) == 0 {
		return fmt.Errorf("current GitLab token metadata is not safe to rotate")
	}
	printGitLabTokenSummary("Current", old, source)
	fmt.Printf("Targets: Keychain=loom/%s GitHTTPS=%s env=%s sops=%s\n",
		"GITLAB_PERSONAL_ACCESS_TOKEN", opts.GitHost, optionalPath(opts.EnvFile), optionalPath(opts.SOPSFile))
	if !apply {
		fmt.Println("Dry run complete; no token or consumer was changed. Re-run with --apply to start the overlap window.")
		return nil
	}
	if replacementName == "" {
		replacementName = old.Name + "-r" + time.Now().UTC().Format("20060102-150405")
	}
	created, err := client.createReplacement(ctx, current, old, replacementName)
	if err != nil {
		return err
	}
	state := gitLabRotationState{
		Version:   gitLabRotationStateVersion,
		APIURL:    opts.APIURL,
		CreatedAt: time.Now().UTC(),
		Old:       old,
		New:       created,
		Targets: gitLabRotationTargets{
			EnvFile:  opts.EnvFile,
			SOPSFile: opts.SOPSFile,
			GitHost:  opts.GitHost,
		},
	}
	if err := writeGitLabRotationState(opts.StateFile, state); err != nil {
		return fmt.Errorf("replacement PAT %d was created but recovery state could not be written: %w", created.ID, err)
	}
	if err := installGitLabRotationState(ctx, opts, &state); err != nil {
		return fmt.Errorf("replacement PAT %d is active; state retained at %s: %w", created.ID, opts.StateFile, err)
	}
	reloadDaemonAfterSecretChange(socketPath, "GitLab token rotation")
	fmt.Printf("Replacement PAT %d installed and verified. Old PAT %d remains active for zero-downtime rollout.\n", created.ID, old.ID)
	fmt.Printf("Recovery state: %s (mode 0600). Run finish --apply only after GitOps and agent verification.\n", opts.StateFile)
	return nil
}

func runGitLabRotationFinish(ctx context.Context, opts gitLabRotationOptions, apply bool) error {
	state, err := readGitLabRotationState(opts.StateFile)
	if err != nil {
		return err
	}
	client, err := newGitLabRotationClient(state.APIURL, opts.CurlPath)
	if err != nil {
		return err
	}
	newInfo, err := client.self(ctx, state.New.Token)
	if err != nil {
		return fmt.Errorf("replacement token verification failed; old token was not revoked: %w", err)
	}
	if newInfo.ID != state.New.ID || !newInfo.Active || newInfo.Revoked {
		return fmt.Errorf("replacement token identity/state mismatch; old token was not revoked")
	}
	oldInfo, err := client.get(ctx, state.New.Token, state.Old.ID)
	if err != nil {
		return fmt.Errorf("inspect old token %d: %w", state.Old.ID, err)
	}
	printGitLabTokenSummary("Replacement", newInfo, "rotation state")
	printGitLabTokenSummary("Old", oldInfo, "GitLab")
	if oldInfo.Revoked || !oldInfo.Active {
		fmt.Printf("Old PAT %d is already inactive.\n", oldInfo.ID)
		if apply {
			return removeGitLabRotationState(opts.StateFile)
		}
		return nil
	}
	if !apply {
		fmt.Printf("Dry run complete; old PAT %d is still active. Re-run with --apply after rollout verification.\n", oldInfo.ID)
		return nil
	}
	if err := client.revoke(ctx, state.New.Token, state.Old.ID); err != nil {
		return fmt.Errorf("revoke old PAT %d (state retained): %w", state.Old.ID, err)
	}
	oldInfo, err = client.get(ctx, state.New.Token, state.Old.ID)
	if err != nil {
		return fmt.Errorf("old PAT revoke returned success but verification failed (state retained): %w", err)
	}
	if !oldInfo.Revoked || oldInfo.Active {
		return fmt.Errorf("old PAT %d did not become inactive (state retained)", state.Old.ID)
	}
	if err := removeGitLabRotationState(opts.StateFile); err != nil {
		return err
	}
	fmt.Printf("Old PAT %d revoked; replacement PAT %d remains active. Recovery state removed.\n", state.Old.ID, state.New.ID)
	return nil
}

func currentGitLabRotationToken(ctx context.Context) (string, string, error) {
	if runtime.GOOS == "darwin" {
		if keychain, err := secrets.NewKeychainBackend(); err == nil {
			value, getErr := keychain.GetContext(ctx, "GITLAB_PERSONAL_ACCESS_TOKEN")
			if getErr == nil && value != "" {
				return value, "macOS Keychain", nil
			}
		}
	}
	mgr, err := secrets.DefaultManagerContext(ctx)
	if err != nil {
		return "", "", fmt.Errorf("initialize secret manager: %w", err)
	}
	value, source, err := mgr.GetContext(ctx, "GITLAB_PERSONAL_ACCESS_TOKEN")
	if err != nil || value == "" {
		return "", "", fmt.Errorf("GITLAB_PERSONAL_ACCESS_TOKEN is not available from a secret backend")
	}
	return value, source, nil
}

func installGitLabRotationState(ctx context.Context, opts gitLabRotationOptions, state *gitLabRotationState) error {
	if state == nil || state.New.Token == "" {
		return fmt.Errorf("rotation state has no replacement token")
	}
	keychain, err := secrets.NewKeychainBackend()
	if err != nil {
		return fmt.Errorf("open macOS Keychain: %w", err)
	}
	if err := keychain.SetContext(ctx, "GITLAB_PERSONAL_ACCESS_TOKEN", state.New.Token); err != nil {
		return fmt.Errorf("update canonical Keychain secret: %w", err)
	}
	for _, legacy := range []string{"GITLAB_PAT", "GITLAB_TOKEN"} {
		if err := keychain.DeleteContext(ctx, legacy); err != nil {
			return fmt.Errorf("remove legacy Keychain alias %s: %w", legacy, err)
		}
	}
	if opts.GitHost != "" {
		if err := storeGitHTTPSCredential(ctx, opts.GitPath, opts.GitHost, state.New.Token); err != nil {
			return err
		}
	}
	if opts.EnvFile != "" {
		if err := updateGitLabEnvFile(opts.EnvFile, state.New.Token); err != nil {
			return err
		}
	}
	if opts.SOPSFile != "" {
		if err := updateGitLabSOPSFile(ctx, opts.SOPSPath, opts.SOPSFile, state.New.Token); err != nil {
			return err
		}
	}
	client, err := newGitLabRotationClient(state.APIURL, opts.CurlPath)
	if err != nil {
		return err
	}
	verified, err := client.self(ctx, state.New.Token)
	if err != nil {
		return fmt.Errorf("verify installed replacement: %w", err)
	}
	if verified.ID != state.New.ID || !verified.Active || verified.Revoked {
		return fmt.Errorf("installed replacement identity/state mismatch")
	}
	state.Targets = gitLabRotationTargets{EnvFile: opts.EnvFile, SOPSFile: opts.SOPSFile, GitHost: opts.GitHost}
	return writeGitLabRotationState(opts.StateFile, *state)
}

func storeGitHTTPSCredential(ctx context.Context, gitPath, host, token string) error {
	if strings.ContainsAny(host, "\r\n") || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("git credential contains a newline")
	}
	cmd := exec.CommandContext(ctx, gitPath, "credential", "approve") //nolint:gosec // configured binary, fixed args
	cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\nusername=oauth2\npassword=" + token + "\n\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("store Git HTTPS credential: %s", sanitizeRotationText(stderr.String(), token))
	}
	return nil
}

func updateGitLabEnvFile(path, token string) error {
	if strings.ContainsAny(token, "\r\n'") {
		return fmt.Errorf("GitLab token contains unsupported env-file characters")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read env file %s: %w", path, err)
	}
	aliases := []string{"GITLAB_PERSONAL_ACCESS_TOKEN", "GITLAB_TOKEN", "GITLAB_PAT"}
	seen := make(map[string]bool, len(aliases))
	lines := strings.Split(strings.TrimSuffix(string(original), "\n"), "\n")
	updated := make([]string, 0, len(lines)+len(aliases))
	for _, line := range lines {
		key := envAssignmentKey(line)
		if slices.Contains(aliases, key) {
			if seen[key] {
				continue
			}
			updated = append(updated, "export "+key+"='"+token+"'")
			seen[key] = true
			continue
		}
		updated = append(updated, line)
	}
	for _, key := range aliases {
		if !seen[key] {
			updated = append(updated, "export "+key+"='"+token+"'")
		}
	}
	return atomicWriteFile(path, []byte(strings.Join(updated, "\n")+"\n"), 0o600)
}

func envAssignmentKey(line string) string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	idx := strings.IndexByte(trimmed, '=')
	if idx <= 0 {
		return ""
	}
	key := strings.TrimSpace(trimmed[:idx])
	for _, r := range key {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return key
}

func updateGitLabSOPSFile(ctx context.Context, sopsPath, path, token string) error {
	encoded, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("encode GitLab token for SOPS: %w", err)
	}
	set := exec.CommandContext(ctx, sopsPath, "set", "--value-stdin", path, `["stringData"]["GITLAB_PERSONAL_ACCESS_TOKEN"]`) //nolint:gosec // configured binary and file; secret is stdin, never argv
	set.Stdin = bytes.NewReader(encoded)
	var stderr bytes.Buffer
	set.Stderr = &stderr
	if err := set.Run(); err != nil {
		return fmt.Errorf("set GitLab token in SOPS file: %s", sanitizeRotationText(stderr.String(), token))
	}
	return nil
}

func writeGitLabRotationState(path string, state gitLabRotationState) error {
	if state.Version != gitLabRotationStateVersion || state.New.Token == "" {
		return fmt.Errorf("invalid GitLab rotation state")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rotation state: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, 0o600)
}

func readGitLabRotationState(path string) (gitLabRotationState, error) {
	var state gitLabRotationState
	info, err := os.Stat(path)
	if err != nil {
		return state, fmt.Errorf("read rotation state %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return state, fmt.Errorf("rotation state %s must not be accessible by group or others", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, fmt.Errorf("read rotation state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parse rotation state: %w", err)
	}
	if state.Version != gitLabRotationStateVersion || state.New.Token == "" || state.Old.ID <= 0 || state.New.ID <= 0 {
		return state, fmt.Errorf("rotation state is incomplete or unsupported")
	}
	return state, nil
}

func removeGitLabRotationState(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rotation state: %w", err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func newGitLabRotationClient(apiURL, curlPath string) (*gitLabRotationClient, error) {
	parsed, err := url.Parse(strings.TrimSuffix(apiURL, "/"))
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid GitLab API URL")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return nil, fmt.Errorf("GitLab API URL must use HTTPS (HTTP is allowed only for loopback tests)")
	}
	return &gitLabRotationClient{
		apiURL:   strings.TrimSuffix(apiURL, "/"),
		curlPath: curlPath,
		http:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *gitLabRotationClient) self(ctx context.Context, token string) (gitLabRotationToken, error) {
	return c.getPath(ctx, token, "/personal_access_tokens/self")
}

func (c *gitLabRotationClient) get(ctx context.Context, token string, id int) (gitLabRotationToken, error) {
	return c.getPath(ctx, token, "/personal_access_tokens/"+strconv.Itoa(id))
}

func (c *gitLabRotationClient) getPath(ctx context.Context, token, path string) (gitLabRotationToken, error) {
	status, body, err := c.request(ctx, http.MethodGet, path, token, nil)
	if err != nil {
		return gitLabRotationToken{}, err
	}
	if status != http.StatusOK {
		return gitLabRotationToken{}, rotationAPIError(status, body, token)
	}
	var info gitLabRotationToken
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("parse GitLab token metadata: %w", err)
	}
	info.Token = ""
	return info, nil
}

func (c *gitLabRotationClient) createReplacement(ctx context.Context, current string, old gitLabRotationToken, name string) (gitLabRotationToken, error) {
	payload := map[string]any{
		"name":        name,
		"description": "Zero-downtime replacement created by loom secrets rotate-gitlab",
		"expires_at":  old.ExpiresAt,
		"scopes":      old.Scopes,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return gitLabRotationToken{}, fmt.Errorf("marshal replacement request: %w", err)
	}
	status, response, err := c.request(ctx, http.MethodPost, "/users/"+strconv.Itoa(old.UserID)+"/personal_access_tokens", current, body)
	if err != nil {
		return gitLabRotationToken{}, err
	}
	if status != http.StatusCreated {
		return gitLabRotationToken{}, rotationAPIError(status, response, current)
	}
	var created gitLabRotationToken
	if err := json.Unmarshal(response, &created); err != nil {
		return created, fmt.Errorf("parse replacement response: %w", err)
	}
	if created.Token == "" || created.ID <= 0 || created.UserID != old.UserID || !created.Active || created.Revoked ||
		created.ExpiresAt != old.ExpiresAt || !sameStringSet(created.Scopes, old.Scopes) {
		return gitLabRotationToken{}, fmt.Errorf("GitLab returned an invalid or mismatched replacement token")
	}
	return created, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := slices.Clone(left)
	rightCopy := slices.Clone(right)
	slices.Sort(leftCopy)
	slices.Sort(rightCopy)
	return slices.Equal(leftCopy, rightCopy)
}

func (c *gitLabRotationClient) revoke(ctx context.Context, token string, id int) error {
	status, body, err := c.request(ctx, http.MethodDelete, "/personal_access_tokens/"+strconv.Itoa(id), token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return rotationAPIError(status, body, token)
	}
	return nil
}

func (c *gitLabRotationClient) request(ctx context.Context, method, path, token string, body []byte) (int, []byte, error) {
	if token == "" || strings.ContainsAny(token, "\r\n\"") {
		return 0, nil, fmt.Errorf("GitLab token is empty or contains unsupported characters")
	}
	if c.curlPath != "" {
		return c.requestCurl(ctx, method, path, token, body)
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("create GitLab request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GitLab request failed: %w", err)
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, gitLabRotationMaxResponse+1))
	if err != nil {
		return 0, nil, fmt.Errorf("read GitLab response: %w", err)
	}
	if len(response) > gitLabRotationMaxResponse {
		return 0, nil, fmt.Errorf("GitLab response exceeded %d bytes", gitLabRotationMaxResponse)
	}
	return resp.StatusCode, response, nil
}

func (c *gitLabRotationClient) requestCurl(ctx context.Context, method, path, token string, body []byte) (int, []byte, error) {
	tmpDir, err := os.MkdirTemp("", "loom-gitlab-rotation-*")
	if err != nil {
		return 0, nil, fmt.Errorf("create curl workspace: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return 0, nil, fmt.Errorf("secure curl workspace: %w", err)
	}
	configPath := filepath.Join(tmpDir, "curl.conf")
	responsePath := filepath.Join(tmpDir, "response.json")
	config := "silent\nshow-error\nmax-time = 15\nconnect-timeout = 5\nheader = \"Accept: application/json\"\nheader = \"PRIVATE-TOKEN: " + token + "\"\n"
	if len(body) > 0 {
		config += "header = \"Content-Type: application/json\"\n"
	}
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return 0, nil, fmt.Errorf("write secure curl config: %w", err)
	}
	args := []string{"--config", configPath, "--request", method, "--output", responsePath, "--write-out", "%{http_code}"}
	if len(body) > 0 {
		bodyPath := filepath.Join(tmpDir, "request.json")
		if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
			return 0, nil, fmt.Errorf("write curl request body: %w", err)
		}
		args = append(args, "--data-binary", "@"+bodyPath)
	}
	args = append(args, c.apiURL+path)
	cmd := exec.CommandContext(ctx, c.curlPath, args...) //nolint:gosec // configured binary; secret is in mode-0600 config, never argv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, fmt.Errorf("GitLab curl request failed: %s", sanitizeRotationText(stderr.String(), token))
	}
	status, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		return 0, nil, fmt.Errorf("parse GitLab HTTP status")
	}
	info, err := os.Stat(responsePath)
	if err != nil {
		return 0, nil, fmt.Errorf("stat GitLab response: %w", err)
	}
	if info.Size() > gitLabRotationMaxResponse {
		return 0, nil, fmt.Errorf("GitLab response exceeded %d bytes", gitLabRotationMaxResponse)
	}
	response, err := os.ReadFile(responsePath)
	if err != nil {
		return 0, nil, fmt.Errorf("read GitLab response: %w", err)
	}
	return status, response, nil
}

func rotationAPIError(status int, body []byte, token string) error {
	safe := sanitizeRotationText(string(body), token)
	if len(safe) > 512 {
		safe = safe[:512]
	}
	return fmt.Errorf("GitLab API returned HTTP %d: %s", status, strings.TrimSpace(safe))
}

func sanitizeRotationText(text, token string) string {
	if token != "" {
		text = strings.ReplaceAll(text, token, "[REDACTED]")
	}
	return text
}

func printGitLabRotationStatus(path string) error {
	state, err := readGitLabRotationState(path)
	if err != nil {
		return err
	}
	fmt.Printf("GitLab rotation state: %s\n", path)
	printGitLabTokenSummary("Old", state.Old, "state")
	printGitLabTokenSummary("Replacement", state.New, "state")
	fmt.Printf("Created: %s\n", state.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Targets: GitHTTPS=%s env=%s sops=%s\n", optionalPath(state.Targets.GitHost), optionalPath(state.Targets.EnvFile), optionalPath(state.Targets.SOPSFile))
	return nil
}

func printGitLabTokenSummary(label string, token gitLabRotationToken, source string) {
	fmt.Printf("%s PAT: id=%d name=%q user=%d scopes=%s expires=%s active=%t revoked=%t source=%s\n",
		label, token.ID, token.Name, token.UserID, strings.Join(token.Scopes, ","), token.ExpiresAt, token.Active, token.Revoked, source)
}

func optionalPath(value string) string {
	if value == "" {
		return "disabled"
	}
	return value
}
