package journalengine

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Render markers. These are a WIRE FORMAT, not cosmetics: changing one shifts
// the first byte of every render, invalidating every warm prefix on the serving
// platform at once and costing a fleet-wide cold prefill. They are byte-for-byte
// identical to the Python libs/journal-engine constants so a Go agent and a
// Python agent can share a warm prefix on the same lane. Treat any change as a
// breaking release of both.
const (
	CoreHeader   = "=== Core memories (distilled from earlier epochs) ==="
	LedgerHeader = "=== Episodic ledger (what happened, in order) ==="
	LivedHeader  = "=== Lived memory ==="
	EmptyJournal = "You are at the beginning. No memories yet."
)

// Kind classifies one journal entry.
type Kind string

const (
	// KindSituation is what the world presented to the agent.
	KindSituation Kind = "situation"
	// KindHeard is another agent's utterance, as received.
	KindHeard Kind = "heard"
	// KindOwn is the agent's own response.
	KindOwn Kind = "own"

	// speakerWorld is the attributed speaker for a KindSituation entry.
	speakerWorld = "world"
)

// Entry is one event in an agent's subjective timeline.
type Entry struct {
	Epoch   int    `json:"epoch"`
	Kind    Kind   `json:"kind"`
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// Utterance is one thing an agent heard, with its speaker.
//
// RecordTurn takes a slice rather than a map on purpose. Go map iteration order
// is randomized, so rendering a map would produce a different byte string on
// every run for the same inputs — which destroys the byte-stable prefix this
// package exists to provide. A slice makes the order the caller's explicit
// choice. If you are starting from a map, use SortedUtterances.
type Utterance struct {
	Speaker string
	Text    string
}

// SortedUtterances converts a speaker→text map into a deterministically ordered
// slice (sorted by speaker). Use it when the caller's inbox is a map and any
// stable order will do.
func SortedUtterances(heard map[string]string) []Utterance {
	out := make([]Utterance, 0, len(heard))
	for speaker, text := range heard {
		out = append(out, Utterance{Speaker: speaker, Text: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Speaker < out[j].Speaker })
	return out
}

// Journal is an agent's append-only lived memory plus a bounded distilled core.
//
// Two invariants matter before using it:
//
//  1. Append-only render. Between consolidations, an earlier Render is a strict
//     string prefix of every later one. This is what makes the prompt
//     prefix-cacheable, and CheckPrefixExtension is how you assert it.
//  2. Bounded core block. ApplyConsolidation *replaces* the identity passage and
//     *appends* to the episodic ledger. Keeping every identity passage forever
//     would let the supposedly bounded core block eventually consume the whole
//     context window.
//
// A Journal is safe for concurrent use.
type Journal struct {
	mu             sync.RWMutex
	owner          string
	ledger         *TokenLedger
	entries        []Entry
	coreMemories   []string
	episodicLedger []string
	consolidations int
	// lastHeard is the last text recorded per speaker. A message bus may
	// redeliver the latest message on a path even after it goes quiet, and
	// re-journaling it every epoch would leak tokens into the prefix forever.
	lastHeard map[string]string
}

// New returns an empty journal owned by owner (an agent id, session id, or job
// name — whatever names the thing whose memory this is). Pass a nil ledger for
// the default 4.0 chars/token prior.
func New(owner string, ledger *TokenLedger) *Journal {
	if ledger == nil {
		ledger = NewTokenLedger(defaultCharsPerToken)
	}
	return &Journal{
		owner:     owner,
		ledger:    ledger,
		lastHeard: make(map[string]string),
	}
}

// Owner reports whose memory this is.
func (j *Journal) Owner() string { return j.owner }

// Ledger returns the journal's token ledger, for the caller to Calibrate after a
// completion reports prompt_tokens.
func (j *Journal) Ledger() *TokenLedger { return j.ledger }

// Consolidations reports how many times this journal has been distilled. It is
// also the count of deliberate cache resets in this agent's life.
func (j *Journal) Consolidations() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.consolidations
}

// Entries returns a copy of the current lived entries.
func (j *Journal) Entries() []Entry {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return append([]Entry(nil), j.entries...)
}

// CoreMemories returns a copy of the distilled identity block.
func (j *Journal) CoreMemories() []string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return append([]string(nil), j.coreMemories...)
}

// EpisodicLedger returns a copy of the append-only event lines.
func (j *Journal) EpisodicLedger() []string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return append([]string(nil), j.episodicLedger...)
}

// ------------------------------------------------------------------ recording

// Record appends one entry. Empty or whitespace-only text is dropped.
func (j *Journal) Record(epoch int, kind Kind, speaker, text string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.record(epoch, kind, speaker, text)
}

func (j *Journal) record(epoch int, kind Kind, speaker, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	j.entries = append(j.entries, Entry{
		Epoch:   epoch,
		Kind:    kind,
		Speaker: speaker,
		Text:    text,
	})
}

