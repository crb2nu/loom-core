package council

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubSource struct {
	sigs []WorkspaceSignal
	err  error
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

func TestCompositeSignals_ConcatenatesAndSkipsErrors(t *testing.T) {
	loki := stubSource{sigs: []WorkspaceSignal{{Source: "loki", Service: "x", Count: 3}}}
	broken := stubSource{err: errors.New("loki down")}
	ci := stubSource{sigs: []WorkspaceSignal{{Source: "gitlab-ci", Service: "ci/main", Count: 1}}}

	src := NewCompositeSignals(loki, broken, ci)
	got, err := src.RecentErrorClusters(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("composite err = %v, want nil (errors skipped)", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d signals, want 2 (broken source skipped): %+v", len(got), got)
	}
	if got[0].Source != "loki" || got[1].Source != "gitlab-ci" {
		t.Errorf("concat order/sources wrong: %+v", got)
	}
}
