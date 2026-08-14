package council

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type stubSource struct {
	sigs []WorkspaceSignal
	err  error
}

func compileWithSignals(t *testing.T, signals WorkspaceSignalSource) *Brief {
	t.Helper()
	st := newCouncilTestStore(t)
	b, err := Compile(context.Background(), BriefSources{
		Store:   st,
		Now:     fixedTime,
		Signals: signals,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return b
}

func (s stubSource) RecentErrorClusters(_ context.Context, _ time.Time) ([]WorkspaceSignal, error) {
	return s.sigs, s.err
}

func TestNewCompositeSignals_Collapsing(t *testing.T) {
	if NewCompositeSignals() != nil {
		t.Error("no sources must collapse to nil")
	}
	if NewCompositeSignals(nil, nil) != nil {
		t.Error("all-nil sources must collapse to nil")
	}
	one := stubSource{sigs: []WorkspaceSignal{{Source: "a"}}}
	if got := NewCompositeSignals(one); got == nil {
		t.Error("a single source must return non-nil")
	}
}

func TestCompositeSignals_ConcatenatesAndReportsErrors(t *testing.T) {
	loki := stubSource{sigs: []WorkspaceSignal{{Source: "loki", Service: "x", Count: 3}}}
	broken := stubSource{err: errors.New("loki down")}
	ci := stubSource{sigs: []WorkspaceSignal{{Source: "gitlab-ci", Service: "ci/main", Count: 1}}}

	src := NewCompositeSignals(loki, broken, ci)
	got, err := src.RecentErrorClusters(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "loki down") {
		t.Fatalf("composite err = %v, want source failure", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d signals, want 2 (broken source skipped): %+v", len(got), got)
	}
	if got[0].Source != "loki" || got[1].Source != "gitlab-ci" {
		t.Errorf("concat order/sources wrong: %+v", got)
	}
}

func TestCompile_WorkspaceSignalsPopulatedAndSourceLabelled(t *testing.T) {
	b := compileWithSignals(t, stubSource{sigs: []WorkspaceSignal{
		{Source: "loki", Service: "loom/operator", Count: 3, Sample: "request failed"},
		{Source: "gitlab-ci", Service: "ci/main", Count: 1, Sample: "pipeline 42 failed"},
	}})

	for _, want := range []string{"## Workspace signals (recent errors)", "[loki]", "[gitlab-ci]"} {
		if !strings.Contains(b.Markdown, want) {
			t.Fatalf("populated brief missing %q:\n%s", want, b.Markdown)
		}
	}
	if strings.Contains(b.Markdown, WorkspaceSignalsUnavailableBody) {
		t.Fatalf("populated brief contains absence note:\n%s", b.Markdown)
	}
	if b.SourceCounts.WorkspaceSignals != 2 {
		t.Fatalf("WorkspaceSignals = %d, want 2", b.SourceCounts.WorkspaceSignals)
	}
}

func TestCompile_WorkspaceSignalsPartialFailureFailsOpen(t *testing.T) {
	signals := NewCompositeSignals(
		stubSource{err: errors.New("loki down")},
		stubSource{sigs: []WorkspaceSignal{{Source: "gitlab-ci", Service: "ci/main", Count: 2, Sample: "failed"}}},
	)
	b := compileWithSignals(t, signals)

	if !strings.Contains(b.Markdown, "[gitlab-ci]") {
		t.Fatalf("surviving source missing after Loki failure:\n%s", b.Markdown)
	}
	if !strings.Contains(b.Markdown, WorkspaceSignalsPartialBody) {
		t.Fatalf("partial failure note missing after Loki failure:\n%s", b.Markdown)
	}
	if b.SourceCounts.WorkspaceSignals != 1 {
		t.Fatalf("WorkspaceSignals = %d, want 1", b.SourceCounts.WorkspaceSignals)
	}
}

func TestCompile_WorkspaceSignalsAbsenceNoteFailsOpen(t *testing.T) {
	tests := []struct {
		name   string
		source WorkspaceSignalSource
	}{
		{name: "unconfigured", source: nil},
		{name: "empty", source: stubSource{}},
		{name: "unavailable", source: stubSource{err: errors.New("Loki unreachable")}},
		{name: "all composite sources unavailable", source: NewCompositeSignals(
			stubSource{err: errors.New("Loki unreachable")},
			stubSource{err: errors.New("GitLab unreachable")},
		)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := compileWithSignals(t, tc.source)
			if !strings.Contains(b.Markdown, WorkspaceSignalsUnavailableBody) {
				t.Fatalf("brief missing explicit absence note:\n%s", b.Markdown)
			}
			if b.SourceCounts.WorkspaceSignals != 0 {
				t.Fatalf("WorkspaceSignals = %d, want 0", b.SourceCounts.WorkspaceSignals)
			}
		})
	}
}

func TestCompile_WorkspaceSignalsAreBounded(t *testing.T) {
	sigs := make([]WorkspaceSignal, workspaceSignalMaxClusters+5)
	for i := range sigs {
		sigs[i] = WorkspaceSignal{Source: "fake", Service: fmt.Sprintf("service-%02d", i), Count: 1, Sample: "failure"}
	}
	b := compileWithSignals(t, stubSource{sigs: sigs})

	if b.SourceCounts.WorkspaceSignals != workspaceSignalMaxClusters {
		t.Fatalf("WorkspaceSignals = %d, want cap %d", b.SourceCounts.WorkspaceSignals, workspaceSignalMaxClusters)
	}
	if strings.Contains(b.Markdown, fmt.Sprintf("service-%02d", workspaceSignalMaxClusters)) {
		t.Fatalf("brief rendered signal beyond cap:\n%s", b.Markdown)
	}
}
