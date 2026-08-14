package clients

import (
	"context"
	"net/http"
	"testing"
)

func TestParseGitLabMRReference(t *testing.T) {
	tests := []struct {
		name      string
		ref       string
		want      GitLabMRReference
		wantValid bool
	}{
		{name: "iid", ref: "!912", want: GitLabMRReference{IID: 912}, wantValid: true},
		{name: "annotated iid", ref: "912 (rebased)", want: GitLabMRReference{IID: 912}, wantValid: true},
		{
			name:      "canonical URL",
			ref:       "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912/diffs",
			want:      GitLabMRReference{IID: 912, Project: "services/loom-core", Authority: "gitlab.flexinfer.ai", ProjectBound: true},
			wantValid: true,
		},
		{name: "credentials rejected", ref: "https://user@gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912", wantValid: false},
		{name: "non-http rejected", ref: "ssh://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912", wantValid: false},
		{name: "ambiguous URL IID rejected", ref: "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/912oops", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseGitLabMRReference(tt.ref)
			if ok != tt.wantValid || got != tt.want {
				t.Fatalf("ParseGitLabMRReference(%q) = (%+v,%v), want (%+v,%v)", tt.ref, got, ok, tt.want, tt.wantValid)
			}
		})
	}
}

func TestGitLabReferenceIdentityComparisonDoesNotDoubleDecode(t *testing.T) {
	ref, ok := ParseGitLabMRReference("https://gitlab.flexinfer.ai/services%252Floom-core/-/merge_requests/912")
	if !ok {
		t.Fatal("double-encoded reference should parse before identity comparison")
	}
	if SameGitLabProject(ref.Project, "services/loom-core") {
		t.Fatalf("double-encoded project %q aliased configured project", ref.Project)
	}
	if !SameGitLabAuthority(ref.Authority, "https://gitlab.flexinfer.ai/api/v4") {
		t.Fatalf("authority %q did not match configured API URL", ref.Authority)
	}
}

func TestGitLabProjectIdentityComparisonIsCaseAndSuffixExact(t *testing.T) {
	for _, other := range []string{"Services/loom-core", "services/loom-core.git"} {
		if SameGitLabProject("services/loom-core", other) {
			t.Fatalf("project identity aliased %q", other)
		}
	}
	if !SameGitLabProject("/services/loom-core/", "services/loom-core") {
		t.Fatal("presentation slashes should not change project identity")
	}
}

func TestVerifyMRRequiresExactResponseIIDAndKnownState(t *testing.T) {
	tests := []struct {
		name    string
		resp    mrResponse
		wantErr bool
	}{
		{name: "valid", resp: mrResponse{IID: 77, State: "merged", WebURL: "https://gitlab.example/services/loom-core/-/merge_requests/77"}},
		{name: "wrong IID", resp: mrResponse{IID: 78, State: "merged", WebURL: "https://gitlab.example/services/loom-core/-/merge_requests/78"}, wantErr: true},
		{name: "wrong URL project", resp: mrResponse{IID: 77, State: "merged", WebURL: "https://gitlab.example/services/other/-/merge_requests/77"}, wantErr: true},
		{name: "wrong URL authority", resp: mrResponse{IID: 77, State: "merged", WebURL: "https://other.example/services/loom-core/-/merge_requests/77"}, wantErr: true},
		{name: "project case mismatch", resp: mrResponse{IID: 77, State: "merged", WebURL: "https://gitlab.example/Services/loom-core/-/merge_requests/77"}, wantErr: true},
		{name: "project git suffix mismatch", resp: mrResponse{IID: 77, State: "merged", WebURL: "https://gitlab.example/services/loom-core.git/-/merge_requests/77"}, wantErr: true},
		{name: "unknown state", resp: mrResponse{IID: 77, State: "reopened", WebURL: "https://gitlab.example/services/loom-core/-/merge_requests/77"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/77": func(_ *http.Request) (int, any) {
					return http.StatusOK, tt.resp
				},
			})
			err := cli.VerifyMR(context.Background(), 77)
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifyMR error = %v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