// RecordTurn appends one full turn in stable order: situation, then each
// utterance heard, then the agent's own response.
//
// The owner's own name is skipped in heard (it does not hear itself), and a
// verbatim redelivery of the last text from a given speaker is dropped as a
// stale repeat.
func (j *Journal) RecordTurn(epoch int, situation string, heard []Utterance, own string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.record(epoch, KindSituation, speakerWorld, situation)
	for _, u := range heard {
		if u.Speaker == j.owner {
			continue
		}
		trimmed := strings.TrimSpace(u.Text)
		if prior, ok := j.lastHeard[u.Speaker]; ok && prior == trimmed {
			continue // stale redelivery, already journaled
		}
		j.record(epoch, KindHeard, u.Speaker, u.Text)
		if trimmed != "" {
			j.lastHeard[u.Speaker] = trimmed
		}
	}
	j.record(epoch, KindOwn, j.owner, own)
}

// ------------------------------------------------------------------ rendering

// Render renders the whole journal: core memories, then the episodic ledger,
// then lived memory in chronological order.
//
// Append-only invariant: as long as no consolidation happens between two calls,
// the earlier render is a strict string prefix of the later one. This is the
// property the prefix cache lives on.
func (j *Journal) Render() string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if len(j.entries) == 0 && len(j.coreMemories) == 0 && len(j.episodicLedger) == 0 {
		return EmptyJournal
	}

	lines := make([]string, 0, len(j.entries)*2+len(j.episodicLedger)+8)
	if len(j.coreMemories) > 0 {
		lines = append(lines, CoreHeader)
		for _, memory := range j.coreMemories {
			lines = append(lines, memory, "")
		}
	}
	if len(j.episodicLedger) > 0 {
		// Between consolidations this block is frozen, so it stays inside the
		// cacheable prefix; only a consolidation ever changes it.
		lines = append(lines, LedgerHeader)
		lines = append(lines, j.episodicLedger...)
		lines = append(lines, "")
	}
	lines = append(lines, LivedHeader)
	lines = appendEntryLines(lines, j.entries)
	return strings.Join(lines, "\n")
}

// RenderEntries renders a slice of entries in the same form Render uses for lived
// memory, without the section headers. This is what a caller feeds to a
// Consolidator as the text to distil.
func RenderEntries(entries []Entry) string {
	// appendEntryLines opens each epoch with a blank separator line, which is
	// right inside a full render but is leading whitespace when standing alone.
	return strings.TrimPrefix(strings.Join(appendEntryLines(nil, entries), "\n"), "\n")
}

// appendEntryLines renders entries chronologically, emitting a blank line and an
// "[Epoch N]" header whenever the epoch changes.
func appendEntryLines(lines []string, entries []Entry) []string {
	currentEpoch := -1
	for _, e := range entries {
		if e.Epoch != currentEpoch {
			lines = append(lines, "", "[Epoch "+strconv.Itoa(e.Epoch)+"]")
		}
		switch e.Kind {
		case KindSituation:
			lines = append(lines, "The world: "+e.Text)
		case KindHeard:
			lines = append(lines, e.Speaker+" said: "+e.Text)
		default:
			lines = append(lines, "You said: "+e.Text)
		}
		currentEpoch = e.Epoch
	}
	return lines
}

// ----------------------------------------------------------------- accounting

// TokenEstimate reports the estimated token count of the current render.
func (j *Journal) TokenEstimate() int {
	return j.ledger.Estimate(j.Render())
}

// NeedsConsolidation reports whether the render has reached threshold (a
// fraction in [0,1]) of budgetTokens and should be distilled before the next
// turn.
func (j *Journal) NeedsConsolidation(budgetTokens int, threshold float64) bool {
	return float64(j.TokenEstimate()) >= float64(budgetTokens)*threshold
}

// -------------------------------------------------------------- consolidation

// SplitOldest picks the oldest entries to distill, splitting on an epoch
// boundary, and returns them alongside the index of the first entry to keep.
//
// Keeps roughly the newest keepFraction of entries and never splits mid-epoch,
// so the kept journal starts at a clean epoch header — a mid-epoch cut would
// render a turn without its situation line and read as a non-sequitur.
func (j *Journal) SplitOldest(keepFraction float64) (old []Entry, firstKept int) {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if len(j.entries) == 0 {
		return nil, 0
	}
	if keepFraction < 0 {
		keepFraction = 0
	}
	if keepFraction > 1 {
		keepFraction = 1
	}
	cut := int(float64(len(j.entries)) * (1.0 - keepFraction))
	if cut < 1 {
		cut = 1
	}
	if cut > len(j.entries) {
		cut = len(j.entries)
	}
	// If the target lands inside an epoch, advance to the next boundary. If it
	// already lands at a boundary, keep that entire epoch.
	if cut < len(j.entries) && j.entries[cut-1].Epoch == j.entries[cut].Epoch {
		cutEpoch := j.entries[cut].Epoch
		for cut < len(j.entries) && j.entries[cut].Epoch == cutEpoch {
			cut++
		}
	}
	return append([]Entry(nil), j.entries[:cut]...), cut
}

