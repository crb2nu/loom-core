package backend

import (
	"strings"
	"testing"
)

// provisionBackend builds a HarvesterVMBackend with just the config fields
// buildProvisionScript / cloneRequested consult. No clients needed — both are
// pure over cfg + opts.
func provisionBackend(gitBaseURL, gitSecret string) *HarvesterVMBackend {
	return &HarvesterVMBackend{
		cfg: HarvesterVMBackendConfig{
			SSHUser:    "agent",
			GitBaseURL: gitBaseURL,
			GitSecret:  gitSecret,
		},
	}
}

func TestCloneRequested(t *testing.T) {
	cases := []struct {
		name       string
		gitBaseURL string
		workDir    string
		want       bool
	}{
		{"clone when base url + concrete workdir", "http://git/services", "/workspace/services/loom-core", true},
		{"no clone without base url", "", "/workspace/services/loom-core", false},
		{"no clone for bare workspace root", "http://git/services", "/workspace", false},
		{"no clone for bare workspace root with slash", "http://git/services", "/workspace/", false},
		{"no clone for empty workdir", "http://git/services", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := provisionBackend(tc.gitBaseURL, "gitlab-creds")
			if got := h.cloneRequested(StartOpts{WorkDir: tc.workDir}); got != tc.want {
				t.Fatalf("cloneRequested=%v want %v", got, tc.want)
			}
		})
	}
}

