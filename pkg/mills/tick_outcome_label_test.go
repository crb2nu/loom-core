package mills

import "testing"

func TestTickOutcomeLabel(t *testing.T) {
	tests := []struct {
		name string
		res  TickResult
		want string
	}{
		{
			name: "errored takes precedence over started",
			res:  TickResult{Errored: 1, Started: 1},
			want: "errored",
		},
		{
			name: "started takes precedence over deferred",
			res:  TickResult{Started: 1, Deferred: 1},
			want: "started_one",
		},
		{
			name: "deferred takes precedence over skipped",
			res:  TickResult{Deferred: 1, Skipped: 1},
			want: "deferred",
		},
		{
			name: "skipped",
			res:  TickResult{Skipped: 1},
			want: "skipped",
		},
		{
			name: "all zero",
			res:  TickResult{},
			want: "no_op",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tickOutcomeLabel(tt.res); got != tt.want {
				t.Fatalf("tickOutcomeLabel(%+v) = %q, want %q", tt.res, got, tt.want)
			}
		})
	}
}
