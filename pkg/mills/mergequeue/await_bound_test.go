package mergequeue

import (
	"testing"
	"time"
)

// The await bound resolves hot-reloaded policy first, then the static
// override, then the compiled default — and a zero from either override
// never shadows a lower tier.
func TestProcessor_AwaitPipelineMaxResolution(t *testing.T) {
	cases := []struct {
		name   string
		static time.Duration
		fn     func() time.Duration
		want   time.Duration
	}{
		{name: "default", want: defaultAwaitPipeline},
		{name: "static override", static: 30 * time.Minute, want: 30 * time.Minute},
		{name: "policy wins over static", static: 30 * time.Minute, fn: func() time.Duration { return 2 * time.Hour }, want: 2 * time.Hour},
		{name: "zero policy falls back to static", static: 30 * time.Minute, fn: func() time.Duration { return 0 }, want: 30 * time.Minute},
		{name: "zero policy and zero static fall back to default", fn: func() time.Duration { return 0 }, want: defaultAwaitPipeline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Processor{AwaitPipeline: tc.static, AwaitPipelineFn: tc.fn}
			if got := p.awaitPipelineMax(); got != tc.want {
				t.Fatalf("awaitPipelineMax() = %v, want %v", got, tc.want)
			}
		})
	}
}
