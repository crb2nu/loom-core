package mills

import (
	"testing"
	"time"
)

func TestAutoRequeuePolicy_Defaults(t *testing.T) {
	// Zero value ⇒ enabled with the conservative built-in caps.
	var a AutoRequeuePolicy
	var p PipelinePolicy
	if !p.AutoRequeueEnabled() {
		t.Error("nil Enabled must default to ON")
	}
	if got := a.CooldownDuration(); got != 10*time.Minute {
		t.Errorf("default cooldown = %v, want 10m", got)
	}
	if got := a.ItemCap(); got != 2 {
		t.Errorf("default per-item cap = %d, want 2", got)
	}
	if got := a.DayCap(); got != 6 {
		t.Errorf("default per-day cap = %d, want 6", got)
	}
	if got := a.ExternalIncidentMaxDwell(); got != 6*time.Hour {
		t.Errorf("default external-incident dwell = %v, want 6h", got)
	}

	off := false
	p.AutoRequeue.Enabled = &off
	if p.AutoRequeueEnabled() {
		t.Error("explicit enabled:false must disable")
	}

	custom := AutoRequeuePolicy{CooldownMinutes: 30, ExternalIncidentMaxDwellMinutes: 45, PerItemMax: 5, PerDayMax: 12}
	if custom.CooldownDuration() != 30*time.Minute {
		t.Errorf("custom cooldown = %v", custom.CooldownDuration())
	}
	if custom.ItemCap() != 5 || custom.DayCap() != 12 {
		t.Errorf("custom caps = %d/%d, want 5/12", custom.ItemCap(), custom.DayCap())
	}
	if custom.ExternalIncidentMaxDwell() != 45*time.Minute {
		t.Errorf("custom external-incident dwell = %v", custom.ExternalIncidentMaxDwell())
	}
}

func TestAutoRequeuePolicy_Validate(t *testing.T) {
	cases := []struct {
		name    string
		a       AutoRequeuePolicy
		wantErr bool
	}{
		{"zero-ok", AutoRequeuePolicy{}, false},
		{"typical-ok", AutoRequeuePolicy{CooldownMinutes: 10, PerItemMax: 2, PerDayMax: 6}, false},
		{"neg-cooldown", AutoRequeuePolicy{CooldownMinutes: -1}, true},
		{"neg-dwell", AutoRequeuePolicy{ExternalIncidentMaxDwellMinutes: -1}, true},
		{"neg-item", AutoRequeuePolicy{PerItemMax: -1}, true},
		{"neg-day", AutoRequeuePolicy{PerDayMax: -1}, true},
		{"cooldown-over-max", AutoRequeuePolicy{CooldownMinutes: autoRequeueMaxCooldownMinutes + 1}, true},
		{"dwell-over-max", AutoRequeuePolicy{ExternalIncidentMaxDwellMinutes: autoRequeueMaxCooldownMinutes + 1}, true},
		{"item-over-max", AutoRequeuePolicy{PerItemMax: autoRequeueMaxPerItem + 1}, true},
		{"day-over-max", AutoRequeuePolicy{PerDayMax: autoRequeueMaxPerDay + 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAutoRequeue(tc.a)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAutoRequeue(%+v) err=%v wantErr=%v", tc.a, err, tc.wantErr)
			}
		})
	}
}

// TestDefaultPolicy_AutoRequeueEnabled locks in that the built-in Default()
// ships the sweep ON.
func TestDefaultPolicy_AutoRequeueEnabled(t *testing.T) {
	p := Default()
	if err := p.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	if !p.Pipeline.AutoRequeueEnabled() {
		t.Error("Default() must ship auto-requeue ON")
	}
}
