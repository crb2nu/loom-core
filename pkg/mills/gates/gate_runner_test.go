package gates

import (
	"context"
	"errors"
	"testing"
	"time"
)

type storageHealthFunc func(context.Context) (HealthSnapshot, error)

func (f storageHealthFunc) EvaluateStorageHealth(ctx context.Context) (HealthSnapshot, error) {
	return f(ctx)
}

type configPreflightFunc func(context.Context) (LocalConfigResult, error)

func (f configPreflightFunc) PreflightLocalConfig(ctx context.Context) (LocalConfigResult, error) {
	return f(ctx)
}

func healthyStorage(now time.Time) StorageHealthEvaluator {
	return storageHealthFunc(func(context.Context) (HealthSnapshot, error) {
		return HealthSnapshot{ObservedAt: now, Components: []HealthComponent{{
			Name: "mills-state-store", State: HealthStateHealthy, Critical: true, CheckedAt: now,
		}}}, nil
	})
}

func TestGateRunner_AllowsOnlyHealthyStorageAndSafeConfig(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	result := GateRunner{
		StorageHealth: healthyStorage(now), Now: func() time.Time { return now },
		LocalConfig: configPreflightFunc(func(context.Context) (LocalConfigResult, error) { return LocalConfigResult{Safe: true}, nil }),
	}.Run(context.Background())
	if !result.Allowed || result.FailClosed || result.Classification != GateClassificationOK {
		t.Fatalf("Run() = %+v, want allowed ok result", result)
	}
}

func TestGateRunner_FailsClosedForUnsafeStorageWithoutRunningConfig(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	configCalled := false
	result := GateRunner{
		Now: func() time.Time { return now },
		StorageHealth: storageHealthFunc(func(context.Context) (HealthSnapshot, error) {
			return HealthSnapshot{ObservedAt: now, Components: []HealthComponent{{
				Name: "mills-state-store", State: HealthStateDown, Critical: true, CheckedAt: now, Error: "write quorum unavailable",
			}}}, nil
		}),
		LocalConfig: configPreflightFunc(func(context.Context) (LocalConfigResult, error) {
			configCalled = true
			return LocalConfigResult{Safe: true}, nil
		}),
	}.Run(context.Background())
	if result.Allowed || !result.FailClosed || result.Classification != GateClassificationStorageHealth || result.PipelineClass != "infra" {
		t.Fatalf("Run() = %+v, want fail-closed storage infra block", result)
	}
	if configCalled {
		t.Fatal("config preflight ran after storage health blocked")
	}
	if got := result.HealthDecision().Reasons[0]; got != "[class=infra]" {
		t.Fatalf("classification marker = %q, want [class=infra]", got)
	}
}

func TestGateRunner_FailsClosedForLocalConfigFailures(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		fn   configPreflightFunc
	}{
		{"unsafe result", func(context.Context) (LocalConfigResult, error) {
			return LocalConfigResult{Reasons: []string{"missing MILLS_STORAGE_DSN"}}, nil
		}},
		{"check error", func(context.Context) (LocalConfigResult, error) {
			return LocalConfigResult{}, errors.New("config file unreadable")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := GateRunner{StorageHealth: healthyStorage(now), LocalConfig: tc.fn, Now: func() time.Time { return now }}.Run(context.Background())
			if result.Allowed || !result.FailClosed || result.Classification != GateClassificationLocalConfig || result.PipelineClass != "config" {
				t.Fatalf("Run() = %+v, want fail-closed config block", result)
			}
			if got := result.HealthDecision().Reasons[0]; got != "[class=config]" {
				t.Fatalf("classification marker = %q, want [class=config]", got)
			}
		})
	}
}

func TestGateRunner_FailsClosedWhenEvaluatorsAreMissing(t *testing.T) {
	result := GateRunner{}.Run(context.Background())
	if result.Allowed || !result.FailClosed || result.Classification != GateClassificationStorageHealth {
		t.Fatalf("Run() = %+v, want fail-closed missing storage evaluator", result)
	}
}
