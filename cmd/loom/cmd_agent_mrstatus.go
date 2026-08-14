package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// errSilent signals a non-zero process exit without cobra printing anything.
// The command already wrote its own JSON error object to stdout; this only
// drives the exit code (main.go os.Exit(1) on any non-nil RunE error).
var errSilent = errors.New("")

// mrStatusMR mirrors internal/hud/mrwatch.MergeRequest on the wire. It is
// duplicated here (rather than imported) to keep the CLI decoupled from the
// HUD daemon internals: the CLI only depends on the JSON contract of
// GET /api/agent/mr-status, not on the registry package.
type mrStatusMR struct {
	Repo             string    `json:"repo"`
	IID              int64     `json:"iid"`
	Title            string    `json:"title"`
	SourceBranch     string    `json:"source_branch"`
	TargetBranch     string    `json:"target_branch,omitempty"`
	State            string    `json:"state"`
	Reason           string    `json:"reason,omitempty"`
	WebURL           string    `json:"web_url,omitempty"`
	PipelineStatus   string    `json:"pipeline_status,omitempty"`
	PipelineURL      string    `json:"pipeline_url,omitempty"`
	LastTransitionAt time.Time `json:"last_transition_at"`
	Stale            bool      `json:"stale"`
}

// mrStatusResponse mirrors internal/hud/domain/mrwatch.BranchStatusResponse.
type mrStatusResponse struct {
	Branch        string       `json:"branch"`
	Repo          string       `json:"repo,omitempty"`
	MergeRequests []mrStatusMR `json:"merge_requests"`
	Count         int          `json:"count"`
	LastPollAt    time.Time    `json:"last_poll_at"`
	Stale         bool         `json:"stale"`
}

// newAgentMRStatusCmd creates the `loom agent mr-status` command.
//
// It reports the classified status of every open merge request whose source
// branch matches --branch (default: the current git branch of the cwd). The
// command is hook-safe: when the HUD is unreachable it exits 0 and prints
// NOTHING (so a UserPromptSubmit/BeforeAgent hook never breaks the agent
// flow) unless --json is set, in which case it emits a JSON error object and
// exits 1.
func newAgentMRStatusCmd() *cobra.Command {
	var (
		branch string
		repo   string
		asJSON bool
		brief  bool
		delta  bool
		hook   string
	)

	cmd := &cobra.Command{
		Use:   "mr-status",
		Short: "Show branch-MR awareness status (from the HUD mrwatch registry)",
		Long: `Report the classified status of every open merge request whose source
branch matches --branch (default: the current git branch of the cwd).

Output modes:
  (default)   human-readable, one block per MR
  --json      raw JSON from the HUD endpoint
  --brief     one line per MR: "!IID <state> <reason> <web_url>"
  --delta     with --brief, only print when the state hash differs from the
              cached hash under ~/.loom/mrwatch/<sanitized-branch>.state;
              exits 0 silently when unchanged (for delta-gated hooks)
  --hook <v>  emit a vendor context-injection hook envelope wrapping the
              gated brief body. v is "claude" (UserPromptSubmit) or
              "gemini" (BeforeAgent). Gating is two-stage: only MRs in an
              attention-worthy state (conflict, ci_failed_deterministic,
              ci_failed_flaky, automerge_unarmed, pipeline_skipped,
              stale_branch) are considered, and that set must have changed
              since the last injection. Always hook-safe: no-attention /
              unchanged / HUD-unreachable → print nothing, exit 0 (never a
              JSON error). See mrStatusHookEnvelope for the JSON contract.

Hook safety: when the HUD is unreachable this command exits 0 and prints
nothing, unless --json is set (then a JSON error object + exit 1).`,
		// The command writes its own JSON error object on --json failures;
		// silence cobra so it does not also print the error/usage.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			hookVendor := strings.TrimSpace(hook)
			if hookVendor != "" {
				// Hook mode is always hook-safe: it must never emit a JSON
				// error object or a non-zero exit, regardless of --json, or a
				// UserPromptSubmit/BeforeAgent hook would inject an error blob
				// or break the agent turn. Force the silent-exit-0 path.
				asJSON = false
			}

			b := strings.TrimSpace(branch)
			if b == "" {
				b = currentGitBranch(cmd.Context())
			}
			if b == "" {
				// No branch and not in a git repo: nothing to report.
				if asJSON {
					fmt.Println(`{"available":false,"error":"no branch specified and cwd is not a git repository"}`)
					return errSilent
				}
				return nil
			}

			port := resolvePort(cmd)
			path := "/api/agent/mr-status?branch=" + url.QueryEscape(b)
			if r := strings.TrimSpace(repo); r != "" {
				path += "&repo=" + url.QueryEscape(r)
			}

			raw, err := hudGet(port, path)
			if err != nil {
				// HUD unreachable / error. Hook-safe default: silent exit 0.
				if asJSON {
					fmt.Printf("{\"available\":false,\"branch\":%q,\"error\":%q}\n", b, err.Error())
					return errSilent
				}
				return nil
			}

			if asJSON {
				fmt.Println(strings.TrimRight(string(raw), "\n"))
				return nil
			}

			var resp mrStatusResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				// Malformed HUD payload: treat like unavailable in text mode.
				return nil
			}

			// --hook wraps the delta-gated brief body in a vendor
			// context-injection envelope. Checked before --delta/--brief
			// because it subsumes both (delta gating + a JSON envelope).
			if hookVendor != "" {
				return renderMRStatusHook(hookVendor, b, resp)
			}

			// --delta implies the brief rendering: it is the compact,
			// hook-injectable form the cache is keyed on.
			if delta {
				return renderMRStatusDelta(b, resp)
			}
			if brief {
				fmt.Print(renderMRStatusBrief(resp))
				return nil
			}
			fmt.Print(renderMRStatusHuman(resp))
			return nil
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "Source branch to query (default: current git branch of cwd)")
	cmd.Flags().StringVar(&repo, "repo", "", "Narrow to a single GitLab project path (optional)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSON from the HUD endpoint")
	cmd.Flags().BoolVar(&brief, "brief", false, "One line per MR: !IID <state> <reason> <web_url>")
	cmd.Flags().BoolVar(&delta, "delta", false, "With --brief: only print when the state hash changed (delta-gated hooks)")
	cmd.Flags().StringVar(&hook, "hook", "", "Emit a vendor context-injection hook envelope: \"claude\" (UserPromptSubmit) or \"gemini\" (BeforeAgent). Injects only attention-worthy MRs, and only when that set changed; always hook-safe.")

	return cmd
}