// AppendLedger appends neutral episodic lines without rewriting existing ones,
// and reports how many were actually added.
//
// Exact repeats (case-insensitively) are dropped so a re-run, or a model that
// echoes the prior ledger back at you, cannot duplicate an event.
func (j *Journal) AppendLedger(lines []string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLedger(lines)
}

func (j *Journal) appendLedger(lines []string) int {
	seen := make(map[string]struct{}, len(j.episodicLedger))
	for _, existing := range j.episodicLedger {
		seen[strings.ToLower(strings.TrimSpace(existing))] = struct{}{}
	}
	added := 0
	for _, line := range lines {
		cleaned := strings.TrimSpace(line)
		if cleaned == "" {
			continue
		}
		key := strings.ToLower(cleaned)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		j.episodicLedger = append(j.episodicLedger, cleaned)
		added++
	}
	return added
}

// ApplyConsolidation replaces the oldest firstKept entries with a distilled
// consolidation.
//
// The Identity passage *replaces* the previous one; Ledger lines are *appended*.
// The two halves have deliberately different survival rules — see Consolidator.
// This is the one operation that rewrites the render's prefix, and therefore the
// one deliberate cache-reset event.
func (j *Journal) ApplyConsolidation(c Consolidation, firstKept int) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if identity := strings.TrimSpace(c.Identity); identity != "" {
		// Each consolidation integrates the previous core into one replacement
		// memory. Keeping every summary forever would eventually let the
		// supposedly bounded core block consume the whole window.
		j.coreMemories = []string{identity}
	}
	if len(c.Ledger) > 0 {
		j.appendLedger(c.Ledger)
	}
	if firstKept < 0 {
		firstKept = 0
	}
	if firstKept > len(j.entries) {
		firstKept = len(j.entries)
	}
	j.entries = append([]Entry(nil), j.entries[firstKept:]...)
	j.consolidations++
}

// Clear resets to a fresh journal, keeping the owner and the calibrated ledger.
func (j *Journal) Clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = nil
	j.coreMemories = nil
	j.episodicLedger = nil
	j.consolidations = 0
	j.lastHeard = make(map[string]string)
}

// --------------------------------------------------------------- persistence

// Snapshot is the serializable form of a Journal: everything needed to restore a
// life, and nothing that cannot be written to durable storage.
type Snapshot struct {
	Owner          string   `json:"owner"`
	Entries        []Entry  `json:"entries"`
	CoreMemories   []string `json:"core_memories"`
	EpisodicLedger []string `json:"episodic_ledger"`
	Consolidations int      `json:"consolidations"`
	CharsPerToken  float64  `json:"chars_per_token"`
}

// Snapshot captures the journal's current state.
func (j *Journal) Snapshot() Snapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return Snapshot{
		Owner:          j.owner,
		Entries:        append([]Entry(nil), j.entries...),
		CoreMemories:   append([]string(nil), j.coreMemories...),
		EpisodicLedger: append([]string(nil), j.episodicLedger...),
		Consolidations: j.consolidations,
		CharsPerToken:  j.ledger.CharsPerToken(),
	}
}

// FromSnapshot restores a journal, tolerating older or partial snapshots:
// missing fields degrade rather than fail, so a snapshot written before the
// episodic ledger existed restores with an empty ledger and starts one at its
// next consolidation.
func FromSnapshot(s Snapshot) *Journal {
	owner := s.Owner
	if owner == "" {
		owner = "unknown"
	}
	cpt := s.CharsPerToken
	if cpt == 0 {
		cpt = defaultCharsPerToken
	}
	j := New(owner, NewTokenLedger(cpt))
	j.coreMemories = append([]string(nil), s.CoreMemories...)
	for _, line := range s.EpisodicLedger {
		if strings.TrimSpace(line) != "" {
			j.episodicLedger = append(j.episodicLedger, line)
		}
	}
	j.consolidations = s.Consolidations
	for _, e := range s.Entries {
		if e.Kind == "" {
			e.Kind = KindOwn
		}
		j.entries = append(j.entries, e)
		if e.Kind == KindHeard {
			// Restore the redelivery-dedupe state too, or the first turn after
			// a restore re-journals whatever is still on the bus.
			j.lastHeard[e.Speaker] = e.Text
		}
	}
	return j
}

// MarshalJSON implements json.Marshaler via Snapshot.
func (j *Journal) MarshalJSON() ([]byte, error) {
	return json.Marshal(j.Snapshot())
}

// UnmarshalJSON implements json.Unmarshaler via FromSnapshot.
func (j *Journal) UnmarshalJSON(data []byte) error {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	restored := FromSnapshot(s)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.owner = restored.owner
	j.ledger = restored.ledger
	j.entries = restored.entries
	j.coreMemories = restored.coreMemories
	j.episodicLedger = restored.episodicLedger
	j.consolidations = restored.consolidations
	j.lastHeard = restored.lastHeard
	return nil
}
