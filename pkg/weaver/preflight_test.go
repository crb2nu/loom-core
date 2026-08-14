package weaver

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/flexinfer"
)

type fakeModelLister struct {
	models []flexinfer.ModelInfo
	err    error
}

func (f fakeModelLister) Models(_ context.Context) ([]flexinfer.ModelInfo, error) {
	return f.models, f.err
}

func TestRunPreflight_AllReady(t *testing.T) {
	cfg := Config{RouterModel: "router-m", SubagentModel: "sub-m"}
	lister := fakeModelLister{models: []flexinfer.ModelInfo{
		{ID: "router-m"}, {ID: "sub-m"}, {ID: "unused-m"},
	}}

	pre := RunPreflight(context.Background(), lister, cfg, nil)
	if pre.Degraded {
		t.Errorf("degraded: got true, missing=%v", pre.MissingModels)
	}
	if pre.CatalogSize != 3 {
		t.Errorf("catalog_size: got %d want 3", pre.CatalogSize)
	}
	if len(pre.ReadyModels) != 2 {
		t.Errorf("ready_models: got %v", pre.ReadyModels)
	}
}

func TestRunPreflight_DegradedMissingModel(t *testing.T) {
	cfg := Config{RouterModel: "router-m", SubagentModel: "gone-m"}
	lister := fakeModelLister{models: []flexinfer.ModelInfo{{ID: "router-m"}}}

	pre := RunPreflight(context.Background(), lister, cfg, nil)
	if !pre.Degraded {
		t.Error("expected degraded")
	}
	if len(pre.MissingModels) != 1 || pre.MissingModels[0] != "gone-m" {
		t.Errorf("missing_models: got %v", pre.MissingModels)
	}
}

func TestRunPreflight_CatalogError(t *testing.T) {
	cfg := Config{RouterModel: "router-m"}
	lister := fakeModelLister{err: errors.New("connection refused")}

	pre := RunPreflight(context.Background(), lister, cfg, nil)
	if !pre.Degraded {
		t.Error("expected degraded on catalog error")
	}
	if pre.CatalogError == "" {
		t.Error("expected catalog_error to be set")
	}
	if len(pre.MissingModels) != 1 {
		t.Errorf("missing_models should carry the full configured set: %v", pre.MissingModels)
	}
}

func TestRunPreflight_DomainModelOverrides(t *testing.T) {
	cfg := Config{RouterModel: "router-m", SubagentModel: "sub-m"}
	reg := NewDomainRegistry()
	reg.Register(SubAgent{Name: "flex-domain", Model: "domain-m", Description: "d"})
	reg.Register(SubAgent{Name: "spawn-domain", Model: "claude-pod", Backend: "claude-code", Description: "d"})
	lister := fakeModelLister{models: []flexinfer.ModelInfo{
		{ID: "router-m"}, {ID: "sub-m"},
	}}

	pre := RunPreflight(context.Background(), lister, cfg, reg)
	// The flexinfer domain override is checked (and missing); the
	// spawn-backed domain's model is not routed through FlexInfer and
	// must be ignored.
	if !pre.Degraded {
		t.Error("expected degraded for missing domain model")
	}
	if len(pre.MissingModels) != 1 || pre.MissingModels[0] != "domain-m" {
		t.Errorf("missing_models: got %v want [domain-m]", pre.MissingModels)
	}
}
