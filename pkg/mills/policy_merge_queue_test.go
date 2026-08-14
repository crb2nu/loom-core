package mills

import "testing"

// The queue must be OFF for a nil policy, an omitted section, and a frozen
// mills (global kill switch), and ON only with an explicit enable.
func TestMergeQueueEnabled(t *testing.T) {
	var nilPolicy *Policy
	if nilPolicy.MergeQueueEnabled() {
		t.Fatalf("nil policy must not enable the merge queue")
	}

	p := &Policy{}
	if p.MergeQueueEnabled() {
		t.Fatalf("omitted merge_queue section must default off")
	}

	p.MergeQueue.Enabled = true
	if !p.MergeQueueEnabled() {
		t.Fatalf("explicit enable must turn the queue on")
	}

	// The global kill switch freezes the queue too.
	off := false
	p.Enabled = &off
	if p.MergeQueueEnabled() {
		t.Fatalf("a frozen mills must freeze the merge queue")
	}
}

func TestMergeQueueMaxDepth(t *testing.T) {
	var nilPolicy *Policy
	if got := nilPolicy.MergeQueueMaxDepth(); got != DefaultMergeQueueMaxDepth {
		t.Fatalf("nil policy depth = %d, want default %d", got, DefaultMergeQueueMaxDepth)
	}
	p := &Policy{}
	if got := p.MergeQueueMaxDepth(); got != DefaultMergeQueueMaxDepth {
		t.Fatalf("zero depth = %d, want default %d", got, DefaultMergeQueueMaxDepth)
	}
	p.MergeQueue.MaxDepth = 3
	if got := p.MergeQueueMaxDepth(); got != 3 {
		t.Fatalf("explicit depth = %d, want 3", got)
	}
}
