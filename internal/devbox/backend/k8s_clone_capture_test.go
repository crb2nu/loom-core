package backend

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestFailedInitContainerName picks the first non-zero-terminated INIT
// container (git-clone) and ignores healthy or running ones — that name drives
// which container's logs the failure-detail helper reads.
func TestFailedInitContainerName(t *testing.T) {
	term := func(code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error"}}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

	cases := []struct {
		name string
		init []corev1.ContainerStatus
		want string
	}{
		{
			name: "git-clone init exit 128",
			init: []corev1.ContainerStatus{{Name: "git-clone", State: term(128)}},
			want: "git-clone",
		},
		{
			name: "healthy init returns empty",
			init: []corev1.ContainerStatus{{Name: "git-clone", State: term(0)}},
			want: "",
		},
		{
			name: "running init returns empty",
			init: []corev1.ContainerStatus{{Name: "git-clone", State: running}},
			want: "",
		},
		{
			name: "first failed of several wins",
			init: []corev1.ContainerStatus{{Name: "prep", State: term(0)}, {Name: "git-clone", State: term(128)}},
			want: "git-clone",
		},
		{
			name: "no init containers",
			init: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: tc.init}}
			if got := failedInitContainerName(pod); got != tc.want {
				t.Errorf("failedInitContainerName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastNonEmptyLines collapses a multi-line git stderr tail into a single
// readable segment, dropping blank lines and keeping only the trailing n.
func TestLastNonEmptyLines(t *testing.T) {
	in := "Cloning into 'familyforge'...\n\nfatal: repository 'https://h/services/familyforge.git/' not found\n"
	got := lastNonEmptyLines(in, cloneLogTailLines)
	want := "Cloning into 'familyforge'... | fatal: repository 'https://h/services/familyforge.git/' not found"
	if got != want {
		t.Errorf("lastNonEmptyLines = %q, want %q", got, want)
	}
	if lastNonEmptyLines("a\nb\nc\nd", 2) != "c | d" {
		t.Errorf("tail cap not applied: %q", lastNonEmptyLines("a\nb\nc\nd", 2))
	}
	if lastNonEmptyLines("anything", 0) != "" {
		t.Errorf("n<=0 must return empty")
	}
}

// TestGitCredentialsRedaction guards the security invariant: the spawn git
// token that git echoes in a clone-URL error must never survive into the
// captured detail (which becomes a GitLab escalation issue body).
func TestGitCredentialsRedaction(t *testing.T) {
	in := "fatal: repository 'https://token:glpat-SECRETVALUE@gitlab.blevins.dev/services/familyforge.git/' not found"
	got := gitCredentialsRE.ReplaceAllString(in, "$1***@")
	if strings.Contains(got, "glpat-SECRETVALUE") {
		t.Fatalf("credential leaked after redaction: %q", got)
	}
	if !strings.Contains(got, "https://***@gitlab.blevins.dev/services/familyforge.git") {
		t.Errorf("redaction mangled the URL: %q", got)
	}
}
