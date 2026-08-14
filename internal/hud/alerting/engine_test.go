package alerting

import (
	"log/slog"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func TestEvaluate_PipelineFailed(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	pipelines := []bridge.PipelineInfo{
		{ID: 100, Project: "services/loom-core", Ref: "main", Status: "failed", WebURL: "https://example.com/100"},
	}

	engine.Evaluate(pipelines)

	alerts := engine.ListAlerts(10)
	if len(alerts) == 0 {
		t.Fatal("expected at least one alert for failed pipeline")
	}

	found := false
	for _, a := range alerts {
		if a.RuleID == "pipeline-failed" {
			found = true
			if a.Severity != "warning" {
				t.Errorf("expected severity=warning, got %s", a.Severity)
			}
			if a.Pipeline.ID != 100 {
				t.Errorf("expected pipeline ID=100, got %d", a.Pipeline.ID)
			}
			if a.Pipeline.Project != "services/loom-core" {
				t.Errorf("expected project=services/loom-core, got %s", a.Pipeline.Project)
			}
		}
	}
	if !found {
		t.Error("expected to find a pipeline-failed alert")
	}
}

func TestEvaluate_CooldownEnforced(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	pipelines := []bridge.PipelineInfo{
		{ID: 100, Project: "services/loom-core", Ref: "main", Status: "failed"},
	}

	// First evaluation fires.
	engine.Evaluate(pipelines)
	alerts1 := engine.ListAlerts(10)
	count1 := len(alerts1)
	if count1 == 0 {
		t.Fatal("expected alerts after first evaluation")
	}

	// Second evaluation within cooldown should not fire additional alerts.
	engine.Evaluate(pipelines)
	alerts2 := engine.ListAlerts(10)
	if len(alerts2) != count1 {
		t.Errorf("expected %d alerts (cooldown), got %d", count1, len(alerts2))
	}
}

func TestEvaluate_ConsecutiveFailures(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	// Set cooldown to 0 for testing.
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
	}
	engine.mu.Unlock()

	ref := "feat/test"
	project := "services/app"

	// Simulate 3 consecutive failures.
	for i := 0; i < 3; i++ {
		pipelines := []bridge.PipelineInfo{
			{ID: 200 + i, Project: project, Ref: ref, Status: "failed"},
		}
		engine.Evaluate(pipelines)
	}

	alerts := engine.ListAlerts(50)
	foundConsecutive := false
	for _, a := range alerts {
		if a.RuleID == "consecutive-failures" {
			foundConsecutive = true
			if a.Severity != "critical" {
				t.Errorf("expected severity=critical, got %s", a.Severity)
			}
		}
	}
	if !foundConsecutive {
		t.Error("expected a consecutive-failures alert after 3 failures")
	}
}

func TestEvaluate_ConsecutiveFailures_ResetOnSuccess(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	// Set cooldown to 0 for testing.
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
	}
	engine.mu.Unlock()

	ref := "feat/test"
	project := "services/app"

	// 2 failures.
	for i := 0; i < 2; i++ {
		engine.Evaluate([]bridge.PipelineInfo{
			{ID: 300 + i, Project: project, Ref: ref, Status: "failed"},
		})
	}

	// Success resets counter.
	engine.Evaluate([]bridge.PipelineInfo{
		{ID: 310, Project: project, Ref: ref, Status: "success"},
	})

	// 2 more failures (should not trigger threshold of 3).
	for i := 0; i < 2; i++ {
		engine.Evaluate([]bridge.PipelineInfo{
			{ID: 320 + i, Project: project, Ref: ref, Status: "failed"},
		})
	}

	alerts := engine.ListAlerts(50)
	for _, a := range alerts {
		if a.RuleID == "consecutive-failures" {
			t.Error("should not have consecutive-failures alert after success reset")
		}
	}
}

