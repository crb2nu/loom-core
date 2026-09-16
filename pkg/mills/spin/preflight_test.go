package spin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPreflight(t *testing.T) {
	good := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	lookup := func(name string) (string, bool) {
		values := map[string]string{"PRESENT": "super-secret-value"}
		v, ok := values[name]
		return v, ok
	}
	cases := []struct {
		name string
		in   PreflightInput
		want PreflightReason
	}{
		{"missing workdir", PreflightInput{Workdir: filepath.Join(good, "absent")}, PreflightWorkdirMissing},
		{"non-directory", PreflightInput{Workdir: file}, PreflightWorkdirNotDir},
		{"unusable", PreflightInput{Workdir: locked}, PreflightWorkdirUnusable},
		{"one credential absent", PreflightInput{Workdir: good, RequiredCredentials: []string{"MISSING"}, CredentialLookup: lookup, Prompt: "go", PromptDeliverable: true}, PreflightCredentialMissing},
		{"multiple credentials absent", PreflightInput{Workdir: good, RequiredCredentials: []string{"ONE", "TWO"}, CredentialLookup: lookup, Prompt: "go", PromptDeliverable: true}, PreflightCredentialMissing},
		{"empty prompt", PreflightInput{Workdir: good, PromptDeliverable: true}, PreflightPromptEmpty},
		{"whitespace prompt", PreflightInput{Workdir: good, Prompt: " \n\t", PromptDeliverable: true}, PreflightPromptEmpty},
		{"undeliverable prompt", PreflightInput{Workdir: good, Prompt: "go"}, PreflightPromptUndeliverable},
		{"nul prompt", PreflightInput{Workdir: good, Prompt: "go\x00now", PromptDeliverable: true}, PreflightPromptUndeliverable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckPreflight(tc.in)
			if got.OK || got.Reason != tc.want {
				t.Fatalf("CheckPreflight() = %+v, want failure %q", got, tc.want)
			}
			if strings.Contains(got.Detail, "super-secret-value") {
				t.Fatal("credential value leaked")
			}
		})
	}
	if got := CheckPreflight(PreflightInput{Workdir: good, RequiredCredentials: []string{"PRESENT"}, CredentialLookup: lookup, Prompt: "implement", PromptDeliverable: true}); !got.OK {
		t.Fatalf("valid preflight = %+v", got)
	}
}