// currentGitBranch returns the current branch of the cwd, or "" if the cwd is
// not a git repository or is in a detached HEAD state.
func currentGitBranch(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" { // detached HEAD
		return ""
	}
	return branch
}

// renderMRStatusBrief renders one line per MR:
//
//	!IID <state> <reason> <web_url>
//
// The reason field collapses to "-" when empty so the columns stay aligned.
// Returns "" when there are no MRs.
func renderMRStatusBrief(resp mrStatusResponse) string {
	if len(resp.MergeRequests) == 0 {
		return ""
	}
	mrs := sortedMRs(resp.MergeRequests)
	var b strings.Builder
	for _, mr := range mrs {
		reason := mr.Reason
		if strings.TrimSpace(reason) == "" {
			reason = "-"
		}
		fmt.Fprintf(&b, "!%d %s %s %s\n", mr.IID, mr.State, reason, mr.WebURL)
	}
	return b.String()
}

// renderMRStatusHuman renders a readable multi-line summary.
func renderMRStatusHuman(resp mrStatusResponse) string {
	var b strings.Builder
	staleNote := ""
	if resp.Stale {
		staleNote = " (registry stale — serving last good snapshot)"
	}
	if len(resp.MergeRequests) == 0 {
		fmt.Fprintf(&b, "Branch %s: no open merge requests%s\n", resp.Branch, staleNote)
		return b.String()
	}
	fmt.Fprintf(&b, "Branch %s: %d open merge request(s)%s\n", resp.Branch, len(resp.MergeRequests), staleNote)
	for _, mr := range sortedMRs(resp.MergeRequests) {
		fmt.Fprintf(&b, "  !%d [%s] %s\n", mr.IID, mr.State, mr.Title)
		if strings.TrimSpace(mr.Reason) != "" {
			fmt.Fprintf(&b, "      reason: %s\n", mr.Reason)
		}
		if strings.TrimSpace(mr.WebURL) != "" {
			fmt.Fprintf(&b, "      %s\n", mr.WebURL)
		}
	}
	return b.String()
}

