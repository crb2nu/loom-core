package pipeline

import "testing"

// TestKnownFailureSignature pins the predicate to the real classifier corpus:
// one case per arm that must match, plus the unexplained failures the
// signature miner exists to surface. The false cases are the load-bearing
// half — ClassifyFailure fails closed to FailureCode for anything, so a
// predicate built on "has a class" would report true for every text and the
// miner would never propose anything.
func TestKnownFailureSignature(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "structured ci failure reason",
			text: `job 4471 ended: failure_reason: runner_system_failure`,
			want: true,
		},
		{
			name: "observed external incident",
			text: "clickhouse: failed to execute merge task on shard 3",
			want: true,
		},
		{
			name: "guard taxonomy signature",
			text: "dial tcp 10.0.0.4:9000: connect: ECONNREFUSED",
			want: true,
		},
		{
			name: "mcperror incident code table",
			text: "GET https://gitlab.example.com/api/v4/projects: 401 Unauthorized",
			want: true,
		},
		{
			name: "unexplained failure",
			text: "fatal: knitter sidecar refused sync token for shard 7 after 21s",
			want: false,
		},
		{
			name: "prose mentioning a reason is not structured evidence",
			text: "the job looked like a runner_system_failure but the runner was healthy",
			want: false,
		},
		{
			name: "empty text",
			text: "   ",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KnownFailureSignature(tc.text); got != tc.want {
				t.Errorf("KnownFailureSignature(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
