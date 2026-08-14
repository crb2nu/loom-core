package textsim

import "testing"

func TestNormalizeTitleTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Add HUD panel", []string{"add", "hud", "panel"}},
		{"add the new HUD panel", []string{"add", "new", "hud", "panel"}}, // "the" dropped
		{"a b c d", nil}, // tokens shorter than 2 dropped
		{"Reconciler-idle/backoff!", []string{"reconciler", "idle", "backoff"}},
		{"foo foo bar", []string{"foo", "bar"}}, // dedupe within title
	}
	for _, tc := range cases {
		got := NormalizeTitleTokens(tc.in)
		if !equalStrings(got, tc.want) {
			t.Errorf("normalize(%q) = %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestJaccard(t *testing.T) {
	cases := []struct {
		name  string
		a, b  []string
		want  float64
		delta float64
	}{
		{"identical", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 1.0, 0.001},
		{"disjoint", []string{"a", "b"}, []string{"c", "d"}, 0.0, 0.001},
		{"half overlap", []string{"a", "b"}, []string{"a", "c"}, 1.0 / 3.0, 0.001}, // |∩|=1, |∪|=3
		{"empty A", nil, []string{"a"}, 0, 0.001},
		{"empty B", []string{"a"}, nil, 0, 0.001},
	}
	for _, tc := range cases {
		got := Jaccard(tc.a, tc.b)
		if abs(got-tc.want) > tc.delta {
			t.Errorf("%s: Jaccard(%v,%v)=%v want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTitleJaccard(t *testing.T) {
	if got := TitleJaccard("", "Add HUD panel"); got != 0 {
		t.Errorf("empty title = %v want 0", got)
	}
	if got := TitleJaccard("Add the HUD panel", "add HUD panel"); got != 1.0 {
		t.Errorf("stopword-only difference = %v want 1.0", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestNormalizeWorkTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain title untouched", "Ground council proposals in merged work", "Ground council proposals in merged work"},
		{"conventional prefix with scope", "feat(mills): council grounds proposals", "council grounds proposals"},
		{"conventional prefix bare", "docs: tick off overnight deliveries", "tick off overnight deliveries"},
		{"breaking-change marker", "refactor(mills/council)!: split the mutator", "split the mutator"},
		{"draft lead", "Draft: feat(daemon): wire otel export", "wire otel export"},
		{"bracketed lead", "[scope-escalated] Harden the classifier", "Harden the classifier"},
		{
			"plan-slice decoration",
			"Wire config-gated OTel trace export into the daemon — daemon-otel-export",
			"Wire config-gated OTel trace export into the daemon",
		},
		{
			"item slug token",
			"psl-plan-council-harden-mills-failure-classifier-1 Harden the failure classifier",
			"Harden the failure classifier",
		},
		{"pipeline run slug token", "PIPE-psl-plan-council-add-a-runbook-1 Add a runbook", "Add a runbook"},
		// A trailing clause carrying whitespace is authored content, not a
		// slug: dropping it would discard the tokens separating two proposals.
		{
			"real trailing clause kept",
			"Harden the classifier — fail closed on external deps",
			"Harden the classifier — fail closed on external deps",
		},
		// A mid-title colon is a sentence, not a conventional-commit prefix.
		{"mid-title colon kept", "Fail closed: the classifier must not guess", "Fail closed: the classifier must not guess"},
		{"unbalanced scope kept", "feat(mills: broken scope", "feat(mills: broken scope"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeWorkTitle(tc.in); got != tc.want {
				t.Errorf("NormalizeWorkTitle(%q) = %q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestWorkTitleJaccardSeesThroughDecoration is the gap grounding exists to
// close: the council proposes a title, the mill ships it as an MR wearing a
// conventional-commit prefix and a plan-slice slug, and the raw comparison
// scores that pair too low to suppress the re-proposal.
func TestWorkTitleJaccardSeesThroughDecoration(t *testing.T) {
	proposal := "Add a Grafana panel and alert for the embedder"
	merged := "feat(hud): add embedder Grafana panel and alert — embedder-alerting"

	raw := TitleJaccard(proposal, merged)
	if raw >= 0.7 {
		t.Fatalf("fixture no longer demonstrates the gap: raw TitleJaccard = %v, want < 0.7", raw)
	}
	if got := WorkTitleJaccard(proposal, merged); got < 0.99 {
		t.Errorf("WorkTitleJaccard = %v, want ~1 after normalization (raw was %v)", got, raw)
	}
}
