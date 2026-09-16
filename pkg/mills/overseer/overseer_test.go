package overseer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestPromotionGateDecide(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	generated := started.Add(S2SoakMinimumDuration)
	rfc := func(at time.Time) string { return at.Format(time.RFC3339) }
	artifact := func(generatedAt, startedAt time.Time, elapsed int64, divergences int) []byte {
		return []byte(fmt.Sprintf(`{"generated_at":%q,"soak_progress":{"soak_started_at":%q,"soak_elapsed_seconds":%d,"soak_divergences":%d}}`,
			rfc(generatedAt), rfc(startedAt), elapsed, divergences))
	}
	// A verbatim shift report: the soak projection surrounded by unrelated
	// fields plus a projected soak_complete the gate must not trust.
	shiftReport := func(elapsed int64, soakComplete bool) []byte {
		return []byte(fmt.Sprintf(`{"generated_at":%q,"window_seconds":86400,"bolts":[],"sparks":[],`+
			`"soak_progress":{"soak_started_at":%q,"soak_elapsed_seconds":%d,"soak_divergences":0},"soak_complete":%t,"markdown":"# Shift"}`,
			rfc(generated), rfc(started), elapsed, soakComplete))
	}

	tests := []struct {
		name       string
		contents   []byte
		readErr    error
		wantMode   PromotionMode
		wantReason string
	}{
		{name: "missing", readErr: os.ErrNotExist, wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMissing},
		{name: "unreadable", readErr: fs.ErrPermission, wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactUnreadable},
		{name: "malformed JSON", contents: []byte(`{"generated_at":`), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "malformed timestamp", contents: []byte(`{"generated_at":"yesterday","soak_progress":{}}`), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "missing soak progress", contents: []byte(fmt.Sprintf(`{"generated_at":%q}`, rfc(generated))), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "missing generated_at", contents: []byte(fmt.Sprintf(`{"soak_progress":{"soak_started_at":%q,"soak_elapsed_seconds":604800,"soak_divergences":0}}`, rfc(started))), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "missing soak start", contents: []byte(fmt.Sprintf(`{"generated_at":%q,"soak_progress":{"soak_elapsed_seconds":604800,"soak_divergences":0}}`, rfc(generated))), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "negative elapsed", contents: artifact(generated, started, -1, 0), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "negative divergences", contents: artifact(generated, started, S2SoakMinimumSeconds, -1), wantMode: PromotionModeDryRun, wantReason: PromotionReasonArtifactMalformed},
		{name: "future-dated soak start", contents: artifact(generated, generated.Add(time.Second), S2SoakMinimumSeconds, 0), wantMode: PromotionModeDryRun, wantReason: PromotionReasonSoakStartFutureDated},
		{name: "one second short", contents: artifact(generated, started, S2SoakMinimumSeconds-1, 0), wantMode: PromotionModeDryRun, wantReason: PromotionReasonSoakTooShort},
		{name: "diverged", contents: artifact(generated, started, S2SoakMinimumSeconds, 1), wantMode: PromotionModeDryRun, wantReason: PromotionReasonSoakDiverged},
		{name: "projected soak_complete is not trusted", contents: shiftReport(S2SoakMinimumSeconds-1, true), wantMode: PromotionModeDryRun, wantReason: PromotionReasonSoakTooShort},
		{name: "exact threshold passes", contents: artifact(generated, started, S2SoakMinimumSeconds, 0), wantMode: PromotionModeActive, wantReason: PromotionReasonSoakComplete},
		{name: "longer soak passes", contents: artifact(generated.Add(24*time.Hour), started, S2SoakMinimumSeconds+86400, 0), wantMode: PromotionModeActive, wantReason: PromotionReasonSoakComplete},
		{name: "verbatim shift report passes", contents: shiftReport(S2SoakMinimumSeconds, true), wantMode: PromotionModeActive, wantReason: PromotionReasonSoakComplete},
	}

	active := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".json")
			if tc.contents != nil {
				if err := os.WriteFile(path, tc.contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			gate := PromotionGate{ArtifactPath: path, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			if tc.readErr != nil {
				gate.ReadFile = func(string) ([]byte, error) { return nil, tc.readErr }
			}

			got := gate.Decide()
			if got.Mode != tc.wantMode || got.Reason != tc.wantReason {
				t.Fatalf("decision = %+v, want mode %q and reason %q", got, tc.wantMode, tc.wantReason)
			}
			if !strings.Contains(logs.String(), got.Reason) || !strings.Contains(logs.String(), string(got.Mode)) {
				t.Fatalf("log = %q, want mode and diagnostic reason", logs.String())
			}
			// The verdict is SoakProgress.CompleteAt and nothing else, so the
			// gate always agrees with the soak_complete projection the shift
			// report derives from the same evidence.
			var parsed SoakCompleteArtifact
			if tc.contents != nil && json.Unmarshal(tc.contents, &parsed) == nil {
				if want := parsed.SoakProgress.CompleteAt(parsed.GeneratedAt); (got.Mode == PromotionModeActive) != want {
					t.Fatalf("mode = %q, but SoakProgress.CompleteAt = %v", got.Mode, want)
				}
			}
			if got.Mode == PromotionModeActive {
				active++
			}
		})
	}
	if active != 3 {
		t.Fatalf("active fixtures = %d, want exactly three", active)
	}
}

