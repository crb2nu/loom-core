package mills

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// knitterEvidence is a failure shape no live classifier recognises: the same
// sentence with a different shard, timeout, token, and path each time. Two
// occurrences must normalize identically and cluster; a different shape must
// not join them.
func knitterEvidence(shard int, token string) string {
	return fmt.Sprintf(
		"fatal: knitter sidecar refused sync token for shard %d after %ds (token %s, spool /var/spool/knit/%d.q)",
		shard, shard*3, token, shard)
}

// seedEscalationEvidence writes one escalated run whose last stage carries the
// given log tail. classified stamps the escalation columns, which is how a run
// the live classifiers already explained is recorded.
func seedEscalationEvidence(t *testing.T, env *recTestEnv, id string, startedAt time.Time, evidence string, classified bool) {
	t.Helper()
	ctx := context.Background()
	if err := env.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: id, Title: "mined " + id, State: store.BacklogEscalated,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", id, err)
	}
	run := &store.PipelineRun{
		ID: "RUN-" + id, BacklogID: id, Template: "t",
		State: store.PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
		StartedAt: startedAt,
	}
	if classified {
		run.EscalationClass = "config"
		run.FailureClass = "configuration"
		run.ExternalDependencyID = "external_dependency.gitlab.auth_failure"
		run.ExternalDependency = "gitlab"
	}
	if err := env.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run %s: %v", run.ID, err)
	}
	outcome := store.StageOutcomeError
	if err := env.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "ci_watch", Attempt: 1,
		StartedAt: startedAt, Outcome: &outcome, LogTail: evidence,
	}); err != nil {
		t.Fatalf("put stage %s: %v", run.ID, err)
	}
}

// armMiner wires a mining sweep whose "already classified" predicate is a
// caller-supplied stub. Tests that must prove the exclusion against the REAL
// classifier corpus live in reconciler_signature_mining_classifier_test.go.
func armMiner(env *recTestEnv, classified func(string) bool) {
	if classified == nil {
		classified = func(string) bool { return false }
	}
	env.rec.SignatureEvidenceClassified = classified
}

func firstCandidate(t *testing.T, env *recTestEnv) *store.Event {
	t.Helper()
	events, err := env.store.Events.ListByActorSince(context.Background(),
		SignatureMinerActor, env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("candidate events = %d, want exactly 1: %+v", len(events), events)
	}
	return events[0]
}

// TestSignatureMiningClustersRecurringShape: three escalations sharing a shape
// become one candidate carrying the shared phrase, its support, bounded sample
// evidence, and the shadow match count.
func TestSignatureMiningClustersRecurringShape(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-KNIT-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, fmt.Sprintf("4f3a2b1c9d8e7f6%d", i)), false)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.TextsScanned != 3 || res.Unclassified != 3 {
		t.Fatalf("scanned/unclassified = %d/%d, want 3/3 (%+v)", res.TextsScanned, res.Unclassified, res)
	}
	if res.Candidates != 1 || res.Clustered != 3 {
		t.Fatalf("candidates/clustered = %d/%d, want 1/3 (%+v)", res.Candidates, res.Clustered, res)
	}

	ev := firstCandidate(t, env)
	if ev.Kind != SignatureCandidateEventKind {
		t.Errorf("kind = %q, want %q", ev.Kind, SignatureCandidateEventKind)
	}
	if ev.SubjectKind != signatureCandidateSubjectKind || ev.SubjectID == "" {
		t.Errorf("subject = (%q, %q), want a %q fingerprint", ev.SubjectKind, ev.SubjectID, signatureCandidateSubjectKind)
	}
	phrase, _ := ev.Payload["phrase"].(string)
	// Pinned exactly: the proposal is what a human reviews, so the phrase is
	// the longest 8-token run the three escalations share, with the shard
	// number, timeout, token, and spool path already collapsed away.
	const wantPhrase = "fatal knitter sidecar refused sync token for shard"
	if phrase != wantPhrase {
		t.Errorf("phrase = %q, want %q", phrase, wantPhrase)
	}
	if words := strings.Fields(phrase); len(words) != signatureMaxPhraseTokens {
		t.Errorf("phrase length = %d tokens, want the %d-token maximum", len(words), signatureMaxPhraseTokens)
	}
	if got := eventInt(t, ev, "member_count"); got != 3 {
		t.Errorf("member_count = %d, want 3", got)
	}
	if got := eventInt(t, ev, "window_match_count"); got != 3 {
		t.Errorf("window_match_count = %d, want 3", got)
	}
	samples, _ := ev.Payload["sample_evidence"].([]any)
	if len(samples) != 3 {
		t.Fatalf("sample_evidence = %d entries, want 3: %+v", len(samples), ev.Payload["sample_evidence"])
	}
	for _, sample := range samples {
		s, _ := sample.(string)
		if len(s) > signatureSampleMaxChars {
			t.Errorf("sample of %d chars exceeds the %d-char cap", len(s), signatureSampleMaxChars)
		}
	}
	if _, ok := ev.Payload["first_seen"].(string); !ok {
		t.Errorf("first_seen = %v, want an RFC3339 string", ev.Payload["first_seen"])
	}
	if _, ok := ev.Payload["last_seen"].(string); !ok {
		t.Errorf("last_seen = %v, want an RFC3339 string", ev.Payload["last_seen"])
	}
}

