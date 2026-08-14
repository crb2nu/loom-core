package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// ---------------------------------------------------------------- test doubles

// fakeConsolidator counts calls so "the flag is off" can be asserted as "zero
// LLM calls ever", not merely as "the journal looks unchanged".
type fakeConsolidator struct {
	calls       int
	err         error
	result      journalengine.Consolidation
	lastReq     journalengine.ConsolidationRequest
	lastTimeout time.Duration
	hadDeadline bool
}

func (f *fakeConsolidator) Consolidate(
	ctx context.Context,
	req journalengine.ConsolidationRequest,
) (journalengine.Consolidation, error) {
	f.calls++
	f.lastReq = req
	if dl, ok := ctx.Deadline(); ok {
		f.hadDeadline = true
		f.lastTimeout = time.Until(dl)
	}
	if f.err != nil {
		return journalengine.Consolidation{}, f.err
	}
	if f.result.IsEmpty() {
		return journalengine.Consolidation{
			Identity: "The item has run several implement attempts and remains unmerged.",
			Ledger: []string{fmt.Sprintf("[Epochs %d-%d] the implement stage ran repeatedly.",
				req.EpochStart, req.EpochEnd)},
		}, nil
	}
	return f.result, nil
}

// fakeMemoryChat records the prompt and the component label the consolidator
// attributed the call to.
type fakeMemoryChat struct {
	calls     int
	lastModel string
	lastComp  string
	lastPromp string
	reply     string
	err       error
}

func (f *fakeMemoryChat) ChatStructured(ctx context.Context, model, prompt string, _ int) (string, float64, error) {
	f.calls++
	f.lastModel = model
	f.lastComp = llmusage.ComponentFrom(ctx)
	f.lastPromp = prompt
	return f.reply, 0, f.err
}

// newCapturingLogger returns a logger writing to buf, so a test can assert on
// the structured Warn the soft-threshold check emits rather than only on the
// counter — the log line is what an operator actually reads first.
func newCapturingLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// counterValue reads a plain counter; counterVecValue reads one label set.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func counterVecValue(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	return counterValue(t, v.WithLabelValues(labels...))
}

// recordUntilSoftThreshold records 8 KiB outcomes through the runner's own hook
// until the soft-threshold observation fires exactly once, and returns how many
// record calls that took.
//
// It stops on the COUNTER rather than on the persisted size because a successful
// consolidation shrinks the journal back under the threshold before Put — so a
// post-Put size check would never see the crossing that just happened, and the
// loop would grow the journal forever.
func recordUntilSoftThreshold(t *testing.T, r *Runner, item *store.BacklogItem) int {
	t.Helper()
	ctx := context.Background()
	filler := strings.Repeat("x", itemMemoryMaxOwnBytes)
	before := counterValue(t, mills.ItemMemorySoftThresholdTotal)
	for i := 0; i < 64; i++ {
		r.recordItemMemory(ctx, item, Stage{ID: "implement"}, i+1, StageOutput{}, filler, nil)
		if counterValue(t, mills.ItemMemorySoftThresholdTotal) > before {
			return i + 1
		}
	}
	t.Fatalf("journal never reached the %d-byte soft threshold", itemMemorySoftThresholdBytes)
	return 0
}

// ------------------------------------------------------- soft-threshold signal

// The soft threshold is the whole point of this slice: before it, "the cap is
// biting" was invisible until the hard refusal, by which point the item's memory
// had already stopped growing. It must fire with the consolidation flag OFF.
func TestItemMemory_SoftThresholdWarnsAndCounts(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	logs := &strings.Builder{}
	r.Logger = newCapturingLogger(logs)

	before := counterValue(t, mills.ItemMemorySoftThresholdTotal)
	records := recordUntilSoftThreshold(t, r, item)
	after := counterValue(t, mills.ItemMemorySoftThresholdTotal)

	if after != before+1 {
		t.Errorf("soft-threshold counter = %v after %d records, want %v (one increment per crossing record)",
			after, records, before+1)
	}
	if !strings.Contains(logs.String(), "item memory: snapshot over soft threshold") {
		t.Errorf("no soft-threshold warning logged after %d records:\n%s", records, logs.String())
	}
	// The threshold must be strictly below the refusal point, or it is not an
	// early warning at all.
	if itemMemorySoftThresholdBytes >= store.ItemMemoryMaxSnapshotBytes {
		t.Fatalf("soft threshold %d is not below the hard cap %d",
			itemMemorySoftThresholdBytes, store.ItemMemoryMaxSnapshotBytes)
	}
}

