package spin

import (
	"os"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/guard"
)

type PreflightReason string

const (
	PreflightWorkdirMissing      PreflightReason = "workdir_missing"
	PreflightWorkdirNotDir       PreflightReason = "workdir_not_directory"
	PreflightWorkdirUnusable     PreflightReason = "workdir_unusable"
	PreflightCredentialMissing   PreflightReason = "credential_missing"
	PreflightPromptEmpty         PreflightReason = "prompt_empty"
	PreflightPromptUndeliverable PreflightReason = "prompt_undeliverable"
)

type PreflightInput struct {
	Workdir             string
	RequiredCredentials []string
	CredentialLookup    func(string) (string, bool)
	Prompt              string
	PromptDeliverable   bool
}

// PreflightResult contains no credential values or prompt content and is safe
// for durable events and escalation metadata.
type PreflightResult struct {
	OK      bool
	Reason  PreflightReason
	Detail  string
	Missing []string
}

// CheckPreflight validates cheap local invocation prerequisites in a stable
// order. It makes no network calls and creates no filesystem artifacts.
func CheckPreflight(in PreflightInput) PreflightResult {
	workdir := strings.TrimSpace(in.Workdir)
	if workdir == "" {
		return PreflightResult{Reason: PreflightWorkdirMissing, Detail: "implement workdir is empty"}
	}
	info, err := os.Stat(workdir)
	if os.IsNotExist(err) {
		return PreflightResult{Reason: PreflightWorkdirMissing, Detail: "implement workdir does not exist: " + workdir}
	}
	if err != nil {
		return PreflightResult{Reason: PreflightWorkdirUnusable, Detail: "implement workdir is inaccessible: " + workdir}
	}
	if !info.IsDir() {
		return PreflightResult{Reason: PreflightWorkdirNotDir, Detail: "implement workdir is not a directory: " + workdir}
	}
	if info.Mode().Perm()&0500 != 0500 {
		return PreflightResult{Reason: PreflightWorkdirUnusable, Detail: "implement workdir lacks owner read/search access: " + workdir}
	}
	if f, openErr := os.Open(workdir); openErr != nil {
		return PreflightResult{Reason: PreflightWorkdirUnusable, Detail: "implement workdir is inaccessible: " + workdir}
	} else {
		_ = f.Close()
	}
	presence := guard.CheckCredentialPresence(in.RequiredCredentials, in.CredentialLookup)
	if len(presence.Missing) > 0 {
		return PreflightResult{Reason: PreflightCredentialMissing, Detail: "required credentials absent: " + strings.Join(presence.Missing, ", "), Missing: presence.Missing}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return PreflightResult{Reason: PreflightPromptEmpty, Detail: "implement prompt is empty"}
	}
	if !in.PromptDeliverable || strings.IndexByte(in.Prompt, 0) >= 0 {
		return PreflightResult{Reason: PreflightPromptUndeliverable, Detail: "implement prompt cannot be delivered"}
	}
	return PreflightResult{OK: true}
}