// TestSignatureMiningIgnoresPairs: two occurrences are a coincidence. The
// minimum cluster size is the whole guard against proposing noise.
func TestSignatureMiningIgnoresPairs(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 2; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-PAIR-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 0 {
		t.Fatalf("candidates = %d, want 0 for a pair (%+v)", res.Candidates, res)
	}
	events, err := env.store.Events.ListByActorSince(context.Background(),
		SignatureMinerActor, env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("candidate events = %d, want 0", len(events))
	}
}

// TestSignatureMiningKeepsDistinctShapesApart: three of one shape and three of
// another produce two candidates, not one merged proposal.
func TestSignatureMiningKeepsDistinctShapesApart(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-KNIT-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-SPOOL-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour),
			fmt.Sprintf("panic: spooler wedged waiting on inode lease %d for 45s", i), false)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Candidates != 2 {
		t.Fatalf("candidates = %d, want 2 (%+v)", res.Candidates, res)
	}
	events, err := env.store.Events.ListByActorSince(context.Background(),
		SignatureMinerActor, env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	phrases := map[string]bool{}
	for _, ev := range events {
		phrase, _ := ev.Payload["phrase"].(string)
		phrases[phrase] = true
		if got := eventInt(t, ev, "member_count"); got != 3 {
			t.Errorf("member_count = %d for %q, want 3", got, phrase)
		}
	}
	if len(phrases) != 2 {
		t.Fatalf("distinct phrases = %d, want 2: %v", len(phrases), phrases)
	}
}

func TestSignatureMiningStopPhrases(t *testing.T) {
	tests := []struct {
		name   string
		phrase string
		want   bool
	}{
		{name: "normalized generic command", phrase: "go test <path> <path>", want: true},
		{name: "case insensitive", phrase: "GO TEST ./pkg/mills ./pkg/store", want: true},
		{name: "punctuation insensitive", phrase: "Go test: ./pkg/mills, ./pkg/store", want: true},
		{name: "distinctive failure", phrase: "fatal knitter sidecar refused sync token", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSignatureMiningStopPhrase(tc.phrase); got != tc.want {
				t.Errorf("isSignatureMiningStopPhrase(%q) = %v, want %v", tc.phrase, got, tc.want)
			}
		})
	}
}

// TestSignatureMiningRejectsStopPhraseBeforePersistence proves a generic
// recurring test command may still form a cluster, but never becomes a stored
// candidate. Filtering after clustering preserves the miner's grouping rules.
func TestSignatureMiningRejectsStopPhraseBeforePersistence(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-GENERIC-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour),
			fmt.Sprintf("go test ./pkg/component%d ./pkg/store%d", i, i), false)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Clustered != 3 || res.Candidates != 0 {
		t.Fatalf("clustered/candidates = %d/%d, want 3/0 (%+v)", res.Clustered, res.Candidates, res)
	}
	events, err := env.store.Events.ListByActorSince(context.Background(),
		SignatureMinerActor, env.now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("candidate events = %d, want 0: %+v", len(events), events)
	}
}

// TestSignatureMiningExcludesColumnClassified: a run whose escalation columns
// already carry a class is explained, so it is never mined — even though its
// evidence text repeats. This is the durable half of the exclusion; the
// classifier half is proven in reconciler_signature_mining_classifier_test.go.
func TestSignatureMiningExcludesColumnClassified(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-CLASSIFIED-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), true)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.TextsScanned != 3 {
		t.Errorf("scanned = %d, want 3 — classified runs still count toward the shadow window", res.TextsScanned)
	}
	if res.Unclassified != 0 || res.Candidates != 0 {
		t.Fatalf("unclassified/candidates = %d/%d, want 0/0 (%+v)", res.Unclassified, res.Candidates, res)
	}
}

// TestSignatureMiningShadowCountSpansWholeWindow: the shadow evaluation scores
// a proposed phrase against every escalation in the window, including the ones
// a classifier already explains. A phrase that would also fire on explained
// failures is a phrase that would misclassify once promoted, and the reviewer
// has to be able to see that BEFORE writing the rule.
func TestSignatureMiningShadowCountSpansWholeWindow(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-SHADOW-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
	}
	// Same shape, but already classified: mined out of the corpus, still
	// counted by the shadow evaluation.
	seedEscalationEvidence(t, env, "MILLS-SHADOW-KNOWN", env.now.Add(-4*time.Hour),
		knitterEvidence(9, "90ab12cd34ef5678"), true)

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Unclassified != 3 || res.Candidates != 1 {
		t.Fatalf("unclassified/candidates = %d/%d, want 3/1 (%+v)", res.Unclassified, res.Candidates, res)
	}
	ev := firstCandidate(t, env)
	if got := eventInt(t, ev, "member_count"); got != 3 {
		t.Errorf("member_count = %d, want 3", got)
	}
	if got := eventInt(t, ev, "window_match_count"); got != 4 {
		t.Errorf("window_match_count = %d, want 4 — the classified escalation matches too", got)
	}
}

