package mills

import (
	"testing"
	"time"
)

func TestMergeQueueSpeculationDepthDefaultsOffAndBoundsToQueue(t *testing.T) {
	var nilPolicy *Policy
	if got := nilPolicy.MergeQueueSpeculationDepth(); got != 0 {
		t.Fatalf("nil depth = %d", got)
	}
	p := &Policy{MergeQueue: MergeQueuePolicy{MaxDepth: 3, SpeculationDepth: 7}}
	if got := p.MergeQueueSpeculationDepth(); got != 3 {
		t.Fatalf("bounded depth = %d, want 3", got)
	}
	p.MergeQueue.SpeculationDepth = 2
	if got := p.MergeQueueSpeculationDepth(); got != 2 {
		t.Fatalf("configured depth = %d, want 2", got)
	}
}

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

// The await bound is opt-in: nil/omitted/zero/negative yield 0 so the
// processor keeps its compiled default; an explicit value is minutes.
func TestMergeQueueAwaitPipeline(t *testing.T) {
	var nilPolicy *Policy
	if got := nilPolicy.MergeQueueAwaitPipeline(); got != 0 {
		t.Fatalf("nil policy await = %v, want 0", got)
	}
	p := &Policy{}
	if got := p.MergeQueueAwaitPipeline(); got != 0 {
		t.Fatalf("omitted await = %v, want 0", got)
	}
	p.MergeQueue.AwaitPipelineMinutes = -5
	if got := p.MergeQueueAwaitPipeline(); got != 0 {
		t.Fatalf("negative await = %v, want 0", got)
	}
	p.MergeQueue.AwaitPipelineMinutes = 120
	if got := p.MergeQueueAwaitPipeline(); got != 120*time.Minute {
		t.Fatalf("explicit await = %v, want 120m", got)
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