func TestEvaluate_PipelineStuck(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	// Set cooldown to 0 and stuck duration to 1ms for fast testing.
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
		if engine.rules[i].ID == "pipeline-stuck" {
			engine.rules[i].Condition.Duration = 1 * time.Millisecond
		}
	}
	engine.mu.Unlock()

	pipelines := []bridge.PipelineInfo{
		{ID: 400, Project: "services/app", Ref: "main", Status: "running"},
	}

	// First evaluation: track start time.
	engine.Evaluate(pipelines)

	// Wait just past the threshold.
	time.Sleep(5 * time.Millisecond)

	// Second evaluation: should detect stuck.
	engine.Evaluate(pipelines)

	alerts := engine.ListAlerts(50)
	foundStuck := false
	for _, a := range alerts {
		if a.RuleID == "pipeline-stuck" {
			foundStuck = true
		}
	}
	if !foundStuck {
		t.Error("expected a pipeline-stuck alert")
	}
}

// fireStuckAlert drives an engine to a fired pipeline-stuck alert for pipeline
// 400 and returns the engine. Cooldowns are zeroed and the stuck threshold
// dropped to 1ms so the rule fires without waiting out the real 60m default.
func fireStuckAlert(t *testing.T) *AlertEngine {
	t.Helper()

	engine := NewAlertEngine(nil, slog.Default())
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
		if engine.rules[i].ID == stuckRuleID {
			engine.rules[i].Condition.Duration = 1 * time.Millisecond
		}
	}
	engine.mu.Unlock()

	running := []bridge.PipelineInfo{
		{ID: 400, Project: "services/loom-core", Ref: "main", Status: "running"},
	}
	engine.Evaluate(running) // records the start time
	time.Sleep(5 * time.Millisecond)
	engine.Evaluate(running) // fires

	if countStuckAlerts(engine.ListAlerts(50)) != 1 {
		t.Fatalf("setup: expected 1 active stuck alert, got %d", countStuckAlerts(engine.ListAlerts(50)))
	}
	return engine
}

func countStuckAlerts(alerts []Alert) int {
	n := 0
	for _, a := range alerts {
		if a.RuleID == stuckRuleID {
			n++
		}
	}
	return n
}

func TestEvaluate_PipelineStuck_ResolvesOnTerminalStatus(t *testing.T) {
	for _, terminal := range []string{"success", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			engine := fireStuckAlert(t)

			// The pipeline settles: same ID, terminal status.
			engine.Evaluate([]bridge.PipelineInfo{
				{ID: 400, Project: "services/loom-core", Ref: "main", Status: terminal},
			})

			if got := countStuckAlerts(engine.ListAlerts(50)); got != 0 {
				t.Errorf("expected 0 active stuck alerts after %s, got %d", terminal, got)
			}

			// The alert is retained in the ring, resolved, with a refreshed
			// status snapshot instead of the stale "running".
			engine.mu.RLock()
			defer engine.mu.RUnlock()
			found := false
			for _, a := range engine.alerts {
				if a.RuleID != stuckRuleID {
					continue
				}
				found = true
				if a.ResolvedAt == nil {
					t.Error("expected ResolvedAt to be set")
				}
				if a.Pipeline.Status != terminal {
					t.Errorf("expected snapshot status=%s, got %s", terminal, a.Pipeline.Status)
				}
			}
			if !found {
				t.Error("expected the resolved alert to be retained in history")
			}
		})
	}
}

func TestEvaluate_PipelineStuck_ResolvesWhenPipelineDisappears(t *testing.T) {
	engine := fireStuckAlert(t)

	// The pipeline finished and dropped off the active list entirely.
	engine.Evaluate(nil)

	if got := countStuckAlerts(engine.ListAlerts(50)); got != 0 {
		t.Errorf("expected 0 active stuck alerts after pipeline vanished, got %d", got)
	}

	engine.mu.RLock()
	defer engine.mu.RUnlock()
	for _, a := range engine.alerts {
		if a.RuleID == stuckRuleID && a.ResolvedAt == nil {
			t.Error("expected the stuck alert to be resolved")
		}
	}
}

func TestEvaluate_PipelineStuck_StaysActiveWhileRunning(t *testing.T) {
	engine := fireStuckAlert(t)

	// Still running: the alert must survive re-evaluation — as exactly one
	// card (fire-side dedup), not a duplicate per cycle.
	engine.Evaluate([]bridge.PipelineInfo{
		{ID: 400, Project: "services/loom-core", Ref: "main", Status: "running"},
	})

	if got := countStuckAlerts(engine.ListAlerts(50)); got != 1 {
		t.Errorf("expected exactly 1 active stuck alert while the pipeline runs, got %d", got)
	}
}

