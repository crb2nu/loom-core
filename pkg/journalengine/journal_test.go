package journalengine

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// turn records one three-entry turn (situation, one utterance heard, own reply),
// mirroring the shape the Python suite uses so the invariants stay comparable.
func turn(j *Journal, epoch int, own string) {
	n := strconv.Itoa(epoch)
	j.RecordTurn(
		epoch,
		"Situation "+n,
		[]Utterance{{Speaker: "Reviewer", Text: "Reviewer speaks in epoch " + n}},
		own,
	)
}

// --------------------------------------------------------------------------- //
// Rendering
// --------------------------------------------------------------------------- //

func TestRenderEmptyJournalIsSentinel(t *testing.T) {
	if got := New("agent", nil).Render(); got != EmptyJournal {
		t.Errorf("Render() = %q, want %q", got, EmptyJournal)
	}
}

func TestRenderIsAppendOnly(t *testing.T) {
	// The cache-hit property: earlier renders are strict prefixes of later ones.
	// Holds from the first non-empty render onward (the empty sentinel is
	// replaced wholesale by the first entry, and nothing is cached before the
	// first turn anyway) and until a consolidation rewrites the prefix.
	j := New("agent", nil)
	turn(j, 1, "I remember.")
	renders := []string{j.Render()}
	for epoch := 2; epoch < 8; epoch++ {
		turn(j, epoch, "I remember.")
		renders = append(renders, j.Render())
	}
	for i := 1; i < len(renders); i++ {
		if err := CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("render %d broke the append-only invariant: %v", i, err)
		}
	}
}

func TestRenderOrdersCoreThenLedgerThenLived(t *testing.T) {
	j := New("agent", nil)
	for epoch := 1; epoch < 5; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(Consolidation{
		Identity: "I have become the one who reads the logs first.",
		Ledger:   []string{"[Epochs 1-2] the reviewer objected twice and nothing changed"},
	}, firstKept)

	rendered := j.Render()
	if !strings.HasPrefix(rendered, CoreHeader) {
		t.Fatalf("render does not open with the core block:\n%s", rendered)
	}
	core := strings.Index(rendered, CoreHeader)
	ledger := strings.Index(rendered, LedgerHeader)
	lived := strings.Index(rendered, LivedHeader)
	if core >= ledger || ledger >= lived {
		t.Errorf("block order wrong: core=%d ledger=%d lived=%d", core, ledger, lived)
	}
	if !strings.Contains(rendered, "the reviewer objected twice") {
		t.Error("episodic ledger line missing from render")
	}
}

func TestRenderStaysAppendOnlyWithALedgerPresent(t *testing.T) {
	j := New("agent", nil)
	for epoch := 1; epoch < 4; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(
		Consolidation{Identity: "core", Ledger: []string{"[Epoch 1] a"}},
		firstKept,
	)
	renders := []string{j.Render()}
	for epoch := 4; epoch < 9; epoch++ {
		turn(j, epoch, "I remember.")
		renders = append(renders, j.Render())
	}
	for i := 1; i < len(renders); i++ {
		if err := CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("render %d broke the invariant with a ledger present: %v", i, err)
		}
	}
}

func TestRenderIsByteStableForIdenticalInput(t *testing.T) {
	// Go map iteration is randomized, so anything that renders a map would drift
	// between runs. Utterance slices exist to make that impossible; this test
	// would catch a regression that reintroduced map ordering.
	build := func() string {
		j := New("agent", nil)
		heard := SortedUtterances(map[string]string{
			"Reviewer": "be concise",
			"Planner":  "split the slice",
			"Auditor":  "cite the file",
		})
		j.RecordTurn(1, "A ticket arrived.", heard, "Understood.")
		return j.Render()
	}
	want := build()
	for i := 0; i < 25; i++ {
		if got := build(); got != want {
			t.Fatalf("render is not byte-stable across runs:\n got %q\nwant %q", got, want)
		}
	}
}

