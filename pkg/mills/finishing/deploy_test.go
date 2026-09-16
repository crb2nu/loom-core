package finishing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestDeployProbeCheck(t *testing.T) {
	tests := []struct {
		name, merge, build string
		ancestor           bool
		err                error
		want               DeployState
	}{
		{name: "missing merge", build: "build", want: DeployNotApplicable},
		{name: "missing build", merge: "merge", want: DeployNotApplicable},
		{name: "live", merge: "merge", build: "build", ancestor: true, want: DeployLive},
		{name: "pending", merge: "merge", build: "build", want: DeployPending},
		{name: "unknown", merge: "merge", build: "build", err: errors.New("git unavailable"), want: DeployUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := DeployProbe{RepoRoot: "/repo", BuildSHA: tc.build, IsAncestor: func(_ context.Context, root, ancestor, descendant string) (bool, error) {
				if root != "/repo" || ancestor != tc.merge || descendant != tc.build {
					t.Fatalf("args = %q %q %q", root, ancestor, descendant)
				}
				return tc.ancestor, tc.err
			}}
			got := p.Check(context.Background(), tc.merge)
			if got.State != tc.want || got.MergeSHA != tc.merge || got.BuildSHA != tc.build {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
			if tc.want == DeployUnknown && !strings.Contains(got.Reason, "git unavailable") {
				t.Fatalf("reason = %q", got.Reason)
			}
		})
	}
}

func TestDeploymentEvidence(t *testing.T) {
	changed, sha := DeploymentEvidence([]*store.StageResult{
		nil,
		{Artifacts: map[string]any{"files_changed": []any{"docs/readme.md", "pkg/mills/operator.go"}, "merged_sha": " first "}},
		{Artifacts: map[string]any{"merged_sha": "last"}},
	})
	if !changed || sha != "last" {
		t.Fatalf("DeploymentEvidence() = %v, %q", changed, sha)
	}
}
