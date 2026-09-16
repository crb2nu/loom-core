package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crb2nu/loom/internal/loomconcurrency"
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
		{name: "minimum", limit: intPointer(MinConcurrency), want: MinConcurrency},
		{name: "explicit", limit: intPointer(3), want: 3},
		{name: "maximum", limit: intPointer(MaxConcurrency), want: MaxConcurrency},
		{name: "zero", limit: intPointer(0), want: 0, wantErr: true},
		{name: "negative", limit: intPointer(-1), want: -1, wantErr: true},
		{name: "overflow", limit: intPointer(MaxConcurrency + 1), want: MaxConcurrency + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PipelineConcurrencyPolicy{MaxConcurrency: tc.limit}
			if got := p.EffectiveLimit(); got != tc.want {
				t.Fatalf("EffectiveLimit() = %d, want %d", got, tc.want)
			}
			got, err := p.ResolveLimit()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("ResolveLimit() error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "max_concurrency ") {
				t.Fatalf("ResolveLimit() error = %q, want field and rejected value", err)
			}
			if tc.wantErr {
				var validationErr *ConcurrencyPolicyValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("ResolveLimit() error = %T, want *ConcurrencyPolicyValidationError", err)
				}
				if len(validationErr.Fields) != 1 || validationErr.Fields[0] != "max_concurrency" ||
					validationErr.Value != tc.want || validationErr.Min != MinConcurrency || validationErr.Max != MaxConcurrency {
					t.Fatalf("validation error = %#v, want field/value and bounds [%d, %d]", validationErr, MinConcurrency, MaxConcurrency)
				}
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("ResolveLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPipelineConcurrencyPolicyDecodeAndResolve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		decode  func([]byte, any) error
		want    int
		wantErr bool
	}{
		{name: "json default", data: `{}`, decode: json.Unmarshal, want: DefaultPipelineConcurrencyLimit},
		{name: "yaml default", data: `{}`, decode: yaml.Unmarshal, want: DefaultPipelineConcurrencyLimit},
		{name: "json canonical", data: `{"max_concurrent_pipelines":3}`, decode: json.Unmarshal, want: 3},
		{name: "yaml canonical", data: "max_concurrent_pipelines: 3\n", decode: yaml.Unmarshal, want: 3},
		{name: "json compatibility", data: `{"max_concurrency":4}`, decode: json.Unmarshal, want: 4},
		{name: "yaml compatibility", data: "max_concurrency: 4\n", decode: yaml.Unmarshal, want: 4},
		{name: "json zero", data: `{"max_concurrent_pipelines":0}`, decode: json.Unmarshal, wantErr: true},
		{name: "yaml zero", data: "max_concurrent_pipelines: 0\n", decode: yaml.Unmarshal, wantErr: true},
		{name: "json negative", data: `{"max_concurrent_pipelines":-1}`, decode: json.Unmarshal, wantErr: true},
		{name: "yaml negative", data: "max_concurrent_pipelines: -1\n", decode: yaml.Unmarshal, wantErr: true},
		{name: "json overflow", data: `{"max_concurrent_pipelines":1000000}`, decode: json.Unmarshal, wantErr: true},
		{name: "yaml overflow", data: "max_concurrent_pipelines: 1000000\n", decode: yaml.Unmarshal, wantErr: true},
		{name: "json conflict", data: `{"max_concurrent_pipelines":3,"max_concurrency":4}`, decode: json.Unmarshal, wantErr: true},
		{name: "yaml conflict", data: "max_concurrent_pipelines: 3\nmax_concurrency: 4\n", decode: yaml.Unmarshal, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configured PipelineConcurrencyPolicy
			if err := tc.decode([]byte(tc.data), &configured); err != nil {
				t.Fatal(err)
			}
			got, err := configured.ResolveLimit()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("ResolveLimit() error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("ResolveLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPipelineConcurrencyPolicyRejectsMultipleSpellings(t *testing.T) {
	preferred, legacy := 3, 4
	configured := PipelineConcurrencyPolicy{
		MaxConcurrency:         &preferred,
		MaxConcurrentPipelines: &legacy,
	}
	if err := configured.Validate(); err == nil {
		t.Fatal("expected multiple policy spellings to be rejected")
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
	if err := yaml.Unmarshal([]byte("max_concurrency: many\n"), &configured); err == nil {
		t.Fatal("expected wrong-type policy value to fail decoding")
	}
}

func intPointer(value int) *int {
	return &value
}

func TestStampTargetPolicy(t *testing.T) {
	p := StampTargetPolicy{AllowedTargets: map[string][]string{
		" services/loom-core ": {" services/flexdeck "},
	}}
	for _, tc := range []struct {
		name, source, target string
		want                 bool
	}{
		{name: "same project needs no entry", source: "services/loom-core", target: "services/loom-core", want: true},
		{name: "allowlisted cross project", source: "services/loom-core", target: "services/flexdeck", want: true},
		{name: "missing cross project entry", source: "services/loom-core", target: "services/other"},
		{name: "reverse relationship absent", source: "services/flexdeck", target: "services/loom-core"},
		{name: "missing source", target: "services/loom-core"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Allows(tc.source, tc.target); got != tc.want {
				t.Fatalf("Allows(%q, %q) = %v, want %v", tc.source, tc.target, got, tc.want)
			}
		})
	}
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