func TestRenderEntriesFormatsASpanWithoutHeaders(t *testing.T) {
	j := New("agent", nil)
	j.RecordTurn(3, "A ticket arrived.", []Utterance{{Speaker: "Reviewer", Text: "hurry"}}, "Reading it.")
	got := RenderEntries(j.Entries())
	want := strings.Join([]string{
		"[Epoch 3]",
		"The world: A ticket arrived.",
		"Reviewer said: hurry",
		"You said: Reading it.",
	}, "\n")
	if got != want {
		t.Errorf("RenderEntries() =\n%q\nwant\n%q", got, want)
	}
	if RenderEntries(nil) != "" {
		t.Errorf("RenderEntries(nil) = %q, want empty", RenderEntries(nil))
	}
}

// --------------------------------------------------------------------------- //
// Recording
// --------------------------------------------------------------------------- //

func TestRecordTurnSkipsOwnNameAndEmptyText(t *testing.T) {
	j := New("Shadow", nil)
	j.RecordTurn(1, "A mirror", []Utterance{
		{Speaker: "Shadow", Text: "should be skipped"},
		{Speaker: "Ego", Text: ""},
		{Speaker: "Self", Text: "kept"},
	}, "Noted.")

	var shadow, ego, self int
	for _, e := range j.Entries() {
		switch e.Speaker {
		case "Shadow":
			shadow++
		case "Ego":
			ego++
		case "Self":
			self++
		}
	}
	if shadow != 1 {
		t.Errorf("own name recorded %d times, want 1 (the own_response entry)", shadow)
	}
	if ego != 0 {
		t.Errorf("empty utterance was recorded %d times, want 0", ego)
	}
	if self != 1 {
		t.Errorf("non-empty utterance recorded %d times, want 1", self)
	}
}

func TestStaleRedeliveredMessagesAreNotRejournaled(t *testing.T) {
	// A message bus redelivers the latest message on quiet paths; the journal
	// must record each heard utterance once, not once per epoch, or the prefix
	// leaks tokens forever.
	j := New("Anima", nil)
	j.RecordTurn(1, "storm", []Utterance{{Speaker: "Shadow", Text: "I am here"}}, "I feel it.")
	j.RecordTurn(2, "calm", []Utterance{{Speaker: "Shadow", Text: "I am here"}}, "Still echoing.")
	j.RecordTurn(3, "calm", []Utterance{{Speaker: "Shadow", Text: "I have changed"}}, "New voice.")

	if got := heardTexts(j); !equalStrings(got, []string{"I am here", "I have changed"}) {
		t.Errorf("heard entries = %v, want [I am here, I have changed]", got)
	}

	// Dedupe state must survive a restore, or the first turn after a restart
	// re-journals whatever is still on the bus.
	restored := FromSnapshot(j.Snapshot())
	restored.RecordTurn(4, "calm", []Utterance{{Speaker: "Shadow", Text: "I have changed"}}, "Quiet.")
	if got := heardTexts(restored); !equalStrings(got, []string{"I am here", "I have changed"}) {
		t.Errorf("after restore, heard entries = %v, want [I am here, I have changed]", got)
	}
}