// Below the threshold nothing fires — otherwise the counter would be noise on
// every ordinary run and the "cap is biting" signal would be unreadable.
func TestItemMemory_SoftThresholdQuietBelowIt(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	logs := &strings.Builder{}
	r.Logger = newCapturingLogger(logs)

	before := counterValue(t, mills.ItemMemorySoftThresholdTotal)
	for i := 0; i < 5; i++ {
		r.recordItemMemory(context.Background(), item, Stage{ID: "implement"}, i+1,
			StageOutput{}, "a normal log tail", nil)
	}
	if got := counterValue(t, mills.ItemMemorySoftThresholdTotal); got != before {
		t.Errorf("soft-threshold counter moved on an ordinary journal (%v -> %v)", before, got)
	}
	if strings.Contains(logs.String(), "soft threshold") {
		t.Errorf("soft-threshold warning fired on an ordinary journal:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------- flag gating

// The flag is the only thing standing between a dark seam and unbudgeted LLM
// spend on every oversized journal. Assert it through the consolidator, not
// through the journal contents: a journal that merely looks unconsolidated
// would also be produced by a call that failed.
func TestItemMemory_ConsolidationOffMakesNoLLMCall(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	t.Setenv(MemoryConsolidateEnv, "")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	fake := &fakeConsolidator{}
	r.MemoryConsolidator = fake

	recordUntilSoftThreshold(t, r, item)

	if fake.calls != 0 {
		t.Fatalf("consolidator called %d times with %s unset", fake.calls, MemoryConsolidateEnv)
	}
	j, err := st.ItemMemory.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if j.Consolidations() != 0 {
		t.Errorf("journal recorded %d consolidations with the knob off", j.Consolidations())
	}
}

func TestMemoryConsolidateEnabled_ParsesTruthyForms(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(MemoryConsolidateEnv, v)
		if !MemoryConsolidateEnabled() {
			t.Errorf("%q should enable consolidation", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no", "maybe"} {
		t.Setenv(MemoryConsolidateEnv, v)
		if MemoryConsolidateEnabled() {
			t.Errorf("%q should not enable consolidation", v)
		}
	}
}

// A wired consolidator with the flag on must be reached at most once per record
// call, no matter how far over the threshold the journal is. The loop stops on
// the first crossing, so "exactly one call" is the per-record bound.
func TestItemMemory_ConsolidatesAtMostOncePerRecord(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	t.Setenv(MemoryConsolidateEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	fake := &fakeConsolidator{}
	r.MemoryConsolidator = fake

	records := recordUntilSoftThreshold(t, r, item)

	if fake.calls != 1 {
		t.Fatalf("consolidator called %d times across %d records, want exactly 1", fake.calls, records)
	}
	if len(fake.lastReq.Entries) == 0 {
		t.Error("consolidator received no entries to distil")
	}
	if fake.lastReq.Owner != item.ID {
		t.Errorf("consolidation request owner = %q, want %q", fake.lastReq.Owner, item.ID)
	}
	// The call sits synchronously on the stage-completion path, so it must
	// carry a bound tighter than the shared client's five-minute research
	// timeout. Without one, a wedged backend stalls a stage that has already
	// succeeded.
	if !fake.hadDeadline {
		t.Error("consolidation ran without a deadline; a wedged backend would stall the stage")
	} else if fake.lastTimeout > itemMemoryConsolidationTimeout {
		t.Errorf("consolidation deadline %v exceeds the %v bound", fake.lastTimeout, itemMemoryConsolidationTimeout)
	}
	j, err := st.ItemMemory.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if j.Consolidations() != 1 {
		t.Errorf("persisted journal records %d consolidations, want 1", j.Consolidations())
	}
	if len(j.CoreMemories()) == 0 || len(j.EpisodicLedger()) == 0 {
		t.Errorf("consolidation was not persisted: core=%v ledger=%v", j.CoreMemories(), j.EpisodicLedger())
	}
}

// ------------------------------------------------------------ failure is safe

// journalengine.Consolidate guarantees the journal is untouched on error; this
// asserts the Mills wrapper preserves that end-to-end — the grown journal is
// still persisted, with every entry intact.
func TestItemMemory_ConsolidationFailureLosesNoEntries(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	t.Setenv(MemoryConsolidateEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	logs := &strings.Builder{}
	r.Logger = newCapturingLogger(logs)
	fake := &fakeConsolidator{err: errors.New("backend on fire")}
	r.MemoryConsolidator = fake

	ctx := context.Background()
	// Entries that must survive verbatim, recorded before the journal is big
	// enough to trigger anything.
	for i := 0; i < 3; i++ {
		r.recordItemMemory(ctx, item, Stage{ID: "research"}, i+1, StageOutput{},
			fmt.Sprintf("marker-entry-%d", i), nil)
	}
	beforeJ, err := st.ItemMemory.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	entriesBefore := len(beforeJ.Entries())

	errBefore := counterVecValue(t, mills.ItemMemoryConsolidationsTotal, "error")
	recordUntilSoftThreshold(t, r, item)
	errAfter := counterVecValue(t, mills.ItemMemoryConsolidationsTotal, "error")

	if fake.calls == 0 {
		t.Fatal("consolidator never called")
	}
	if errAfter <= errBefore {
		t.Errorf("error outcome not counted (%v -> %v)", errBefore, errAfter)
	}
	j, err := st.ItemMemory.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if j.Consolidations() != 0 {
		t.Errorf("failed consolidation was applied anyway (%d recorded)", j.Consolidations())
	}
	if len(j.Entries()) <= entriesBefore {
		t.Errorf("journal shrank or stalled after a failed consolidation: %d entries, had %d before",
			len(j.Entries()), entriesBefore)
	}
	rendered := j.Render()
	for i := 0; i < 3; i++ {
		if !strings.Contains(rendered, fmt.Sprintf("marker-entry-%d", i)) {
			t.Errorf("failed consolidation dropped marker-entry-%d", i)
		}
	}
	if !strings.Contains(logs.String(), "consolidation failed; persisting unconsolidated journal") {
		t.Errorf("no consolidation-failure warning logged:\n%s", logs.String())
	}
}

// An empty result is journalengine's other "leave it alone" case — a model that
// answers with nothing must not buy the drop of a span of history.
func TestItemMemory_EmptyConsolidationLosesNoEntries(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	t.Setenv(MemoryConsolidateEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	// A ConsolidatorFunc returning the zero Consolidation: no error, nothing
	// usable. journalengine converts this to an error before applying anything.
	calls := 0
	r.MemoryConsolidator = journalengine.ConsolidatorFunc(
		func(context.Context, journalengine.ConsolidationRequest) (journalengine.Consolidation, error) {
			calls++
			return journalengine.Consolidation{}, nil
		})

	recordUntilSoftThreshold(t, r, item)

	if calls == 0 {
		t.Fatal("consolidator never called")
	}
	j, err := st.ItemMemory.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if j.Consolidations() != 0 {
		t.Errorf("empty consolidation was applied (%d recorded)", j.Consolidations())
	}
	if len(j.Entries()) == 0 {
		t.Error("empty consolidation dropped every entry")
	}
}

// ------------------------------------------------- cache contract across reset

// Consolidation is the ONE legal prefix-cache reset event. The contract is not
// "the prefix survives" — it cannot — but "the reset happens once, deliberately,
// and the renders on either side of it are each strict prefix extensions".
func TestItemMemory_ConsolidationIsAtMostOneCacheReset(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	t.Setenv(MemoryConsolidateEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	r.MemoryConsolidator = &fakeConsolidator{}
	ctx := context.Background()

	var (
		renders       []string
		consolidCount []int
	)
	filler := strings.Repeat("x", itemMemoryMaxOwnBytes)
	postReset := 0
	for i := 0; i < 64 && postReset < 3; i++ {
		// Pathological 8 KiB outcomes until the reset lands, then ordinary
		// ones: a consolidation halves the journal, so continuing to record
		// 8 KiB tails would re-cross the threshold immediately and mint a
		// second reset. Production entries are small; this mirrors that and
		// leaves a clean post-reset run to check.
		tail := filler
		if len(consolidCount) > 0 && consolidCount[len(consolidCount)-1] > 0 {
			tail = "an ordinary log tail"
			postReset++
		}
		r.recordItemMemory(ctx, item, Stage{ID: "implement"}, i+1, StageOutput{}, tail, nil)
		j, err := st.ItemMemory.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		renders = append(renders, j.Render())
		consolidCount = append(consolidCount, j.Consolidations())
	}

	resetAt := -1
	for i := 1; i < len(consolidCount); i++ {
		if consolidCount[i] > consolidCount[i-1] {
			if resetAt >= 0 {
				t.Fatalf("more than one consolidation in the sequence: %v", consolidCount)
			}
			resetAt = i
		}
	}
	if resetAt < 0 {
		t.Fatalf("no consolidation happened across %d records: %v", len(renders), consolidCount)
	}
	if resetAt >= len(renders)-1 {
		t.Fatalf("consolidation landed on the last render (%d of %d); nothing to check after the reset",
			resetAt, len(renders))
	}

	// Before the reset: strict append-only, exactly as without this feature.
	for i := 1; i < resetAt; i++ {
		if err := journalengine.CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("prefix contract broken BEFORE the reset, between %d and %d: %v", i-1, i, err)
		}
	}
	// The reset itself is expected to break the prefix — that is what makes it a
	// reset. Assert it actually did, so a no-op consolidation cannot pass as one.
	if err := journalengine.CheckPrefixExtension(renders[resetAt-1], renders[resetAt]); err == nil {
		t.Error("consolidation did not rewrite the prefix; it reclaimed nothing")
	}
	// After the reset: append-only again, from the reset point forward.
	for i := resetAt + 1; i < len(renders); i++ {
		if err := journalengine.CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("prefix contract broken AFTER the reset, between %d and %d: %v", i-1, i, err)
		}
	}
	if err := journalengine.CheckPrefixExtension(renders[resetAt], renders[len(renders)-1]); err != nil {
		t.Fatalf("post-reset render is not a prefix extension of the reset render: %v", err)
	}
}

// ------------------------------------------------------ consolidator internals

func TestMemoryConsolidator_PromptCarriesTheSpan(t *testing.T) {
	chat := &fakeMemoryChat{reply: `{"identity":"It is a stalled item.","ledger":["[Epochs 0-2] two attempts failed."]}`}
	mc := NewMemoryConsolidator(chat, "test-model", nil)

	entries := []journalengine.Entry{
		{Epoch: 0, Kind: journalengine.KindSituation, Text: `Pipeline stage "implement" ran (attempt 1).`},
		{Epoch: 0, Kind: journalengine.KindOwn, Text: "Outcome: FAILED — scope gate"},
	}
	got, err := mc.Consolidate(context.Background(), journalengine.ConsolidationRequest{
		Owner:         "BL-1",
		PriorIdentity: "A previously summarised life.",
		Entries:       entries,
		EpochStart:    0,
		EpochEnd:      2,
	})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if got.Identity != "It is a stalled item." || len(got.Ledger) != 1 {
		t.Errorf("parsed consolidation = %+v", got)
	}
	if chat.lastModel != "test-model" {
		t.Errorf("dialed model %q, want test-model", chat.lastModel)
	}
	// Token accounting must land on the memory lane, not on whatever component
	// owned the surrounding context.
	if chat.lastComp != ComponentMemory {
		t.Errorf("llmusage component = %q, want %q", chat.lastComp, ComponentMemory)
	}
	// The prompt must carry the rendered span and the prior identity, or the
	// model is being asked to summarise nothing and to restart the voice.
	for _, want := range []string{
		journalengine.RenderEntries(entries),
		"A previously summarised life.",
		"BL-1",
	} {
		if !strings.Contains(chat.lastPromp, want) {
			t.Errorf("prompt missing %q:\n%s", want, chat.lastPromp)
		}
	}
}

func TestMemoryConsolidator_RejectsUnusableResponses(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		err   error
	}{
		{name: "transport error", err: errors.New("503 parked")},
		{name: "empty", reply: "   "},
		{name: "no json", reply: "I'm sorry, I can't do that."},
		{name: "malformed json", reply: `{"identity": `},
		{name: "empty envelope", reply: `{"identity": "", "ledger": []}`},
		{name: "blank ledger lines", reply: `{"identity": "  ", "ledger": ["  ", ""]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := NewMemoryConsolidator(&fakeMemoryChat{reply: tc.reply, err: tc.err}, "m", nil)
			if _, err := mc.Consolidate(context.Background(), journalengine.ConsolidationRequest{
				Owner:   "BL-1",
				Entries: []journalengine.Entry{{Epoch: 0, Text: "x"}},
			}); err == nil {
				t.Fatal("want an error so journalengine leaves the journal intact, got nil")
			}
		})
	}
}

// Models routinely wrap the envelope in prose or a fence; a parse miss there
// would cost the call and reclaim nothing.
func TestMemoryConsolidator_ParsesFencedEnvelope(t *testing.T) {
	reply := "Here you go:\n```json\n{\"identity\":\"An item.\",\"ledger\":[\"[Epochs 0-1] it ran.\"]}\n```\nHope that helps."
	mc := NewMemoryConsolidator(&fakeMemoryChat{reply: reply}, "m", nil)
	got, err := mc.Consolidate(context.Background(), journalengine.ConsolidationRequest{
		Owner:   "BL-1",
		Entries: []journalengine.Entry{{Epoch: 0, Text: "x"}},
	})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if got.Identity != "An item." || len(got.Ledger) != 1 {
		t.Errorf("parsed = %+v", got)
	}
}

func TestMemoryConsolidator_UnconfiguredFailsClosed(t *testing.T) {
	var mc *MemoryConsolidator
	if _, err := mc.Consolidate(context.Background(), journalengine.ConsolidationRequest{}); err == nil {
		t.Error("nil consolidator should error, not return an empty consolidation")
	}
	if _, err := (&MemoryConsolidator{}).Consolidate(context.Background(),
		journalengine.ConsolidationRequest{Entries: []journalengine.Entry{{Epoch: 0, Text: "x"}}}); err == nil {
		t.Error("client-less consolidator should error")
	}
}
