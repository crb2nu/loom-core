package mills

import "testing"

// The scope-amendment section deliberately INVERTS this file's usual
// zero-value-off default: an omitted `scope_amendment:` block must resolve to
// ENABLED, or the fix ships inert through exactly the window it was written for
// (83% escalation rate, 2026-07-26). Locked here because the inversion is the
// one thing about this policy a future reader is most likely to "correct".
func TestScopeAmendmentDefaultsOn(t *testing.T) {
	var zero PipelinePolicy
	if !zero.ScopeAmendmentEnabled() {
		t.Error("omitted scope_amendment block must default to enabled")
	}
	if !Default().Pipeline.ScopeAmendmentEnabled() {
		t.Error("Default() must resolve to enabled")
	}
	off := PipelinePolicy{ScopeAmendment: ScopeAmendmentPolicy{Enabled: boolPtr(false)}}
	if off.ScopeAmendmentEnabled() {
		t.Error("explicit enabled:false must opt out")
	}
}

func TestScopeAmendmentTunableDefaults(t *testing.T) {
	var zero ScopeAmendmentPolicy
	if got := zero.Depth(); got != scopeAmendmentDefaultAncestorDepth {
		t.Errorf("Depth() = %d, want %d", got, scopeAmendmentDefaultAncestorDepth)
	}
	if got := zero.FileCap(); got != scopeAmendmentDefaultMaxFiles {
		t.Errorf("FileCap() = %d, want %d", got, scopeAmendmentDefaultMaxFiles)
	}
	set := ScopeAmendmentPolicy{AncestorDepth: 3, MaxFiles: 10}
	if set.Depth() != 3 || set.FileCap() != 10 {
		t.Errorf("explicit tunables not honored: depth=%d cap=%d", set.Depth(), set.FileCap())
	}
}

func TestScopeAmendmentValidation(t *testing.T) {
	cases := []struct {
		name    string
		pol     ScopeAmendmentPolicy
		wantErr bool
	}{
		{"zero value", ScopeAmendmentPolicy{}, false},
		{"in range", ScopeAmendmentPolicy{AncestorDepth: 3, MaxFiles: 8}, false},
		{"negative depth", ScopeAmendmentPolicy{AncestorDepth: -1}, true},
		// An unreachable depth would make the amendment a silent no-op rather
		// than an error the operator can see, so the ceiling is a hard reject.
		{"depth past the ceiling", ScopeAmendmentPolicy{AncestorDepth: scopeAmendmentMaxAncestorDepth + 1}, true},
		{"negative max_files", ScopeAmendmentPolicy{MaxFiles: -1}, true},
		{"max_files past the ceiling", ScopeAmendmentPolicy{MaxFiles: scopeAmendmentMaxFiles + 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			p.Pipeline.ScopeAmendment = tc.pol
			err := p.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// The amendment's sensitive-path refusal must use the SAME matcher the
// path_policy gate runs, so the two can never disagree about what is protected.
func TestProtectedPathsMatchIsTheSharedKernel(t *testing.T) {
	p := Default()
	paths := []string{"platform/gitops/x.yaml", "pkg/mills/pipeline/runner.go", "internal/hud/auth_handler.go"}
	viaPolicy := p.ProtectedPathsHit(paths)
	viaKernel := ProtectedPathsMatch(p.Pipeline.ProtectedPaths, paths)
	if len(viaPolicy) != len(viaKernel) {
		t.Fatalf("ProtectedPathsHit=%v vs ProtectedPathsMatch=%v", viaPolicy, viaKernel)
	}
	for i := range viaPolicy {
		if viaPolicy[i] != viaKernel[i] {
			t.Errorf("hit[%d]: %q vs %q", i, viaPolicy[i], viaKernel[i])
		}
	}
	if len(viaKernel) != 2 {
		t.Errorf("hits = %v, want the gitops path and the auth file", viaKernel)
	}
}
