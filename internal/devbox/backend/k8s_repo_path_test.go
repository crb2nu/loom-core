package backend

import "testing"

func TestRepoProjectPath(t *testing.T) {
	tests := []struct {
		name        string
		base        string
		repoPath    string
		wantAPIBase string
		wantProject string
		wantOK      bool
	}{
		{
			name:        "group-neutral root",
			base:        "http://192.168.50.218",
			repoPath:    "services/loom-core",
			wantAPIBase: "http://192.168.50.218",
			wantProject: "services/loom-core",
			wantOK:      true,
		},
		{
			name:        "legacy group-suffixed base dedups the boundary segment",
			base:        "https://gitlab.example.com/services/",
			repoPath:    "services/loom-core",
			wantAPIBase: "https://gitlab.example.com",
			wantProject: "services/loom-core",
			wantOK:      true,
		},
		{
			name:        "nested group base keeps its prefix",
			base:        "https://gitlab.example.com/homelab",
			repoPath:    "libs/fi-fhir",
			wantAPIBase: "https://gitlab.example.com",
			wantProject: "homelab/libs/fi-fhir",
			wantOK:      true,
		},
		{
			name:     "relative base is rejected",
			base:     "gitlab.example.com/services",
			repoPath: "services/loom-core",
			wantOK:   false,
		},
		{
			name:     "empty repo path is rejected",
			base:     "https://gitlab.example.com",
			repoPath: "",
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiBase, project, ok := RepoProjectPath(tt.base, tt.repoPath)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (apiBase=%q project=%q)", ok, tt.wantOK, apiBase, project)
			}
			if !ok {
				return
			}
			if apiBase != tt.wantAPIBase || project != tt.wantProject {
				t.Fatalf("got (%q, %q), want (%q, %q)", apiBase, project, tt.wantAPIBase, tt.wantProject)
			}
		})
	}
}
