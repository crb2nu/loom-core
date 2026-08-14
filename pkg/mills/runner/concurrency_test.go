package runner

import (
	"testing"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

func TestRunnerConcurrencyPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy sharedpolicy.PipelineConcurrencyPolicy
		want   int
	}{
		{name: "unset uses previous default", want: loomconcurrency.DefaultLimit},
		{name: "explicit override", policy: sharedpolicy.PipelineConcurrencyPolicy{Limit: concurrencyLimitPtr(3)}, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{ConcurrencyPolicy: tc.policy}
			if err := r.acquireConcurrency(t.Context()); err != nil {
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
	r := &Runner{ConcurrencyPolicy: sharedpolicy.PipelineConcurrencyPolicy{Limit: concurrencyLimitPtr(-1)}}
	err := r.acquireConcurrency(t.Context())
	if err == nil {
		t.Fatal("expected invalid concurrency policy to be rejected")
	}
	if r.concurrency != nil {
		t.Fatal("invalid concurrency policy initialized the limiter")
	}
}

// concurrencyLimitPtr builds the explicit-limit form. PipelineConcurrencyPolicy
// carries *int precisely so "unset" and "explicitly zero" stay distinguishable.
func concurrencyLimitPtr(limit int) *int { return &limit }
