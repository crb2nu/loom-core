package hud

import (
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func TestFlattenSessionEntriesPreservesSessionID(t *testing.T) {
	entries := flattenSessionEntries([]bridge.ContextEntryInfo{{
		Entry: bridge.ContextEntry{
			ID:        "entry-1",
			SessionID: "session-1",
			EntryType: "finding",
		},
	}})

	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if got := entries[0]["session_id"]; got != "session-1" {
		t.Fatalf("session_id = %v, want session-1", got)
	}
}
