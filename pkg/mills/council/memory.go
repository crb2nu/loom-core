package council

import (
	"context"
	"os"
	"strings"

	"github.com/crb2nu/loom/pkg/journalengine"
)

// MemoryEnv gates the council lane's durable cross-run memory: the runner's
// record hook and the editor prompt's render hook both read it, so one variable
// turns the whole feature on or off. Default OFF — with it unset the editor
// prompt is byte-identical to the pre-feature prompt.
const MemoryEnv = "LOOM_MILLS_COUNCIL_MEMORY"

// MemoryPreface labels the memory block inside the editor prompt. It is a
// constant for the same reason itemJournalPreface is: any per-run text here
// would sit above the now-block boundary and void the warm prefix.
const MemoryPreface = "WHAT THIS COUNCIL HAS ALREADY DECIDED IN PAST RUNS (recorded by the runner, in order — treat it as fact, not as your own recollection; do NOT re-propose work already minted below):"

// MemoryEnabled reports whether the council lane's durable memory is on.
func MemoryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(MemoryEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// MemoryLoader loads the council lane's durable journal. *store.CouncilMemoryDAO
// satisfies it; the interface keeps pkg/mills/clients free of a store dependency
// in the editor path and lets prompt tests supply a journal without a database.
type MemoryLoader interface {
	Get(ctx context.Context) (*journalengine.Journal, error)
}

// MemoryBlock renders the council lane's durable memory for the STABLE half of
// the editor prompt. Empty string when the feature is off, no loader is wired,
// the lane has no memory yet, or the load fails — a memory read must never
// block a council run, and an empty block keeps the prompt byte-identical to
// the pre-feature prompt.
func MemoryBlock(ctx context.Context, mem MemoryLoader) string {
	if mem == nil || !MemoryEnabled() {
		return ""
	}
	j, err := mem.Get(ctx)
	if err != nil || j == nil {
		return ""
	}
	rendered := j.Render()
	if rendered == "" || rendered == journalengine.EmptyJournal {
		return ""
	}
	return MemoryPreface + "\n" + rendered
}
