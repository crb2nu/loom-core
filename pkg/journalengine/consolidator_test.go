package journalengine

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestConsolidationIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		in   Consolidation
		want bool
	}{
		{name: "zero value", in: Consolidation{}, want: true},
		{name: "whitespace identity only", in: Consolidation{Identity: "  \n"}, want: true},
		{name: "identity present", in: Consolidation{Identity: "who I am"}, want: false},
		{name: "ledger only", in: Consolidation{Ledger: []string{"[Epoch 1] a"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.IsEmpty(); got != tt.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConsolidateAppliesTheResult(t *testing.T) {
	j := New("agent", nil)
	for epoch := 1; epoch <= 6; epoch++ {
		turn(j, epoch, "I remember.")
	}
	before := len(j.Entries())

	var captured ConsolidationRequest
	result, dropped, err := Consolidate(
		context.Background(),
		j,
		ConsolidatorFunc(func(_ context.Context, req ConsolidationRequest) (Consolidation, error) {
			captured = req
			return Consolidation{
				Identity: "I have read many tickets.",
				Ledger:   []string{"[Epochs 1-3] three tickets arrived and two were closed"},
			}, nil
		}),
		0.5,
	)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	// The request carries the span and the entries, not a prompt: the prompt is
	// the implementation's business.
	if captured.Owner != "agent" {
		t.Errorf("request Owner = %q, want %q", captured.Owner, "agent")
	}
	if captured.EpochStart != 1 || captured.EpochEnd != 3 {
		t.Errorf("request span = %d-%d, want 1-3", captured.EpochStart, captured.EpochEnd)
	}
	if len(captured.Entries) != dropped {
		t.Errorf("request carried %d entries but %d were dropped", len(captured.Entries), dropped)
	}
	if captured.PriorIdentity != "" {
		t.Errorf("first consolidation PriorIdentity = %q, want empty", captured.PriorIdentity)
	}

	if got, want := len(j.Entries()), before-dropped; got != want {
		t.Errorf("entries after consolidation = %d, want %d", got, want)
	}
	if !strings.Contains(j.Render(), "I have read many tickets") {
		t.Error("identity passage missing from render")
	}
	if got := j.EpisodicLedger(); len(got) != 1 {
		t.Errorf("episodic ledger = %v, want one line", got)
	}
	if result.Identity != "I have read many tickets." {
		t.Errorf("returned Identity = %q", result.Identity)
	}
}

func TestConsolidatePassesThePriorIdentityForward(t *testing.T) {
	// The second consolidation must see the first, so the model integrates it
	// rather than starting over — and so a caller can compare for a
	// paraphrase-only response.
	j := New("agent", nil)
	for epoch := 1; epoch <= 4; epoch++ {
		turn(j, epoch, "I remember.")
	}
	j.ApplyConsolidation(Consolidation{Identity: "the first identity"}, 0)

	var seen string
	_, _, err := Consolidate(
		context.Background(),
		j,
		ConsolidatorFunc(func(_ context.Context, req ConsolidationRequest) (Consolidation, error) {
			seen = req.PriorIdentity
			return Consolidation{Identity: "the second identity"}, nil
		}),
		0.5,
	)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if seen != "the first identity" {
		t.Errorf("PriorIdentity = %q, want %q", seen, "the first identity")
	}
}

func TestConsolidateLeavesTheJournalIntactOnFailure(t *testing.T) {
	// Running one turn over budget is recoverable; silently discarding history is
	// not. Every failure path must be a no-op on the journal.
	sentinel := errors.New("lane unreachable")
	tests := []struct {
		name        string
		consolidate ConsolidatorFunc
		wantErr     string
	}{
		{
			name: "consolidator error",
			consolidate: func(context.Context, ConsolidationRequest) (Consolidation, error) {
				return Consolidation{}, sentinel
			},
			wantErr: "lane unreachable",
		},
		{
			name: "empty result",
			consolidate: func(context.Context, ConsolidationRequest) (Consolidation, error) {
				return Consolidation{}, nil
			},
			wantErr: "empty result",
		},
		{
			name: "whitespace-only result",
			consolidate: func(context.Context, ConsolidationRequest) (Consolidation, error) {
				return Consolidation{Identity: "   "}, nil
			},
			wantErr: "empty result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := New("agent", nil)
			for epoch := 1; epoch <= 6; epoch++ {
				turn(j, epoch, "I remember.")
			}
			before := j.Render()
			beforeCount := len(j.Entries())

			_, dropped, err := Consolidate(context.Background(), j, tt.consolidate, 0.5)
			if err == nil {
				t.Fatal("Consolidate() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
			if dropped != 0 {
				t.Errorf("dropped = %d, want 0 on failure", dropped)
			}
			if got := j.Render(); got != before {
				t.Error("journal was modified despite the failure")
			}
			if got := len(j.Entries()); got != beforeCount {
				t.Errorf("entries = %d, want %d (unchanged)", got, beforeCount)
			}
			if got := j.Consolidations(); got != 0 {
				t.Errorf("Consolidations() = %d, want 0 on failure", got)
			}
		})
	}
}

func TestConsolidateOnAnEmptyJournalIsANoop(t *testing.T) {
	called := false
	j := New("agent", nil)
	result, dropped, err := Consolidate(
		context.Background(),
		j,
		ConsolidatorFunc(func(context.Context, ConsolidationRequest) (Consolidation, error) {
			called = true
			return Consolidation{Identity: "x"}, nil
		}),
		0.5,
	)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if called {
		t.Error("consolidator was called with nothing to distil")
	}
	if dropped != 0 || !result.IsEmpty() {
		t.Errorf("got (%v, %d), want an empty no-op", result, dropped)
	}
}

func TestConsolidateGuardsAgainstNilArguments(t *testing.T) {
	noop := ConsolidatorFunc(func(context.Context, ConsolidationRequest) (Consolidation, error) {
		return Consolidation{Identity: "x"}, nil
	})
	if _, _, err := Consolidate(context.Background(), nil, noop, 0.5); err == nil {
		t.Error("Consolidate(nil journal) succeeded, want an error")
	}
	if _, _, err := Consolidate(context.Background(), New("a", nil), nil, 0.5); err == nil {
		t.Error("Consolidate(nil consolidator) succeeded, want an error")
	}
}

func TestConsolidatePropagatesContextCancellation(t *testing.T) {
	j := New("agent", nil)
	for epoch := 1; epoch <= 4; epoch++ {
		turn(j, epoch, "I remember.")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Consolidate(ctx, j, ConsolidatorFunc(
		func(ctx context.Context, _ ConsolidationRequest) (Consolidation, error) {
			return Consolidation{}, ctx.Err()
		},
	), 0.5)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if j.Consolidations() != 0 {
		t.Error("a cancelled consolidation still mutated the journal")
	}
}

// --------------------------------------------------------------------------- //
// The prefix contract helper
// --------------------------------------------------------------------------- //

func TestCheckPrefixExtension(t *testing.T) {
	tests := []struct {
		name    string
		earlier string
		later   string
		wantErr string
	}{
		{name: "clean extension", earlier: "abc", later: "abcd"},
		{name: "no growth", earlier: "abc", later: "abc", wantErr: "did not grow"},
		{name: "shrank", earlier: "abcd", later: "abc", wantErr: "shrank"},
		{
			name:    "a volatile byte above the boundary",
			earlier: "turn 13 of 30: history",
			later:   "turn 14 of 30: history and more",
			wantErr: "prefix diverges at byte 6",
		},
		{
			name:    "empty to non-empty is a clean extension",
			earlier: "",
			later:   "abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPrefixExtension(tt.earlier, tt.later)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckPrefixExtension() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckPrefixExtension() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckPrefixExtensionErrorIsActionable(t *testing.T) {
	err := CheckPrefixExtension("history at 10:31:02", "history at 10:34:56 plus more")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ephemeral tail") {
		t.Errorf("error should point at the fix, got: %v", err)
	}
}

func TestFirstDivergence(t *testing.T) {
	tests := []struct {
		name    string
		earlier string
		later   string
		want    int
	}{
		{name: "extension", earlier: "abc", later: "abcd", want: -1},
		{name: "identical", earlier: "abc", later: "abc", want: -1},
		{name: "differs at 2", earlier: "abc", later: "abd", want: 2},
		{name: "later is shorter", earlier: "abcd", later: "abc", want: 3},
		{name: "differs at 0", earlier: "xbc", later: "abc", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstDivergence(tt.earlier, tt.later); got != tt.want {
				t.Errorf("FirstDivergence() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --------------------------------------------------------------------------- //
// The end-to-end shape a consumer would adopt
// --------------------------------------------------------------------------- //

func TestALongLifeStaysCacheableAcrossConsolidations(t *testing.T) {
	// The whole contract in one test: many turns, a consolidation whenever the
	// budget gets tight, and the prefix invariant holding between every pair of
	// turns that did not consolidate.
	j := New("agent", NewTokenLedger(4.0))
	consolidator := ConsolidatorFunc(
		func(_ context.Context, req ConsolidationRequest) (Consolidation, error) {
			return Consolidation{
				Identity: "I have handled " + strings.Repeat("work ", 3) + "before.",
				Ledger: []string{
					"[Epochs " + itoaTest(req.EpochStart) + "-" + itoaTest(req.EpochEnd) +
						"] a span of work was distilled",
				},
			}, nil
		},
	)

	const budget = 900
	prior := ""
	consolidatedAt := map[int]bool{}
	for epoch := 1; epoch <= 40; epoch++ {
		if j.NeedsConsolidation(budget, 0.9) {
			if _, dropped, err := Consolidate(context.Background(), j, consolidator, 0.5); err != nil {
				t.Fatalf("epoch %d: Consolidate: %v", epoch, err)
			} else if dropped > 0 {
				consolidatedAt[epoch] = true
				prior = "" // the prefix legitimately reset
			}
		}
		turn(j, epoch, "A response of some length so the budget actually moves.")

		render := j.Render()
		if prior != "" {
			if err := CheckPrefixExtension(prior, render); err != nil {
				t.Fatalf("epoch %d broke the prefix contract: %v", epoch, err)
			}
		}
		prior = render
	}

	if len(consolidatedAt) == 0 {
		t.Fatal("no consolidation ever fired; the test never exercised a cache reset")
	}
	if j.Consolidations() != len(consolidatedAt) {
		t.Errorf("Consolidations() = %d, want %d", j.Consolidations(), len(consolidatedAt))
	}
	// The episodic ledger accumulated one line per consolidation; the identity
	// block stayed bounded at one passage.
	if got := len(j.EpisodicLedger()); got == 0 {
		t.Error("episodic ledger is empty after multiple consolidations")
	}
	if got := len(j.CoreMemories()); got != 1 {
		t.Errorf("core memories = %d, want exactly 1 (the block must stay bounded)", got)
	}
	// And the whole life survives a restart byte-for-byte.
	if got, want := FromSnapshot(j.Snapshot()).Render(), j.Render(); got != want {
		t.Error("render differs after a snapshot round trip")
	}
}

func itoaTest(i int) string { return strconv.Itoa(i) }