// TestSignatureMiningProposesExactlyOnce: a second pass over the same window
// re-derives the same cluster and writes nothing. The window is bounded by
// wall-clock time, so re-observation is the normal case.
func TestSignatureMiningProposesExactlyOnce(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-ONCE-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
	}

	before := testutil.ToFloat64(SignatureCandidatesTotal)
	first, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.Candidates != 1 {
		t.Fatalf("first sweep candidates = %d, want 1", first.Candidates)
	}
	second, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Candidates != 0 {
		t.Errorf("second sweep candidates = %d, want 0 (%+v)", second.Candidates, second)
	}
	if second.Unclassified != 3 {
		t.Errorf("second sweep unclassified = %d, want 3 — the cluster is still derived", second.Unclassified)
	}
	if got := testutil.ToFloat64(SignatureCandidatesTotal) - before; got != 1 {
		t.Errorf("candidate counter delta = %v, want 1", got)
	}
	firstCandidate(t, env)
}

// TestSignatureMiningWindowBoundsCorpus: an escalation older than the lookback
// is not evidence for a candidate proposed today.
func TestSignatureMiningWindowBoundsCorpus(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	env.rec.SignatureMiningLookback = 48 * time.Hour
	for i := 1; i <= 2; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-WIN-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
	}
	seedEscalationEvidence(t, env, "MILLS-WIN-OLD", env.now.Add(-72*time.Hour),
		knitterEvidence(3, "4f3a2b1c9d8e7f60"), false)

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.TextsScanned != 2 {
		t.Errorf("scanned = %d, want 2 — the 72h-old escalation is outside a 48h window", res.TextsScanned)
	}
	if res.Candidates != 0 {
		t.Errorf("candidates = %d, want 0", res.Candidates)
	}
}

// TestSignatureMiningDisabledWithoutClassifier: without the real classifier the
// miner cannot tell an explained failure from an unexplained one, so it is off
// rather than guessing.
func TestSignatureMiningDisabledWithoutClassifier(t *testing.T) {
	env := newRecEnv(t, nil)
	for i := 1; i <= 3; i++ {
		seedEscalationEvidence(t, env, fmt.Sprintf("MILLS-OFF-%d", i),
			env.now.Add(-time.Duration(i)*time.Hour), knitterEvidence(i, "4f3a2b1c9d8e7f60"), false)
	}

	res, err := env.rec.SweepSignatureMining(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res != (SignatureMiningSweepResult{}) {
		t.Errorf("result = %+v, want zero value", res)
	}
	if env.rec.signatureMiningDue(env.now) {
		t.Error("sweep reported due with no classifier wired")
	}
}

// TestSignatureMiningDueRespectsInterval: the reconciler rate-limits the sweep
// rather than re-deriving a two-week corpus on every tick.
func TestSignatureMiningDueRespectsInterval(t *testing.T) {
	env := newRecEnv(t, nil)
	armMiner(env, nil)
	env.rec.SignatureMiningInterval = 90 * time.Minute

	if !env.rec.signatureMiningDue(env.now) {
		t.Fatal("first sweep must be due immediately")
	}
	env.rec.nextSignatureMining = env.now.Add(env.rec.signatureMiningInterval())
	if env.rec.signatureMiningDue(env.now.Add(89 * time.Minute)) {
		t.Error("sweep due before the interval elapsed")
	}
	if !env.rec.signatureMiningDue(env.now.Add(90 * time.Minute)) {
		t.Error("sweep not due after the interval elapsed")
	}
	if got := (&Reconciler{}).signatureMiningInterval(); got != DefaultSignatureMiningInterval {
		t.Errorf("default interval = %s, want %s", got, DefaultSignatureMiningInterval)
	}
	if got := (&Reconciler{}).signatureMiningLookback(); got != defaultSignatureMiningLookback {
		t.Errorf("default lookback = %s, want %s", got, defaultSignatureMiningLookback)
	}
}

// TestTruncateSignatureSample bounds a snippet without splitting a rune.
func TestTruncateSignatureSample(t *testing.T) {
	short := "fatal: knitter refused"
	if got := truncateSignatureSample(short); got != short {
		t.Errorf("short sample was altered: %q", got)
	}
	long := strings.Repeat("x", signatureSampleMaxChars+50)
	if got := truncateSignatureSample(long); len(got) != signatureSampleMaxChars {
		t.Errorf("long sample = %d chars, want %d", len(got), signatureSampleMaxChars)
	}
	multibyte := strings.Repeat("a", signatureSampleMaxChars-1) + "é" + "tail"
	got := truncateSignatureSample(multibyte)
	if !utf8Valid(got) {
		t.Errorf("truncation split a rune: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func eventInt(t *testing.T, ev *store.Event, key string) int64 {
	t.Helper()
	switch v := ev.Payload[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		t.Fatalf("payload %q = %v (%T), want a number", key, ev.Payload[key], ev.Payload[key])
		return 0
	}
}