// renderMRStatusDelta prints the brief rendering only when its hash differs
// from the value cached under ~/.loom/mrwatch/<sanitized-branch>.state, then
// writes the new hash. When unchanged it prints nothing and returns nil
// (exit 0). Cache read/write errors never fail the command — the worst case
// is a redundant print, which is safe for a hook.
func renderMRStatusDelta(branch string, resp mrStatusResponse) error {
	body, changed := mrStatusDeltaGate(branch, "", resp)
	if changed {
		fmt.Print(body)
	}
	return nil
}

// mrStatusAttentionStates is the set of classified states worth interrupting a
// model turn for. It deliberately mirrors internal/hud/mrwatch.notifyStates
// (the M5 inbox-nudge gate) so the hook lane and the notification lane agree on
// what "needs attention" means — a divergence there would make the injected
// text ("need attention") disagree with the HUD.
//
// Excluded on purpose, because they are transient or expected and would fire on
// ordinary progress: "ok", "awaiting_pipeline", "ci_running", "draft_idle" and
// the terminal "merged". Keep in sync with mrwatch/notifier.go.
var mrStatusAttentionStates = map[string]struct{}{
	"conflict":                {},
	"ci_failed_flaky":         {},
	"ci_failed_deterministic": {},
	"automerge_unarmed":       {},
	"pipeline_skipped":        {},
	"stale_branch":            {},
}

// filterMRStatusAttention narrows a response to only the MRs in an
// attention-worthy state. The result feeds the hook's delta gate, so the cached
// hash tracks the ATTENTION set rather than the raw set: ordinary transitions
// between benign states (awaiting_pipeline → ci_running → ok) collapse to the
// same empty attention set and never re-inject.
func filterMRStatusAttention(resp mrStatusResponse) mrStatusResponse {
	out := resp
	out.MergeRequests = nil
	for _, mr := range resp.MergeRequests {
		if _, ok := mrStatusAttentionStates[mr.State]; ok {
			out.MergeRequests = append(out.MergeRequests, mr)
		}
	}
	out.Count = len(out.MergeRequests)
	return out
}

// renderMRStatusHook emits a vendor context-injection hook envelope wrapping
// the delta-gated brief body. It prints nothing (exit 0) when no MR is in an
// attention-worthy state, when the attention set is unchanged since the last
// emission, or when the vendor is unknown — all safe no-ops for a
// UserPromptSubmit/BeforeAgent hook.
//
// Gating is two-stage, so the hook cannot fire on every prompt:
//
//  1. CLASS gate — only MRs in mrStatusAttentionStates survive. A healthy or
//     merely-in-progress MR never injects anything.
//  2. DELTA gate — the surviving set must differ from the last set injected for
//     this branch+vendor.
//
// The delta gate runs over the FILTERED set and always writes its cache, even
// when the filtered set is empty. That is what makes recovery-then-regression
// observable: conflict (emit, cache=H1) → fixed (cache=H∅, nothing emitted) →
// conflict again (H1 ≠ H∅ → emits again). Hashing the raw set instead would
// leave the cache at H1 and silently swallow the second conflict.
//
// The delta cache is scoped per hook vendor ("hook-claude"/"hook-gemini") so a
// Claude session and a Gemini session working the same branch do not starve
// each other's first emission (they observe independent last-seen hashes). The
// plain --delta path keeps its branch-only cache key, unchanged.
func renderMRStatusHook(vendor, branch string, resp mrStatusResponse) error {
	// Class gate first: the delta cache must track the attention set, not the
	// raw set (see the recovery-then-regression note above).
	attention := filterMRStatusAttention(resp)

	body, changed := mrStatusDeltaGate(branch, "hook-"+vendor, attention)
	if !changed || strings.TrimSpace(body) == "" {
		return nil
	}
	envelope, err := mrStatusHookEnvelope(vendor, branch, body)
	if err != nil {
		// Unknown vendor: stay silent (hook-safe) rather than erroring.
		return nil
	}
	fmt.Println(envelope)
	return nil
}

