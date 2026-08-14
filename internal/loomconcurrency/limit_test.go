package loomconcurrency

import (
	"sync"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		limit   int
		wantErr bool
	}{
		{limit: MinLimit},
		{limit: DefaultLimit},
		{limit: MaxLimit},
		{limit: MinLimit - 1, wantErr: true},
		{limit: MaxLimit + 1, wantErr: true},
	} {
		if got := Validate(tc.limit); (got != nil) != tc.wantErr {
			t.Errorf("Validate(%d) = %v, want error %v", tc.limit, got, tc.wantErr)
		}
	}
}

func TestDefault_UnsetReturnsDefault(t *testing.T) {
	t.Setenv(EnvVar, "")
	if got := Default(); got != DefaultLimit {
		t.Fatalf("Default() with %s unset = %d, want %d", EnvVar, got, DefaultLimit)
	}
}

func TestDefault_HonorsEnvOverride(t *testing.T) {
	t.Setenv(EnvVar, "16")
	if got := Default(); got != 16 {
		t.Fatalf("Default() = %d, want 16", got)
	}
}

func TestDefault_ClampsLow(t *testing.T) {
	t.Setenv(EnvVar, "0")
	if got := Default(); got != MinLimit {
		t.Fatalf("Default() with %s=0 = %d, want %d", EnvVar, got, MinLimit)
	}
	t.Setenv(EnvVar, "-5")
	if got := Default(); got != MinLimit {
		t.Fatalf("Default() with %s=-5 = %d, want %d", EnvVar, got, MinLimit)
	}
}

func TestDefault_ClampsHigh(t *testing.T) {
	t.Setenv(EnvVar, "1024")
	if got := Default(); got != MaxLimit {
		t.Fatalf("Default() with %s=1024 = %d, want %d", EnvVar, got, MaxLimit)
	}
}

func TestDefault_UnparseableFallsBack(t *testing.T) {
	t.Setenv(EnvVar, "not-a-number")
	if got := Default(); got != DefaultLimit {
		t.Fatalf("Default() with garbage = %d, want fallback %d", got, DefaultLimit)
	}
}

func TestDefault_LegacySequentialMode(t *testing.T) {
	t.Setenv(EnvVar, "1")
	if got := Default(); got != 1 {
		t.Fatalf("Default() with %s=1 = %d, want 1 (sequential)", EnvVar, got)
	}
}

// fakeLimiter records the values passed to SetConcurrencyLimit so the
// Apply* test path doesn't need to import mcp-go.
type fakeLimiter struct {
	mu     sync.Mutex
	values []int
}

func (f *fakeLimiter) SetConcurrencyLimit(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = append(f.values, n)
}

func (f *fakeLimiter) lastValue() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.values) == 0 {
		return 0, false
	}
	return f.values[len(f.values)-1], true
}

func TestApply_UsesDefault(t *testing.T) {
	t.Setenv(EnvVar, "12")
	f := &fakeLimiter{}
	Apply(f)
	v, ok := f.lastValue()
	if !ok {
		t.Fatal("Apply did not invoke SetConcurrencyLimit")
	}
	if v != 12 {
		t.Fatalf("Apply set %d, want 12 from env", v)
	}
}

func TestApply_NilIsNoOp(t *testing.T) {
	// Should not panic, should not error.
	Apply(nil)
}

func TestApplyValue_Clamps(t *testing.T) {
	f := &fakeLimiter{}
	ApplyValue(f, 9999)
	v, _ := f.lastValue()
	if v != MaxLimit {
		t.Fatalf("ApplyValue(9999) = %d, want clamp to %d", v, MaxLimit)
	}
	ApplyValue(f, 0)
	v, _ = f.lastValue()
	if v != MinLimit {
		t.Fatalf("ApplyValue(0) = %d, want clamp to %d", v, MinLimit)
	}
}

func TestApplyValue_NilIsNoOp(t *testing.T) {
	ApplyValue(nil, 4) // must not panic
}

func TestApplyValidatedRejectsWithoutMutation(t *testing.T) {
	f := &fakeLimiter{}
	if err := ApplyValidated(f, MinLimit-1); err == nil {
		t.Fatal("expected invalid limit to be rejected")
	}
	if _, ok := f.lastValue(); ok {
		t.Fatal("invalid limit mutated limiter")
	}
	if err := ApplyValidated(f, 3); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.lastValue(); !ok || got != 3 {
		t.Fatalf("valid limit = %d, applied %v; want 3, true", got, ok)
	}
}