func TestBuildProvisionScript_CloneAndCLI(t *testing.T) {
	h := provisionBackend("http://192.168.50.218/services", "gitlab-creds")
	opts := StartOpts{
		Name:               "spawn-x",
		WorkDir:            "/workspace/services/loom-core",
		Branch:             "feat/MILLS-CANARY",
		BaseBranch:         "main",
		AgentCLIInstallCmd: `if ! command -v codex >/dev/null 2>&1; then sudo npm install -g @openai/codex@0.130.0; fi`,
	}
	script, doClone := h.buildProvisionScript(opts, "s3cr3t-tok")
	if !doClone {
		t.Fatalf("doClone=false, want true")
	}

	// Project name derived from the last WorkDir segment, http scheme preserved,
	// token exported via env (not inlined into the URL literal). shellQuote
	// leaves values without shell-special chars unquoted.
	wantSubstrings := []string{
		"set -e",
		"command -v git >/dev/null 2>&1",
		"sudo npm install -g @openai/codex@0.130.0",
		"export GIT_TOKEN=s3cr3t-tok",
		`git clone "http://token:${GIT_TOKEN}@192.168.50.218/services/loom-core.git" /workspace/services/loom-core`,
		"git checkout feat/MILLS-CANARY",
		`git checkout -b feat/MILLS-CANARY "origin/${BASE}"`,
		"BASE=main",
		"sudo chown -R agent:agent /workspace/services/loom-core",
		"git rev-parse --abbrev-ref HEAD",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
}

// TestBuildProvisionScript_QuotesSpecialChars verifies shellQuote kicks in when
// the token or branch carries shell-special characters, keeping the
// clone/checkout lines injection-safe.
func TestBuildProvisionScript_QuotesSpecialChars(t *testing.T) {
	h := provisionBackend("http://192.168.50.218/services", "gitlab-creds")
	opts := StartOpts{
		WorkDir: "/workspace/services/loom-core",
		Branch:  "feat/has space",
	}
	script, _ := h.buildProvisionScript(opts, "tok n$ake")
	if !strings.Contains(script, "export GIT_TOKEN='tok n$ake'") {
		t.Errorf("special-char token not single-quoted\n%s", script)
	}
	if !strings.Contains(script, "git checkout 'feat/has space'") {
		t.Errorf("special-char branch not single-quoted\n%s", script)
	}
}

func TestBuildProvisionScript_HTTPSScheme(t *testing.T) {
	h := provisionBackend("https://gitlab.example.com/services", "gitlab-creds")
	opts := StartOpts{WorkDir: "/workspace/services/flexdeck", Branch: "fix/y"}
	script, doClone := h.buildProvisionScript(opts, "tok")
	if !doClone {
		t.Fatal("doClone=false, want true")
	}
	if !strings.Contains(script, `git clone "https://token:${GIT_TOKEN}@gitlab.example.com/services/flexdeck.git"`) {
		t.Errorf("https scheme not preserved in clone URL\n%s", script)
	}
}

func TestBuildProvisionScript_DefaultsBaseBranchToMain(t *testing.T) {
	h := provisionBackend("http://git/services", "gitlab-creds")
	opts := StartOpts{WorkDir: "/workspace/services/loom-core", Branch: "feat/z"} // BaseBranch empty
	script, _ := h.buildProvisionScript(opts, "tok")
	if !strings.Contains(script, "BASE=main") {
		t.Errorf("empty BaseBranch did not default to main\n%s", script)
	}
}

func TestBuildProvisionScript_NoBranchSkipsCheckout(t *testing.T) {
	h := provisionBackend("http://git/services", "gitlab-creds")
	opts := StartOpts{WorkDir: "/workspace/services/loom-core"} // no Branch
	script, doClone := h.buildProvisionScript(opts, "tok")
	if !doClone {
		t.Fatal("doClone=false, want true")
	}
	if strings.Contains(script, "git checkout") {
		t.Errorf("expected no checkout when Branch empty\n%s", script)
	}
	if !strings.Contains(script, "git clone") {
		t.Errorf("expected clone even without a branch\n%s", script)
	}
}

func TestBuildProvisionScript_CLIOnlyNoClone(t *testing.T) {
	h := provisionBackend("", "") // no git config → no clone
	opts := StartOpts{
		WorkDir:            "/workspace/services/loom-core",
		AgentCLIInstallCmd: `if ! command -v gemini >/dev/null 2>&1; then sudo npm install -g @google/gemini-cli@0.37.1; fi`,
	}
	script, doClone := h.buildProvisionScript(opts, "")
	if doClone {
		t.Fatal("doClone=true, want false (no GitBaseURL)")
	}
	if strings.Contains(script, "git clone") {
		t.Errorf("unexpected clone in CLI-only script\n%s", script)
	}
	if !strings.Contains(script, "@google/gemini-cli@0.37.1") {
		t.Errorf("CLI snippet missing\n%s", script)
	}
}

func TestBuildProvisionScript_EmptyWhenNothingToDo(t *testing.T) {
	h := provisionBackend("", "")
	script, doClone := h.buildProvisionScript(StartOpts{WorkDir: "/workspace/services/loom-core"}, "")
	if doClone || script != "" {
		t.Fatalf("expected empty no-op script, got doClone=%v script=%q", doClone, script)
	}
}

func TestBuildProvisionScript_CloneSkippedWhenTokenEmpty(t *testing.T) {
	// GitBaseURL is set but the resolved token is empty: buildProvisionScript
	// must not emit a clone (provisionVM enforces a non-empty token upstream;
	// this guards the builder independently). With a CLI snippet present the
	// script still installs the CLI.
	h := provisionBackend("http://git/services", "gitlab-creds")
	opts := StartOpts{
		WorkDir:            "/workspace/services/loom-core",
		Branch:             "feat/z",
		AgentCLIInstallCmd: `if ! command -v codex >/dev/null 2>&1; then sudo npm install -g @openai/codex@0.130.0; fi`,
	}
	script, doClone := h.buildProvisionScript(opts, "")
	if doClone {
		t.Fatal("doClone=true with empty token, want false")
	}
	if strings.Contains(script, "git clone") {
		t.Errorf("clone emitted with empty token\n%s", script)
	}
	if !strings.Contains(script, "@openai/codex@0.130.0") {
		t.Errorf("CLI snippet missing\n%s", script)
	}
}