func heardTexts(j *Journal) []string {
	var out []string
	for _, e := range j.Entries() {
		if e.Kind == KindHeard {
			out = append(out, e.Text)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------------------- //
// Consolidation
// --------------------------------------------------------------------------- //

func TestSplitOldestLandsOnEpochBoundary(t *testing.T) {
	tests := []struct {
		name         string
		epochs       int
		keepFraction float64
		wantFirst    int
	}{
		{name: "half of ten three-entry epochs", epochs: 10, keepFraction: 0.5, wantFirst: 15},
		{name: "keep everything still cuts one epoch", epochs: 4, keepFraction: 1.0, wantFirst: 3},
		{name: "keep nothing cuts all", epochs: 4, keepFraction: 0.0, wantFirst: 12},
		{name: "out of range fraction is clamped", epochs: 4, keepFraction: 9.0, wantFirst: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := New("agent", nil)
			for epoch := 1; epoch <= tt.epochs; epoch++ {
				turn(j, epoch, "I remember.")
			}
			old, firstKept := j.SplitOldest(tt.keepFraction)
			if firstKept != tt.wantFirst {
				t.Fatalf("firstKept = %d, want %d", firstKept, tt.wantFirst)
			}
			if len(old) != firstKept {
				t.Fatalf("len(old) = %d, want %d", len(old), firstKept)
			}
			// The kept journal must start at a fresh epoch.
			entries := j.Entries()
			if firstKept < len(entries) {
				cutEpoch := entries[firstKept].Epoch
				for _, e := range old {
					if e.Epoch >= cutEpoch {
						t.Fatalf("epoch %d was split across the cut at epoch %d", e.Epoch, cutEpoch)
					}
				}
			}
		})
	}
}

func TestSplitOldestOnAnEmptyJournalIsANoop(t *testing.T) {
	old, firstKept := New("agent", nil).SplitOldest(0.5)
	if len(old) != 0 || firstKept != 0 {
		t.Errorf("SplitOldest() = (%d entries, %d), want (0, 0)", len(old), firstKept)
	}
}

func TestApplyConsolidationRewritesThePrefix(t *testing.T) {
	j := New("agent", nil)
	for epoch := 1; epoch < 7; epoch++ {
		turn(j, epoch, "I remember.")
	}
	before := len(j.Entries())
	beforeRender := j.Render()

	old, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(Consolidation{Identity: "I was born in conflict."}, firstKept)

	if j.Consolidations() != 1 {
		t.Errorf("Consolidations() = %d, want 1", j.Consolidations())
	}
	if got, want := len(j.Entries()), before-len(old); got != want {
		t.Errorf("entries after consolidation = %d, want %d", got, want)
	}
	rendered := j.Render()
	if !strings.HasPrefix(rendered, CoreHeader) {
		t.Error("render does not open with the core block")
	}
	if !strings.Contains(rendered, "I was born in conflict") {
		t.Error("core memory missing from render")
	}
	if !strings.Contains(rendered, LivedHeader) {
		t.Error("lived memory does not continue after the core block")
	}
	// Consolidation is the one deliberate cache reset: the invariant is
	// *expected* to break here, and only here.
	if err := CheckPrefixExtension(beforeRender, rendered); err == nil {
		t.Error("expected consolidation to reset the prefix, but it still extended it")
	}
}

func TestRepeatedConsolidationReplacesTheBoundedCore(t *testing.T) {
	// An unbounded core block would eventually consume the whole window.
	j := New("agent", nil)
	for epoch := 1; epoch < 5; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(Consolidation{Identity: "first core"}, firstKept)
	j.ApplyConsolidation(Consolidation{Identity: "integrated replacement core"}, 0)

	if got := j.CoreMemories(); !equalStrings(got, []string{"integrated replacement core"}) {
		t.Errorf("core memories = %v, want [integrated replacement core]", got)
	}
}

func TestLedgerSurvivesLaterConsolidationsVerbatim(t *testing.T) {
	// Identity is replaced, biography accumulates — the whole point of the
	// two-part consolidation output.
	j := New("agent", nil)
	for epoch := 1; epoch < 5; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(
		Consolidation{Identity: "first identity", Ledger: []string{"[Epoch 1] a"}},
		firstKept,
	)
	for epoch := 5; epoch < 9; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept = j.SplitOldest(0.5)
	j.ApplyConsolidation(
		Consolidation{Identity: "second identity", Ledger: []string{"[Epoch 5] b"}},
		firstKept,
	)

	if got := j.CoreMemories(); !equalStrings(got, []string{"second identity"}) {
		t.Errorf("core memories = %v, want [second identity]", got)
	}
	if got := j.EpisodicLedger(); !equalStrings(got, []string{"[Epoch 1] a", "[Epoch 5] b"}) {
		t.Errorf("episodic ledger = %v, want [[Epoch 1] a, [Epoch 5] b]", got)
	}
}

func TestAppendLedgerIsAppendOnlyAndDeduped(t *testing.T) {
	tests := []struct {
		name      string
		existing  []string
		additions []string
		wantAdded int
		want      []string
	}{
		{
			name:      "fresh lines land",
			additions: []string{"[Epoch 1] a", "[Epoch 2] b"},
			wantAdded: 2,
			want:      []string{"[Epoch 1] a", "[Epoch 2] b"},
		},
		{
			name:      "exact repeats are dropped",
			existing:  []string{"[Epoch 1] a"},
			additions: []string{"[Epoch 1] a", "[Epoch 2] b"},
			wantAdded: 1,
			want:      []string{"[Epoch 1] a", "[Epoch 2] b"},
		},
		{
			name:      "repeats are dropped case-insensitively",
			existing:  []string{"[Epoch 1] An Event"},
			additions: []string{"[epoch 1] an event"},
			wantAdded: 0,
			want:      []string{"[Epoch 1] An Event"},
		},
		{
			name:      "blank lines never land",
			additions: []string{"  ", "", "\t"},
			wantAdded: 0,
			want:      nil,
		},
		{
			name:      "surrounding whitespace is normalized away",
			additions: []string{"  [Epoch 1] a  "},
			wantAdded: 1,
			want:      []string{"[Epoch 1] a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := New("agent", nil)
			if len(tt.existing) > 0 {
				j.AppendLedger(tt.existing)
			}
			if got := j.AppendLedger(tt.additions); got != tt.wantAdded {
				t.Errorf("AppendLedger() added %d, want %d", got, tt.wantAdded)
			}
			if got := j.EpisodicLedger(); !equalStrings(got, tt.want) {
				t.Errorf("episodic ledger = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyConsolidationWithBlankIdentityKeepsThePrior(t *testing.T) {
	// A model that returns nothing usable for the identity half must not erase
	// the identity the agent already has.
	j := New("agent", nil)
	turn(j, 1, "I remember.")
	j.ApplyConsolidation(Consolidation{Identity: "who I am"}, 0)
	j.ApplyConsolidation(Consolidation{Identity: "   ", Ledger: []string{"[Epoch 1] a"}}, 0)

	if got := j.CoreMemories(); !equalStrings(got, []string{"who I am"}) {
		t.Errorf("core memories = %v, want [who I am]", got)
	}
	if got := j.EpisodicLedger(); !equalStrings(got, []string{"[Epoch 1] a"}) {
		t.Errorf("episodic ledger = %v, want [[Epoch 1] a]", got)
	}
}

func TestApplyConsolidationClampsAnOutOfRangeIndex(t *testing.T) {
	j := New("agent", nil)
	turn(j, 1, "I remember.")
	j.ApplyConsolidation(Consolidation{Identity: "core"}, 9999)
	if got := len(j.Entries()); got != 0 {
		t.Errorf("entries = %d, want 0 after an over-large firstKept", got)
	}
	j2 := New("agent", nil)
	turn(j2, 1, "I remember.")
	j2.ApplyConsolidation(Consolidation{Identity: "core"}, -5)
	if got := len(j2.Entries()); got != 3 {
		t.Errorf("entries = %d, want 3 after a negative firstKept", got)
	}
}

// --------------------------------------------------------------------------- //
// Persistence
// --------------------------------------------------------------------------- //

func TestSnapshotRoundTrip(t *testing.T) {
	j := New("agent", NewTokenLedger(3.1))
	for epoch := 1; epoch < 4; epoch++ {
		turn(j, epoch, "I remember.")
	}
	_, firstKept := j.SplitOldest(0.5)
	j.ApplyConsolidation(
		Consolidation{Identity: "core", Ledger: []string{"[Epoch 1] an event"}},
		firstKept,
	)

	restored := FromSnapshot(j.Snapshot())
	if got, want := restored.Render(), j.Render(); got != want {
		t.Errorf("restored render differs:\n got %q\nwant %q", got, want)
	}
	if got, want := restored.Consolidations(), j.Consolidations(); got != want {
		t.Errorf("restored consolidations = %d, want %d", got, want)
	}
	if got := restored.Ledger().CharsPerToken(); got != 3.1 {
		t.Errorf("restored chars/token = %v, want 3.1", got)
	}
	if got := restored.Owner(); got != "agent" {
		t.Errorf("restored owner = %q, want %q", got, "agent")
	}
}

func TestJSONRoundTripSurvivesTheWire(t *testing.T) {
	j := New("agent", nil)
	turn(j, 1, "I remember.")
	j.ApplyConsolidation(
		Consolidation{Identity: "core", Ledger: []string{"[Epoch 1] an event"}},
		0,
	)

	blob, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Journal
	if err := json.Unmarshal(blob, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := restored.Render(), j.Render(); got != want {
		t.Errorf("render after JSON round trip differs:\n got %q\nwant %q", got, want)
	}
	if got := restored.EpisodicLedger(); !equalStrings(got, []string{"[Epoch 1] an event"}) {
		t.Errorf("episodic ledger = %v after round trip", got)
	}
}

func TestFromSnapshotToleratesPartialSnapshots(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   Snapshot
		wantOwner  string
		wantCPT    float64
		wantRender string
	}{
		{
			name:       "zero value restores a usable journal",
			snapshot:   Snapshot{},
			wantOwner:  "unknown",
			wantCPT:    defaultCharsPerToken,
			wantRender: EmptyJournal,
		},
		{
			name: "a snapshot from before the episodic ledger existed",
			snapshot: Snapshot{
				Owner:        "agent",
				CoreMemories: []string{"core"},
			},
			wantOwner:  "agent",
			wantCPT:    defaultCharsPerToken,
			wantRender: CoreHeader + "\ncore\n\n" + LivedHeader,
		},
		{
			name: "blank ledger lines are dropped on restore",
			snapshot: Snapshot{
				Owner:          "agent",
				CoreMemories:   []string{"core"},
				EpisodicLedger: []string{"  ", "[Epoch 1] a", ""},
			},
			wantOwner:  "agent",
			wantCPT:    defaultCharsPerToken,
			wantRender: CoreHeader + "\ncore\n\n" + LedgerHeader + "\n[Epoch 1] a\n\n" + LivedHeader,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := FromSnapshot(tt.snapshot)
			if got := j.Owner(); got != tt.wantOwner {
				t.Errorf("Owner() = %q, want %q", got, tt.wantOwner)
			}
			if got := j.Ledger().CharsPerToken(); got != tt.wantCPT {
				t.Errorf("CharsPerToken() = %v, want %v", got, tt.wantCPT)
			}
			if got := j.Render(); got != tt.wantRender {
				t.Errorf("Render() =\n%q\nwant\n%q", got, tt.wantRender)
			}
		})
	}
}

func TestClearResetsEverythingButIdentity(t *testing.T) {
	j := New("agent", NewTokenLedger(3.5))
	turn(j, 1, "I remember.")
	j.ApplyConsolidation(Consolidation{Identity: "core", Ledger: []string{"[Epoch 1] a"}}, 0)
	j.Clear()

	if got := j.Render(); got != EmptyJournal {
		t.Errorf("Render() = %q, want the empty sentinel", got)
	}
	if got := j.Consolidations(); got != 0 {
		t.Errorf("Consolidations() = %d, want 0", got)
	}
	if got := j.Owner(); got != "agent" {
		t.Errorf("Owner() = %q, want %q", got, "agent")
	}
	if got := j.Ledger().CharsPerToken(); got != 3.5 {
		t.Errorf("CharsPerToken() = %v, want 3.5 (calibration survives Clear)", got)
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	// A caller that mutates a returned slice must not corrupt the journal, or a
	// stray append in a caller's prompt builder silently rewrites the prefix.
	j := New("agent", nil)
	turn(j, 1, "I remember.")
	j.ApplyConsolidation(Consolidation{Identity: "core", Ledger: []string{"[Epoch 1] a"}}, 0)

	entries := j.Entries()
	if len(entries) > 0 {
		entries[0].Text = "tampered"
	}
	core := j.CoreMemories()
	core[0] = "tampered"
	ledger := j.EpisodicLedger()
	ledger[0] = "tampered"

	if strings.Contains(j.Render(), "tampered") {
		t.Error("mutating a returned slice changed the journal")
	}
}