func TestDefaultRules_StuckThreshold(t *testing.T) {
	for _, r := range defaultRules() {
		if r.ID != stuckRuleID {
			continue
		}
		if r.Condition.Duration != 60*time.Minute {
			t.Errorf("expected stuck threshold of 60m, got %s", r.Condition.Duration)
		}
		return
	}
	t.Fatalf("no %s rule in defaults", stuckRuleID)
}

func TestAckAlert(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	engine.Evaluate([]bridge.PipelineInfo{
		{ID: 500, Project: "services/app", Ref: "main", Status: "failed"},
	})

	alerts := engine.ListAlerts(10)
	if len(alerts) == 0 {
		t.Fatal("expected alerts")
	}

	alertID := alerts[0].ID
	if err := engine.AckAlert(alertID, "test-user"); err != nil {
		t.Fatalf("ack failed: %v", err)
	}

	// Verify ack.
	alerts = engine.ListAlerts(10)
	for _, a := range alerts {
		if a.ID == alertID {
			if a.AckedAt == nil {
				t.Error("expected AckedAt to be set")
			}
			if a.AckedBy != "test-user" {
				t.Errorf("expected AckedBy=test-user, got %s", a.AckedBy)
			}
		}
	}

	// Ack non-existent alert.
	if err := engine.AckAlert("nonexistent", "user"); err == nil {
		t.Error("expected error for nonexistent alert")
	}
}

func TestAddRemoveRule(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	initialCount := len(engine.ListRules())

	engine.AddRule(AlertRule{
		ID:       "custom-rule",
		Name:     "Custom Rule",
		Enabled:  true,
		Severity: "info",
		Condition: AlertCondition{
			Type: "pipeline_failed",
		},
		Cooldown: time.Minute,
	})

	if got := len(engine.ListRules()); got != initialCount+1 {
		t.Errorf("expected %d rules after add, got %d", initialCount+1, got)
	}

	// Adding same ID replaces.
	engine.AddRule(AlertRule{
		ID:       "custom-rule",
		Name:     "Custom Rule v2",
		Enabled:  false,
		Severity: "critical",
		Condition: AlertCondition{
			Type: "pipeline_failed",
		},
	})

	if got := len(engine.ListRules()); got != initialCount+1 {
		t.Errorf("expected %d rules after replace, got %d", initialCount+1, got)
	}

	engine.RemoveRule("custom-rule")
	if got := len(engine.ListRules()); got != initialCount {
		t.Errorf("expected %d rules after remove, got %d", initialCount, got)
	}
}

func TestListAlerts_Limit(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	// Set cooldown to 0 for testing.
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
	}
	engine.mu.Unlock()

	// Generate multiple alerts.
	for i := 0; i < 10; i++ {
		engine.Evaluate([]bridge.PipelineInfo{
			{ID: 600 + i, Project: "services/app", Ref: "main", Status: "failed"},
		})
	}

	all := engine.ListAlerts(0)
	if len(all) == 0 {
		t.Fatal("expected alerts")
	}

	limited := engine.ListAlerts(3)
	if len(limited) > 3 {
		t.Errorf("expected at most 3 alerts, got %d", len(limited))
	}

	// Verify newest-first ordering.
	if len(limited) >= 2 {
		if limited[0].FiredAt.Before(limited[1].FiredAt) {
			t.Error("expected newest-first ordering")
		}
	}
}

func TestMatchesProject(t *testing.T) {
	tests := []struct {
		filter  []string
		project string
		want    bool
	}{
		{nil, "any-project", true},
		{[]string{}, "any-project", true},
		{[]string{"services/app"}, "services/app", true},
		{[]string{"services/app"}, "services/other", false},
		{[]string{"a", "b", "c"}, "b", true},
	}

	for _, tt := range tests {
		got := matchesProject(tt.filter, tt.project)
		if got != tt.want {
			t.Errorf("matchesProject(%v, %q) = %v, want %v", tt.filter, tt.project, got, tt.want)
		}
	}
}