type soakTelemetryStub struct{ persistErr error }

type soakStartStub struct {
	start time.Time
	calls int
}

func (s *soakStartStub) EnsureOverseerSoakStart(_ context.Context, candidate time.Time) (time.Time, error) {
	s.calls++
	if s.start.IsZero() {
		s.start = candidate
	}
	return s.start, nil
}

func TestObserveSoakProgressPersistsStableStart(t *testing.T) {
	first := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	persistence := &soakStartStub{}
	initial, err := ObserveSoakProgress(context.Background(), persistence, first, 0)
	if err != nil {
		t.Fatal(err)
	}
	later, err := ObserveSoakProgress(context.Background(), persistence, first.Add(25*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if initial.StartedAt != first || later.StartedAt != first || persistence.start != first {
		t.Fatalf("start changed: initial=%s later=%s persisted=%s", initial.StartedAt, later.StartedAt, persistence.start)
	}
	if later.ElapsedSeconds != 25*60*60 || later.Divergences != 2 || persistence.calls != 2 {
		t.Fatalf("later progress = %+v, persistence calls=%d", later, persistence.calls)
	}
}

func TestSoakProgressCompleteBoundariesAndMalformed(t *testing.T) {
	started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		progress *SoakProgress
		want     bool
	}{
		{name: "missing"},
		{name: "missing start", progress: &SoakProgress{ElapsedSeconds: S2SoakMinimumSeconds}},
		{name: "one second short", progress: &SoakProgress{StartedAt: started, ElapsedSeconds: 604799}},
		{name: "exact threshold", progress: &SoakProgress{StartedAt: started, ElapsedSeconds: 604800}, want: true},
		{name: "divergence", progress: &SoakProgress{StartedAt: started, ElapsedSeconds: 604800, Divergences: 1}},
		{name: "negative divergence", progress: &SoakProgress{StartedAt: started, ElapsedSeconds: 604800, Divergences: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.progress.Complete(); got != tc.want {
				t.Fatalf("Complete() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSoakProgressCompleteAtRejectsFutureDatedStart(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	progress := &SoakProgress{StartedAt: observedAt.Add(time.Second), ElapsedSeconds: S2SoakMinimumSeconds}
	if progress.CompleteAt(observedAt) {
		t.Fatal("future-dated soak start passed completion gate")
	}
}

func (s soakTelemetryStub) RecordOverseerSoakDecision(context.Context, time.Time, bool, bool) error {
	return s.persistErr
}

func (soakTelemetryStub) OverseerSoakTelemetry(context.Context, time.Time) ([]store.OverseerSoakDailyCounters, error) {
	return nil, nil
}

type dryRunDecisionRecorderStub struct {
	calls    int
	wouldAct bool
	diverged bool
}

func (r *dryRunDecisionRecorderStub) RecordOverseerDryRunDecision(_ context.Context, wouldAct, diverged bool) {
	r.calls++
	r.wouldAct = wouldAct
	r.diverged = diverged
}

func TestRecordDryRunDecisionEmitsOnceAfterPersistence(t *testing.T) {
	recorder := &dryRunDecisionRecorderStub{}
	restore := telemetry.SetOverseerDryRunDecisionRecorderForTest(recorder)
	t.Cleanup(restore)

	if err := RecordDryRunDecision(context.Background(), soakTelemetryStub{}, time.Now(), true, true); err != nil {
		t.Fatalf("record dry-run decision: %v", err)
	}
	if recorder.calls != 1 || !recorder.wouldAct || !recorder.diverged {
		t.Fatalf("metric calls = %d (%v, %v), want one (true, true)", recorder.calls, recorder.wouldAct, recorder.diverged)
	}
}

func TestRecordDryRunDecisionDoesNotEmitWhenPersistenceFails(t *testing.T) {
	recorder := &dryRunDecisionRecorderStub{}
	restore := telemetry.SetOverseerDryRunDecisionRecorderForTest(recorder)
	t.Cleanup(restore)

	wantErr := errors.New("persist failed")
	err := RecordDryRunDecision(context.Background(), soakTelemetryStub{persistErr: wantErr}, time.Now(), false, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if recorder.calls != 0 {
		t.Fatalf("metric calls = %d, want zero", recorder.calls)
	}
}

func TestEvaluateS2Soak(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	passing := func() *guard.PromotionReport {
		return &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 3, TotalDryRun: 3}
	}
	tests := []struct {
		name        string
		report      *guard.PromotionReport
		divergences int
		promotable  bool
		wantReason  string
	}{
		{name: "nil report", wantReason: "missing or unreadable"},
		{name: "malformed zero-value report", report: &guard.PromotionReport{}, wantReason: "window is missing or unreadable"},
		{name: "stale missing window start", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowEnd: now, TotalActions: 1, TotalDryRun: 1}, wantReason: "window is missing or unreadable"},
		{name: "invalid reversed window", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now, WindowEnd: now.Add(-time.Hour), TotalActions: 1, TotalDryRun: 1}, wantReason: "closed soak window"},
		{name: "short soak", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-6 * 24 * time.Hour), WindowEnd: now, TotalActions: 1, TotalDryRun: 1}, wantReason: "closed soak window"},
		{name: "empty evidence threshold", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, ZeroEvidence: true}, wantReason: "promotion evidence is empty"},
		{name: "divergence threshold breach", report: passing(), divergences: 1, wantReason: "divergence threshold exceeded"},
		{name: "inconsistent counters", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 2, TotalDryRun: 1}, wantReason: "promotion evidence is inconsistent"},
		{name: "negative dry-run counter", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: -1, TotalDryRun: -1}, wantReason: "promotion evidence is inconsistent"},
		{name: "negative reviewed divergences", report: passing(), divergences: -1, wantReason: "promotion evidence is inconsistent"},
		{name: "executed non-dry-run action", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 2, TotalDryRun: 1, TotalExecuted: 1}, wantReason: "divergence threshold exceeded"},
		{name: "wrong actor prefix", report: &guard.PromotionReport{ActorPrefix: "overseer", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 1, TotalDryRun: 1}, wantReason: "actor_prefix"},
		{name: "well-formed promotable report", report: passing(), promotable: true},
	}
	allowed := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateS2Soak(tc.report, tc.divergences)
			if got.Promotable != tc.promotable || got.FailClosed == tc.promotable {
				t.Fatalf("verdict = promotable %v fail_closed %v, want promotable %v: %v", got.Promotable, got.FailClosed, tc.promotable, got.FailureReasons)
			}
			if tc.wantReason != "" && !containsReason(got.FailureReasons, tc.wantReason) {
				t.Fatalf("failure_reasons = %v, want reason containing %q", got.FailureReasons, tc.wantReason)
			}
			if tc.promotable {
				allowed++
			}
			if tc.name == "well-formed promotable report" && got.ElapsedDays != 7 {
				t.Fatalf("elapsed_days = %d, want exact threshold of 7", got.ElapsedDays)
			}
			if tc.name == "well-formed promotable report" && got.WouldHaveActed != 3 {
				t.Fatalf("would_have_acted = %d, want 3", got.WouldHaveActed)
			}
		})
	}
	if allowed != 1 {
		t.Fatalf("promotable fixtures = %d, want exactly one", allowed)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

func TestSoakMetricsJSONNames(t *testing.T) {
	b, err := json.Marshal(SoakMetrics{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{S2SoakElapsedDaysMetric, S2SoakDryRunDecisionsMetric, S2SoakWouldHaveActedMetric, S2SoakDivergencesMetric} {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Fatalf("JSON %s missing metric %q", b, name)
		}
	}
}
