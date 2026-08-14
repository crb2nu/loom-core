package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// killSwitchRequest is the admin POST body for /api/mills/policy/kill-switch.
// All fields optional; an empty body toggles the current state.
type killSwitchRequest struct {
	// Action is "pause" (enabled:false), "resume" (enabled:true), or
	// "toggle" (default — flip whatever gitops currently has).
	Action string `json:"action,omitempty"`
	// Reason is recorded in the commit message + MR description so the
	// MR itself is the durable audit trail of who paused/resumed and why.
	Reason string `json:"reason,omitempty"`
}

// killSwitchResponse is returned to the HUD so the toast can link the MR.
type killSwitchResponse struct {
	Changed         bool   `json:"changed"`
	PreviousEnabled bool   `json:"previous_enabled"`
	DesiredEnabled  bool   `json:"desired_enabled"`
	MRURL           string `json:"mr_url,omitempty"`
	MRIID           int64  `json:"mr_iid,omitempty"`
	Branch          string `json:"branch,omitempty"`
	Message         string `json:"message"`
}

// killSwitchEnabledLine matches the top-level autonomy kill-switch line in
// the mills policy ConfigMap. It is anchored to the `# kill switch` comment
// so the several nested `enabled:` keys (squads/audit/intake/...) are never
// matched by accident. If the comment is ever removed the handler fails
// closed (422) rather than editing the wrong line.
var killSwitchEnabledLine = regexp.MustCompile(`(?m)^([ \t]*enabled:[ \t]*)(true|false)([ \t]*#[ \t]*kill switch.*)$`)

// handlePolicyKillSwitch flips the global autonomy `enabled:` flag in the
// platform/gitops mills policy ConfigMap by opening a GitOps auto-PR. It
// does NOT write through to the live ConfigMap — Flux owns that file, so a
// live edit would be reverted on the next reconcile. The merged MR + Flux
// reconcile is what actually pauses/resumes the reconciler.
//
// Routing decision: .loom/127 (operator interview 2026-06-02). The pipeline
// GitLab token cannot write platform/gitops (kill-test 2026-06-03), so this
// uses a separate gitops-scoped client wired via withKillSwitch.
func (o *operator) handlePolicyKillSwitch(w http.ResponseWriter, r *http.Request) {
	if o.gitopsClient == nil {
		http.Error(w, "kill-switch not configured (GITOPS_GITLAB_TOKEN/GITOPS_GITLAB_PROJECT unset)",
			http.StatusServiceUnavailable)
		return
	}

	var req killSwitchRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "", "toggle", "pause", "resume":
	default:
		http.Error(w, "action must be one of pause|resume|toggle", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	branch := o.gitopsDefaultBranch
	policyPath := o.gitopsPolicyPath

	content, err := o.gitopsClient.GetRawFile(ctx, policyPath, branch)
	if err != nil {
		http.Error(w, "read gitops policy file: "+err.Error(), http.StatusBadGateway)
		return
	}
	m := killSwitchEnabledLine.FindStringSubmatch(content)
	if m == nil {
		http.Error(w,
			"could not locate kill-switch `enabled:` line in "+policyPath+" (expected a line ending with `# kill switch...`)",
			http.StatusUnprocessableEntity)
		return
	}
	currentEnabled := m[2] == "true"

	var desired bool
	switch action {
	case "pause":
		desired = false
	case "resume":
		desired = true
	default: // "" / "toggle"
		desired = !currentEnabled
	}

	if desired == currentEnabled {
		writeJSON(w, http.StatusOK, killSwitchResponse{
			Changed:         false,
			PreviousEnabled: currentEnabled,
			DesiredEnabled:  desired,
			Message:         fmt.Sprintf("autonomy already %s in gitops; no MR opened", enabledWord(currentEnabled)),
		})
		return
	}

	newBool := "false"
	if desired {
		newBool = "true"
	}
	newLine := m[1] + newBool + m[3]
	newContent := strings.Replace(content, m[0], newLine, 1)
	if newContent == content {
		http.Error(w, "internal: policy edit produced no change", http.StatusInternalServerError)
		return
	}

	verb := "pause"
	if desired {
		verb = "resume"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	mrBranch := fmt.Sprintf("mills/kill-switch-%s-%s", verb, stamp)
	reason := strings.TrimSpace(req.Reason)

	commitMsg := fmt.Sprintf("chore(mills): %s autonomy kill-switch (enabled=%s) [HUD]", verb, newBool)
	if reason != "" {
		commitMsg += "\n\n" + reason
	}

	if _, err := o.gitopsClient.CreateCommit(ctx, clients.CreateCommitRequest{
		Branch:        mrBranch,
		StartBranch:   branch,
		CommitMessage: commitMsg,
		Actions: []clients.CommitAction{{
			Action:   "update",
			FilePath: policyPath,
			Content:  newContent,
		}},
	}); err != nil {
		http.Error(w, "gitops commit: "+err.Error(), http.StatusBadGateway)
		return
	}

	mr, err := o.gitopsClient.CreateMR(ctx, pipeline.CreateMRRequest{
		SourceBranch: mrBranch,
		TargetBranch: branch,
		Title:        fmt.Sprintf("%s Mills autonomy kill-switch (enabled=%s)", titleCaseVerb(verb), newBool),
		Description:  killSwitchMRDescription(verb, reason),
	})
	if err != nil {
		http.Error(w, "gitops merge request: "+err.Error(), http.StatusBadGateway)
		return
	}

	o.logger.Warn("mills kill-switch MR opened",
		"verb", verb, "desired_enabled", desired, "mr", mr.URL, "branch", mrBranch, "reason", reason)

	writeJSON(w, http.StatusOK, killSwitchResponse{
		Changed:         true,
		PreviousEnabled: currentEnabled,
		DesiredEnabled:  desired,
		MRURL:           mr.URL,
		MRIID:           mr.MRIID,
		Branch:          mrBranch,
		Message: fmt.Sprintf("opened MR to %s autonomy (enabled=%s); merge + Flux reconcile applies it",
			verb, newBool),
	})
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func titleCaseVerb(verb string) string {
	if verb == "" {
		return ""
	}
	return strings.ToUpper(verb[:1]) + verb[1:]
}

func killSwitchMRDescription(verb, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Automated GitOps PR from the Loom HUD Mills Overview to **%s** the autonomy kill-switch.\n\n", verb)
	b.WriteString("Flips `enabled:` in the mills policy ConfigMap. Merging this MR and letting Flux reconcile is what actually ")
	if verb == "pause" {
		b.WriteString("freezes the reconciler (it exits cleanly on the next tick).\n")
	} else {
		b.WriteString("resumes the reconciler.\n")
	}
	if reason != "" {
		fmt.Fprintf(&b, "\n**Reason:** %s\n", reason)
	}
	b.WriteString("\n_Origin: POST /api/mills/policy/kill-switch (HUD admin)._\n")
	return b.String()
}
