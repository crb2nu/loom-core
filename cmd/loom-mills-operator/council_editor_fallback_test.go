package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
)

// A remote frontier model id (openai/anthropic) is never deployable on the
// local flexinfer tier, so neither the per-run fallback editor nor the
// no-API-key degrade path may inherit it. COUNCIL-2026-08-03-060011 and
// -120011 hard-failed on exactly that: a DNS blip broke the Anthropic call
// and the fallback dialed flexinfer with claude-fable-5 → guaranteed 404.
func TestBuildEditorForAgentRemoteBackendUsesServableFallbackModel(t *testing.T) {
	flexClient, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.invalid"})
	if err != nil {
		t.Fatalf("NewFlexInferClient: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent := mills.CouncilAgent{Name: "editor", Model: "claude-fable-5", Backend: "anthropic"}

	t.Run("anthropic primary gets weaver-resolved flexinfer fallback", func(t *testing.T) {
		t.Setenv("LOOM_ANTHROPIC_API_KEY", "test-key")
		ed := buildEditorForAgent(agent, "", flexClient, t.TempDir(), nil, logger)
		fb, ok := ed.(*clients.FallbackCouncilEditor)
		if !ok {
			t.Fatalf("want *clients.FallbackCouncilEditor, got %T", ed)
		}
		flex, ok := fb.Fallback.(*clients.FlexInferCouncilEditor)
		if !ok {
			t.Fatalf("want flexinfer fallback editor, got %T", fb.Fallback)
		}
		if flex.Model == agent.Model {
			t.Fatalf("fallback editor inherited un-servable remote model id %q", flex.Model)
		}
		if flex.Model != "" {
			t.Fatalf("want empty model (weaver-chain resolution), got %q", flex.Model)
		}
	})

	t.Run("explicit policy pin wins over weaver resolution", func(t *testing.T) {
		t.Setenv("LOOM_ANTHROPIC_API_KEY", "test-key")
		ed := buildEditorForAgent(agent, "qwen35-9b-ablit-rp", flexClient, t.TempDir(), nil, logger)
		fb, ok := ed.(*clients.FallbackCouncilEditor)
		if !ok {
			t.Fatalf("want *clients.FallbackCouncilEditor, got %T", ed)
		}
		flex, ok := fb.Fallback.(*clients.FlexInferCouncilEditor)
		if !ok {
			t.Fatalf("want flexinfer fallback editor, got %T", fb.Fallback)
		}
		if flex.Model != "qwen35-9b-ablit-rp" {
			t.Fatalf("want pinned fallback model, got %q", flex.Model)
		}
	})

	t.Run("missing key degrades to servable flexinfer primary", func(t *testing.T) {
		t.Setenv("LOOM_ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		ed := buildEditorForAgent(agent, "", flexClient, t.TempDir(), nil, logger)
		flex, ok := ed.(*clients.FlexInferCouncilEditor)
		if !ok {
			t.Fatalf("want *clients.FlexInferCouncilEditor, got %T", ed)
		}
		if flex.Model == agent.Model {
			t.Fatalf("degrade editor inherited un-servable remote model id %q", flex.Model)
		}
	})

	t.Run("flexinfer backend keeps its configured model", func(t *testing.T) {
		local := mills.CouncilAgent{Name: "editor", Model: "gemma4-26b-a4b-gptq", Backend: "flexinfer"}
		ed := buildEditorForAgent(local, "", flexClient, t.TempDir(), nil, logger)
		flex, ok := ed.(*clients.FlexInferCouncilEditor)
		if !ok {
			t.Fatalf("want *clients.FlexInferCouncilEditor, got %T", ed)
		}
		if flex.Model != local.Model {
			t.Fatalf("want configured local model %q, got %q", local.Model, flex.Model)
		}
	})
}
