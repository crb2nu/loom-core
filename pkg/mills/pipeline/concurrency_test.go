package pipeline

import (
	"testing"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

type recordingConcurrencyLimiter struct {
	limit int
	calls int
}

func (l *recordingConcurrencyLimiter) SetConcurrencyLimit(limit int) {
	l.limit = limit
	l.calls++
}

func TestConfigureConcurrency(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		limiter := &recordingConcurrencyLimiter{}
		if err := ConfigureConcurrency(limiter, sharedpolicy.PipelineConcurrencyPolicy{}); err != nil {
			t.Fatal(err)
		}
		if limiter.limit != loomconcurrency.DefaultLimit || limiter.calls != 1 {
			t.Fatalf("limiter = {limit:%d calls:%d}, want {%d 1}", limiter.limit, limiter.calls, loomconcurrency.DefaultLimit)
		}
	})

	t.Run("configured", func(t *testing.T) {
		limiter := &recordingConcurrencyLimiter{}
		limit := 2
		if err := ConfigureConcurrency(limiter, sharedpolicy.PipelineConcurrencyPolicy{MaxConcurrentPipelines: &limit}); err != nil {
			t.Fatal(err)
		}
		if limiter.limit != 2 || limiter.calls != 1 {
			t.Fatalf("limiter = {limit:%d calls:%d}, want {2 1}", limiter.limit, limiter.calls)
		}
	})

	t.Run("invalid is not applied", func(t *testing.T) {
		limiter := &recordingConcurrencyLimiter{limit: 7}
		for _, limit := range []int{0, -1, loomconcurrency.MaxLimit + 1} {
			err := ConfigureConcurrency(limiter, sharedpolicy.PipelineConcurrencyPolicy{MaxConcurrentPipelines: &limit})
			if err == nil {
				t.Fatalf("limit %d: expected validation error", limit)
			}
			if limiter.limit != 7 || limiter.calls != 0 {
				t.Fatalf("limit %d changed limiter: %+v", limit, limiter)
			}
		}
	})
}
