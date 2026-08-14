package registry

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// FuzzTemplateClamp is the S7 numeric-clamp fuzz test (.loom/134 §S7): for any
// float64 input the clamped value is finite, inside [Min,Max], and clamping is
// idempotent — so re-clamping frozen params at replay is byte-stable.
func FuzzTemplateClamp(f *testing.F) {
	for _, seed := range []float64{0, 1, -1, 0.049, 0.05, 5.0, 5.0000001, 1e308, -1e308, math.NaN(), math.Inf(1), math.Inf(-1)} {
		f.Add(seed)
	}
	tmpl := implementGateV1()
	spec := tmpl.Params["budget_usd"]
	f.Fuzz(func(t *testing.T, v float64) {
		clamped := tmpl.ClampParams(map[string]float64{"budget_usd": v})
		got := clamped["budget_usd"]
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("clamp produced non-finite %v from %v", got, v)
		}
		if got < spec.Min || got > spec.Max {
			t.Fatalf("clamp(%v) = %v outside [%v,%v]", v, got, spec.Min, spec.Max)
		}
		again := tmpl.ClampParams(clamped)["budget_usd"]
		if again != got {
			t.Fatalf("clamp not idempotent: clamp(clamp(%v)) = %v, want %v", v, again, got)
		}
	})
}

func TestTemplateClampTable(t *testing.T) {
	tmpl := implementGateV1()
	for _, tt := range []struct {
		name string
		in   map[string]float64
		want float64
	}{
		{"missing takes default", nil, 1.0},
		{"NaN takes default", map[string]float64{"budget_usd": math.NaN()}, 1.0},
		{"+Inf takes default", map[string]float64{"budget_usd": math.Inf(1)}, 1.0},
		{"below min clamps up", map[string]float64{"budget_usd": 0.0001}, 0.05},
		{"above max clamps down", map[string]float64{"budget_usd": 400}, 5.0},
		{"in range passes through", map[string]float64{"budget_usd": 2.5}, 2.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tmpl.ClampParams(tt.in)["budget_usd"]; got != tt.want {
				t.Fatalf("clamp = %v, want %v", got, tt.want)
			}
		})
	}
	clamped := tmpl.ClampParams(map[string]float64{"budget_usd": 1, "surprise": 9})
	if _, ok := clamped["surprise"]; ok {
		t.Fatal("unknown numeric param survived clamping")
	}
}

func TestUnknownTemplateFailsClosed(t *testing.T) {
	r := NewDefault()
	if _, err := r.Resolve("no-such-template", "v1"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown name error = %v, want ErrUnknownTemplate", err)
	}
	if _, err := r.Resolve("implement-gate", "v999"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown version error = %v, want ErrUnknownTemplate", err)
	}
	if err := r.Validate("no-such-template", "v1", nil, nil); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("validate error = %v, want ErrUnknownTemplate", err)
	}
}

func TestEnumOutsideClosedVocabularyRejected(t *testing.T) {
	r := NewDefault()
	_, err := r.FreezeSelection("implement-gate", "v1", nil, map[string]string{"model": "gemini'); os.system('rm"})
	if err == nil || !strings.Contains(err.Error(), "rejects value") {
		t.Fatalf("hostile enum accepted: %v", err)
	}
	// Unknown enum keys are dropped; missing take defaults.
	paramsJSON, err := r.FreezeSelection("implement-gate", "v1", nil, map[string]string{"surprise": "x"})
	if err != nil {
		t.Fatalf("freeze with unknown enum key: %v", err)
	}
	if strings.Contains(paramsJSON, "surprise") {
		t.Fatalf("unknown enum key survived freeze: %s", paramsJSON)
	}
}

func TestContentHashDriftFailsClosed(t *testing.T) {
	r := NewDefault()
	paramsJSON, err := r.FreezeSelection("implement-gate", "v1", nil, nil)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	run := &store.WorkflowRun{
		ID: "wf-reg-drift", Engine: store.WorkflowEngineImperative,
		Template: "implement-gate", TemplateVersion: "v1",
		WorkflowParams: paramsJSON,
	}
	if _, err := r.ScriptFromRun(run); err != nil {
		t.Fatalf("un-tampered selection rejected: %v", err)
	}
	run.WorkflowParams = strings.Replace(paramsJSON, `"content_hash":"`, `"content_hash":"00`, 1)
	if _, err := r.ScriptFromRun(run); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Fatalf("drifted content hash accepted: %v", err)
	}
	// A frozen selection from a template whose registry entry no longer
	// exists (renamed/retired) is the same drift class.
	run.Template = "retired-template"
	if _, err := r.ScriptFromRun(run); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("retired template error = %v, want ErrUnknownTemplate", err)
	}
}

func TestScriptDeterminismAndInjectionSafety(t *testing.T) {
	r := NewDefault()
	tmpl, err := r.Resolve("implement-gate", "v1")
	if err != nil {
		t.Fatal(err)
	}
	s1, _, _, err := tmpl.Render(map[string]float64{"budget_usd": 2.5}, map[string]string{"model": AgentTypeCodex})
	if err != nil {
		t.Fatal(err)
	}
	s2, _, _, err := tmpl.Render(map[string]float64{"budget_usd": 2.5}, map[string]string{"model": AgentTypeCodex})
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatalf("render not byte-stable:\n%q\n%q", s1, s2)
	}
	if !strings.Contains(s1, "model='codex'") || !strings.Contains(s1, "budget_usd=2.5") {
		t.Fatalf("render missing validated params: %q", s1)
	}
}
