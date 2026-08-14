package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"gopkg.in/yaml.v3"
)

func TestPipelineConcurrencyPolicy(t *testing.T) {
	if DefaultPipelineConcurrencyLimit != loomconcurrency.DefaultLimit {
		t.Fatalf("policy default = %d, runtime default = %d", DefaultPipelineConcurrencyLimit, loomconcurrency.DefaultLimit)
	}
	for _, tc := range []struct {
		name    string
		limit   *int
		want    int
		wantErr bool
	}{
		{name: "absent", want: loomconcurrency.DefaultLimit},
		{name: "explicit", limit: intPointer(3), want: 3},
		{name: "zero", limit: intPointer(0), want: 0, wantErr: true},
		{name: "negative", limit: intPointer(-1), want: -1, wantErr: true},
		{name: "overflow", limit: intPointer(loomconcurrency.MaxLimit + 1), want: loomconcurrency.MaxLimit + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PipelineConcurrencyPolicy{MaxConcurrentPipelines: tc.limit}
			if got := p.EffectiveLimit(); got != tc.want {
				t.Fatalf("EffectiveLimit() = %d, want %d", got, tc.want)
			}
			err := p.Validate()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Validate() error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "max_concurrent_pipelines ") {
				t.Fatalf("Validate() error = %q, want field and rejected value", err)
			}
		})
	}
}

func TestProductionConfigMapPinsPipelineConcurrencyDefault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "k8s", "configmap-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var configMap struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &configMap); err != nil {
		t.Fatalf("parse ConfigMap: %v", err)
	}
	var configured PipelineConcurrencyPolicy
	if err := yaml.Unmarshal([]byte(configMap.Data["policy.yaml"]), &configured); err != nil {
		t.Fatalf("parse policy.yaml: %v", err)
	}
	if err := configured.Validate(); err != nil {
		t.Fatalf("validate policy.yaml: %v", err)
	}
	if got := configured.EffectiveLimit(); got != DefaultPipelineConcurrencyLimit {
		t.Fatalf("ConfigMap limit = %d, code default = %d", got, DefaultPipelineConcurrencyLimit)
	}
}

func TestPipelineConcurrencyPolicyRejectsWrongType(t *testing.T) {
	var configured PipelineConcurrencyPolicy
	if err := yaml.Unmarshal([]byte("max_concurrent_pipelines: many\n"), &configured); err == nil {
		t.Fatal("expected wrong-type policy value to fail decoding")
	}
}

func intPointer(value int) *int {
	return &value
}

func TestExternalIncidentThreshold(t *testing.T) {
	for _, tc := range []struct {
		configured int
		want       int
	}{
		{configured: 0, want: DefaultExternalIncidentThreshold},
		{configured: -1, want: DefaultExternalIncidentThreshold},
		{configured: 7, want: 7},
	} {
		if got := (ExternalIncidentPolicy{Threshold: tc.configured}).ExternalIncidentThreshold(); got != tc.want {
			t.Fatalf("threshold(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

func TestCouncilIntentPolicyDefault(t *testing.T) {
	if !DefaultCouncilRequireRoadmapIntents {
		t.Fatal("the roadmap-intent guardrail must default to fail-closed")
	}
	// Unset (nil) resolves to the conservative default.
	if got := (CouncilIntentPolicy{}).RequireRoadmapIntentsEnabled(); got != DefaultCouncilRequireRoadmapIntents {
		t.Fatalf("unset = %v, want %v", got, DefaultCouncilRequireRoadmapIntents)
	}
	for _, want := range []bool{true, false} {
		v := want
		if got := (CouncilIntentPolicy{RequireRoadmapIntents: &v}).RequireRoadmapIntentsEnabled(); got != want {
			t.Fatalf("explicit %v = %v", want, got)
		}
	}
}