// mrStatusHookEnvelope wraps the brief MR-status body in the JSON stdout
// contract that Claude Code and Gemini CLI parse to inject prompt context.
//
// Both vendors use the identical shape, differing only in the hookEventName
// value:
//
//	{"hookSpecificOutput":{"hookEventName":"<event>","additionalContext":"<text>"}}
//
// Vendor contracts re-verified against live docs 2026-08-07:
//
// Claude Code — UserPromptSubmit. JSON on stdout is parsed only on exit 0, and
// hookSpecificOutput requires hookEventName set to the event name;
// additionalContext is inserted alongside the submitted prompt. UserPromptSubmit
// is one of the few events whose plain stdout is ALSO added as context, so a
// malformed envelope degrades to a visible line rather than silence.
// https://code.claude.com/docs/en/hooks
// (https://docs.anthropic.com/en/docs/claude-code/hooks 301-redirects here.)
//
// Gemini CLI — BeforeAgent (NOT BeforeModel: BeforeModel's only documented
// hookSpecificOutput members are llm_request/llm_response, so it cannot inject
// context; BeforeAgent documents additionalContext as text appended to the
// prompt). reference.md's field table omits hookEventName, but writing-hooks.md's
// runnable BeforeAgent example emits it, so we send it for both vendors.
// https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md
// https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/writing-hooks.md
func mrStatusHookEnvelope(vendor, branch, body string) (string, error) {
	var eventName string
	switch vendor {
	case "claude":
		eventName = "UserPromptSubmit"
	case "gemini":
		eventName = "BeforeAgent"
	default:
		return "", fmt.Errorf("unknown hook vendor %q (want claude|gemini)", vendor)
	}
	env := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     eventName,
			"additionalContext": mrStatusHookContextText(branch, body),
		},
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mrStatusHookContextText renders the human-facing context string injected into
// the model. It labels the source ([loom mr-status]) so the agent recognizes
// it as tool-provided awareness, names the branch, and lists the brief MR lines.
func mrStatusHookContextText(branch, body string) string {
	return "[loom mr-status] Open merge request(s) for branch " + branch +
		" need attention:\n" + strings.TrimRight(body, "\n")
}

// mrStatusDeltaGate computes the brief body and reports whether it changed
// versus the hash cached under ~/.loom/mrwatch/<sanitized-branch>[.<scope>].state,
// writing the new hash when it changed. An empty scope preserves the historical
// branch-only cache key used by --delta; a non-empty scope (e.g. "hook-claude")
// namespaces an independent last-seen hash. Cache read/write errors never fail:
// on a path-resolution error the body is reported as changed (fail toward
// emitting), which is safe for a best-effort awareness hook.
func mrStatusDeltaGate(branch, scope string, resp mrStatusResponse) (body string, changed bool) {
	body = renderMRStatusBrief(resp)
	newHash := hashMRStatus(resp)

	statePath, err := mrStatusStatePath(branch, scope)
	if err != nil {
		return body, true // cannot cache → always emit
	}

	prev, _ := os.ReadFile(statePath) // missing/unreadable → "" → treated as changed
	if strings.TrimSpace(string(prev)) == newHash {
		return body, false // unchanged
	}

	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err == nil {
		_ = os.WriteFile(statePath, []byte(newHash+"\n"), 0o644)
	}
	return body, true
}

// hashMRStatus computes a stable hash over the classified MR set. It captures
// IID, state, reason, and web_url so any classification transition (and any
// MR appearing/disappearing) changes the hash, while cosmetic fields (title,
// timestamps) do not churn it.
func hashMRStatus(resp mrStatusResponse) string {
	mrs := sortedMRs(resp.MergeRequests)
	var b strings.Builder
	for _, mr := range mrs {
		fmt.Fprintf(&b, "%s|%d|%s|%s|%s\n", mr.Repo, mr.IID, mr.State, mr.Reason, mr.WebURL)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// sortedMRs returns a copy sorted by repo then IID for deterministic output
// and hashing (the HUD does not guarantee an order).
func sortedMRs(in []mrStatusMR) []mrStatusMR {
	out := append([]mrStatusMR(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].IID < out[j].IID
	})
	return out
}

// mrStatusStatePath returns the delta-cache path for a branch, optionally
// namespaced by scope: ~/.loom/mrwatch/<sanitized-branch>[.<scope>].state.
// An empty scope yields the historical branch-only path used by --delta.
func mrStatusStatePath(branch, scope string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := sanitizeBranchForFile(branch)
	if scope != "" {
		name += "." + sanitizeBranchForFile(scope)
	}
	return filepath.Join(home, ".loom", "mrwatch", name+".state"), nil
}

// sanitizeBranchForFile makes a branch name safe as a single path segment:
// every character outside [A-Za-z0-9._-] becomes '-'. This keeps
// "feat/mrwatch-m2" from creating nested directories.
func sanitizeBranchForFile(branch string) string {
	var b strings.Builder
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	return out
}
