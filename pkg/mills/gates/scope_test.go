package gates

import (
	"context"
	"reflect"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestScopeTestCommandsDoNotChangeScope(t *testing.T) {
	for _, command := range []string{"go test ./pkg/mills/ -run 'Scope|Envelope'", "cd internal/hud/frontend && pnpm test", "pnpm test", "npm test", "make test", "go\ttest", "go\ntest", "go\u00a0test", "a&&b", "a|b", "a;b", "a>b", ""} {
		t.Run(command, func(t *testing.T) {
			for _, files := range [][]string{nil, {"pkg/alpha/a.go"}} {
				baseline := &store.BacklogItem{Slices: []store.Slice{{Files: files}}}
				withCommand := &store.BacklogItem{Slices: []store.Slice{{Files: files, Tests: []string{command}}}}
				for _, includeTests := range []bool{false, true} {
					want := buildAllowedSet(baseline.Slices, includeTests)
					got := buildAllowedSet(withCommand.Slices, includeTests)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("command changed allow-set: got %+v want %+v", got, want)
					}
				}
				for _, changed := range []string{"pkg/alpha/new.go", "pkg/beta/new.go", "go test ./pkg/mills/new.go"} {
					g := &Scope{}
					want, err := g.Evaluate(context.Background(), StageInput{Item: baseline, FilesChanged: []string{changed}})
					if err != nil {
						t.Fatal(err)
					}
					got, err := g.Evaluate(context.Background(), StageInput{Item: withCommand, FilesChanged: []string{changed}})
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("command changed verdict: got %+v want %+v", got, want)
					}
				}
			}
		})
	}
}

func TestScopePreservesTestPathsGlobsAndFiles(t *testing.T) {
	slices := []store.Slice{{Files: []string{"dir with spaces/file.go"}, Tests: []string{"pkg/alpha/a_test.go", "pkg/beta/*.go"}}}
	allowed := buildAllowedSet(slices, true)
	for _, path := range []string{"dir with spaces/file.go", "pkg/alpha/helper.go", "pkg/beta/fixture.go"} {
		if !isAllowed(path, allowed, false) {
			t.Errorf("path no longer allowed: %q", path)
		}
	}
	if isAllowed("pkg/gamma/other.go", allowed, false) {
		t.Error("unrelated path allowed")
	}
	withoutTests := buildAllowedSet(slices, false)
	if isAllowed("pkg/alpha/helper.go", withoutTests, false) || isAllowed("pkg/beta/fixture.go", withoutTests, false) {
		t.Error("includeTests=false includes tests")
	}
}

func TestScopeClassificationIgnoresTestCommands(t *testing.T) {
	original := pathExists
	t.Cleanup(func() { pathExists = original })
	for _, tc := range []struct {
		name     string
		existing map[string]bool
		want     telemetry.ScopeFailureClass
	}{
		{"grounded", map[string]bool{"pkg/alpha": true, "pkg/alpha/a.go": true}, telemetry.ScopeFailureGenuineDetour},
		{"basename", map[string]bool{"pkg/alpha": true}, telemetry.ScopeFailureWrongBasename},
		{"directory", map[string]bool{}, telemetry.ScopeFailureMissingDirectory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pathExists = func(path string) bool { return tc.existing[path] }
			for _, command := range []string{"go test ./pkg/mills/ -run 'Scope|Envelope'", "cd internal/hud/frontend && pnpm test", "pnpm test", "npm test", "make test", "go\ttest", "go\ntest", "go\u00a0test", "a&&b", "a|b", "a;b", "a>b", ""} {
				item := &store.BacklogItem{Slices: []store.Slice{{Files: []string{"pkg/alpha/a.go"}, Tests: []string{command}}}}
				if got := (&Scope{}).classifyFailure(item); got != tc.want {
					t.Fatalf("command %q: got %s want %s", command, got, tc.want)
				}
			}
		})
	}
}
