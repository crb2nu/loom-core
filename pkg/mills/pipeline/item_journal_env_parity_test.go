package pipeline

import (
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
)

// TestItemJournalEnvParity pins the auto-requeue sweep's inlined env name to
// the pipeline package's authoritative constant (pkg/mills cannot import
// pipeline — pipeline imports mills). If these drift, code/config auto-requeue
// would read a dead variable and silently never fire.
func TestItemJournalEnvParity(t *testing.T) {
	if mills.ItemJournalEnvName != ItemJournalEnv {
		t.Fatalf("mills.ItemJournalEnvName %q != pipeline.ItemJournalEnv %q",
			mills.ItemJournalEnvName, ItemJournalEnv)
	}
}