func TestNormalizePipelineStatus(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"running", "running"},
		{"success", "success"},
		{"passed", "success"},
		{"pending", "pending"},
		{"created", "pending"},
		{"failed", "failed"},
		{"canceled", "failed"},
		{"cancelled", "failed"},
		{"unknown", "failed"},
	}

	for _, tt := range tests {
		got := normalizePipelineStatus(tt.input)
		if got != tt.want {
			t.Errorf("normalizePipelineStatus(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDisabledRule_NotFired(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())

	// Disable all rules.
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Enabled = false
	}
	engine.mu.Unlock()

	engine.Evaluate([]bridge.PipelineInfo{
		{ID: 700, Project: "services/app", Ref: "main", Status: "failed"},
	})

	alerts := engine.ListAlerts(10)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts with all rules disabled, got %d", len(alerts))
	}
}

// TestEvaluate_PipelineStuck_NoDuplicateWhileStuck pins the fire-side dedup:
// a pipeline that stays stuck across evaluation cycles keeps ONE open card
// whose message tracks the current age, instead of stacking a new card (and
// re-notifying) every cooldown window.
func TestEvaluate_PipelineStuck_NoDuplicateWhileStuck(t *testing.T) {
	engine := fireStuckAlert(t)

	running := []bridge.PipelineInfo{
		{ID: 400, Project: "services/loom-core", Ref: "main", Status: "running"},
	}
	// Several further cycles past the (zeroed) cooldown, still stuck.
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Millisecond)
		engine.Evaluate(running)
	}

	if got := countStuckAlerts(engine.ListAlerts(50)); got != 1 {
		t.Fatalf("expected exactly 1 open stuck card while stuck, got %d", got)
	}
}

// TestEvaluate_PipelineStuck_RefiresAfterResolution: dedup keys on OPEN cards
// only — once the pipeline settles and the card resolves, the same pipeline
// getting stuck again (e.g. a retry that wedges) fires a fresh card.
func TestEvaluate_PipelineStuck_RefiresAfterResolution(t *testing.T) {
	engine := fireStuckAlert(t)

	// Settle: the open card resolves.
	engine.Evaluate(nil)
	if got := countStuckAlerts(engine.ListAlerts(50)); got != 0 {
		t.Fatalf("setup: expected 0 open stuck cards after settle, got %d", got)
	}

	// The same pipeline wedges again: re-track, exceed threshold, re-fire.
	running := []bridge.PipelineInfo{
		{ID: 400, Project: "services/loom-core", Ref: "main", Status: "running"},
	}
	engine.Evaluate(running)
	time.Sleep(5 * time.Millisecond)
	engine.Evaluate(running)

	if got := countStuckAlerts(engine.ListAlerts(50)); got != 1 {
		t.Errorf("expected a fresh stuck card after resolution, got %d", got)
	}
}

// TestEvaluate_PipelineFailed_NoDuplicateWhileFailed pins that a failed
// pipeline lingering in the active window does not re-card every cycle.
func TestEvaluate_PipelineFailed_NoDuplicateWhileFailed(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
	}
	engine.mu.Unlock()

	failed := []bridge.PipelineInfo{
		{ID: 500, Project: "services/app", Ref: "main", Status: "failed"},
	}
	for i := 0; i < 3; i++ {
		engine.Evaluate(failed)
	}

	count := 0
	for _, a := range engine.ListAlerts(50) {
		if a.RuleID == "pipeline-failed" && a.Pipeline.ID == 500 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 open pipeline-failed card, got %d", count)
	}
}

// TestEvaluate_ConsecutiveFailures_NoDuplicatePerStreak pins that one open
// streak card absorbs further failures on the same project+ref — a growing
// streak must not mint a card per newly failed pipeline.
func TestEvaluate_ConsecutiveFailures_NoDuplicatePerStreak(t *testing.T) {
	engine := NewAlertEngine(nil, slog.Default())
	engine.mu.Lock()
	for i := range engine.rules {
		engine.rules[i].Cooldown = 0
	}
	engine.mu.Unlock()

	// 5 consecutive failures on one ref: crosses the threshold at 3, keeps
	// failing with new pipeline IDs after the card is open.
	for i := 0; i < 5; i++ {
		engine.Evaluate([]bridge.PipelineInfo{
			{ID: 600 + i, Project: "services/app", Ref: "feat/streak", Status: "failed"},
		})
	}

	count := 0
	for _, a := range engine.ListAlerts(50) {
		if a.RuleID == "consecutive-failures" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 open consecutive-failures card for the streak, got %d", count)
	}
}
