package planning

import "testing"

func TestClassifyWorkspaceSignalDebt_IgnoresResolvedSignals(t *testing.T) {
	got := ClassifyWorkspaceSignalDebt([]WorkspaceSignal{
		{Source: "loki", Service: "api", Count: 200, Resolved: true},
	}, DefaultSignalDebtThresholds())
	if got.Level != SignalDebtNone {
		t.Fatalf("level = %s, want none", got.Level)
	}
	if got.BlocksAutonomy {
		t.Fatalf("BlocksAutonomy = true, want false")
	}
}

func TestClassifyWorkspaceSignalDebt_ClassifiesMediumAsBlocking(t *testing.T) {
	got := ClassifyWorkspaceSignalDebt([]WorkspaceSignal{
		{Source: "loki", Service: "api", Count: 4},
		{Source: "ci", Service: "loom-core", Count: 2},
	}, DefaultSignalDebtThresholds())
	if got.Level != SignalDebtMedium {
		t.Fatalf("level = %s, want medium", got.Level)
	}
	if !got.BlocksAutonomy {
		t.Fatalf("BlocksAutonomy = false, want true")
	}
	if got.TotalEvents != 6 || got.UnresolvedClusters != 2 {
		t.Fatalf("summary = %+v", got)
	}
}

func TestClassifyWorkspaceSignalDebt_ClassifiesHighDebt(t *testing.T) {
	got := ClassifyWorkspaceSignalDebt([]WorkspaceSignal{
		{Source: "loki", Service: "api", Count: 30},
	}, DefaultSignalDebtThresholds())
	if got.Level != SignalDebtHigh {
		t.Fatalf("level = %s, want high", got.Level)
	}
	if !got.BlocksAutonomy {
		t.Fatalf("BlocksAutonomy = false, want true")
	}
}
