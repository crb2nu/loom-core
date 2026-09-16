package runner

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/mills"
)

func TestRunnerConcurrencyPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy *mills.Policy
		want   int
	}{
		{name: "unset uses previous default", policy: &mills.Policy{}, want: loomconcurrency.DefaultLimit},
		{name: "explicit override", policy: &mills.Policy{MaxConcurrentPipelines: concurrencyLimitPtr(3)}, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{}
			if err := r.acquireConcurrency(t.Context(), tc.policy); err != nil {
				t.Fatal(err)
			}
			defer r.concurrency.Release()
			if got := r.concurrency.Limit(); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunnerConcurrencyPolicyRejectsInvalidLimit(t *testing.T) {
	r := &Runner{}
	err := r.acquireConcurrency(t.Context(), &mills.Policy{MaxConcurrentPipelines: concurrencyLimitPtr(-1)})
	if err == nil {
		t.Fatal("expected invalid concurrency policy to be rejected")
	}
	if r.concurrency != nil {
		t.Fatal("invalid concurrency policy initialized the limiter")
	}
}

func TestRunnerConcurrencyPolicyEnforcedAtAdmission(t *testing.T) {
	r := &Runner{}
	policy := &mills.Policy{MaxConcurrentPipelines: concurrencyLimitPtr(1)}
	if err := r.acquireConcurrency(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	defer r.concurrency.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.acquireConcurrency(ctx, policy); err == nil {
		t.Fatal("second admission exceeded policy limit")
	}
}

func TestRunnerConcurrencyPolicyReloadChangesAdmissionLimit(t *testing.T) {
	r := &Runner{}
	limitOne := &mills.Policy{MaxConcurrentPipelines: concurrencyLimitPtr(1)}
	if err := r.acquireConcurrency(t.Context(), limitOne); err != nil {
		t.Fatal(err)
	}

	limitTwo := &mills.Policy{MaxConcurrentPipelines: concurrencyLimitPtr(2)}
	if err := r.acquireConcurrency(t.Context(), limitTwo); err != nil {
		t.Fatalf("raised reloaded limit did not admit second run: %v", err)
	}
	if got := r.concurrency.Limit(); got != 2 {
		t.Fatalf("reloaded limit = %d, want 2", got)
	}
	r.concurrency.Release()
	r.concurrency.Release()

	if err := r.acquireConcurrency(t.Context(), limitOne); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.acquireConcurrency(ctx, limitOne); err == nil {
		t.Fatal("lowered reloaded limit admitted too much work")
	}
	r.concurrency.Release()
}

// concurrencyLimitPtr builds the explicit-limit form. PipelineConcurrencyPolicy
// carries *int precisely so "unset" and "explicitly zero" stay distinguishable.
func concurrencyLimitPtr(limit int) *int { return &limit }
