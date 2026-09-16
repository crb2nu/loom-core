package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
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

// The council editor chain is primary → cross-vendor remote → local
// flexinfer. 2026-09-07 three consecutive council runs died at the editor on
// an Anthropic billing hold after ~$1.7 of reviewer spend each while the
// OpenAI key beside it sat idle; the hop is the fix, and these cases pin its
// shape, its default, its opt-out, and the two places it must NOT appear.
func TestBuildCouncilEditorCrossVendorChain(t *testing.T) {
	flexClient, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.invalid"})
	if err != nil {
		t.Fatalf("NewFlexInferClient: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	anthropicPolicy := func() *mills.Policy {
		p := &mills.Policy{}
		p.Council.Ensemble.Editor = mills.CouncilAgent{Name: "editor", Model: "claude-fable-5-1", Backend: "anthropic"}
		return p
	}
	setKeys := func(t *testing.T, anthropic, openai string) {
		t.Helper()
		t.Setenv("LOOM_ANTHROPIC_API_KEY", anthropic)
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("LOOM_RESPONSES_API_KEY", openai)
		t.Setenv("OPENAI_API_KEY", "")
	}
	outerChain := func(t *testing.T, ed council.Editor) *clients.FallbackCouncilEditor {
		t.Helper()
		fb, ok := ed.(*clients.FallbackCouncilEditor)
		if !ok {
			t.Fatalf("want *clients.FallbackCouncilEditor, got %T", ed)
		}
		return fb
	}

	t.Run("anthropic primary hops to openai gpt-5.5 then flexinfer by default", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		outer := outerChain(t, buildCouncilEditor(anthropicPolicy(), flexClient, t.TempDir(), nil, logger))
		if _, ok := outer.Primary.(*clients.AnthropicCouncilEditor); !ok {
			t.Fatalf("primary = %T, want anthropic editor", outer.Primary)
		}
		inner := outerChain(t, outer.Fallback)
		cross, ok := inner.Primary.(*clients.OpenAIResponsesCouncilEditor)
		if !ok {
			t.Fatalf("cross-vendor hop = %T, want OpenAI Responses editor", inner.Primary)
		}
		if cross.Model != mills.DefaultEditorCrossVendorFallback.Model {
			t.Fatalf("cross-vendor model = %q, want default %q", cross.Model, mills.DefaultEditorCrossVendorFallback.Model)
		}
		flex, ok := inner.Fallback.(*clients.FlexInferCouncilEditor)
		if !ok {
			t.Fatalf("local fallback = %T, want flexinfer editor", inner.Fallback)
		}
		if flex.Model != "" {
			t.Fatalf("local fallback model = %q, want empty (weaver-chain resolution)", flex.Model)
		}
		if outer.PrimaryLabel != "anthropic:claude-fable-5-1" || outer.FallbackLabel != "openai:gpt-5.5→flexinfer" {
			t.Fatalf("labels = %q → %q", outer.PrimaryLabel, outer.FallbackLabel)
		}
	})

	t.Run("missing openai key skips the hop instead of failing the build", func(t *testing.T) {
		setKeys(t, "anthropic-key", "")
		outer := outerChain(t, buildCouncilEditor(anthropicPolicy(), flexClient, t.TempDir(), nil, logger))
		if _, ok := outer.Fallback.(*clients.FlexInferCouncilEditor); !ok {
			t.Fatalf("fallback = %T, want flexinfer editor directly (no key, no hop)", outer.Fallback)
		}
	})

	t.Run("backend none opts out of the hop", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		p := anthropicPolicy()
		p.Council.Ensemble.EditorFallback = mills.CouncilAgent{Backend: "none"}
		outer := outerChain(t, buildCouncilEditor(p, flexClient, t.TempDir(), nil, logger))
		if _, ok := outer.Fallback.(*clients.FlexInferCouncilEditor); !ok {
			t.Fatalf("fallback = %T, want flexinfer editor directly (hop disabled)", outer.Fallback)
		}
	})

	t.Run("explicit editor_fallback pin wins over the default", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		p := anthropicPolicy()
		p.Council.Ensemble.EditorFallback = mills.CouncilAgent{Model: "gpt-5.6-sol", Backend: "openai-responses"}
		inner := outerChain(t, outerChain(t, buildCouncilEditor(p, flexClient, t.TempDir(), nil, logger)).Fallback)
		cross, ok := inner.Primary.(*clients.OpenAIResponsesCouncilEditor)
		if !ok || cross.Model != "gpt-5.6-sol" {
			t.Fatalf("cross-vendor hop = %T model %q, want OpenAI gpt-5.6-sol", inner.Primary, cross.Model)
		}
	})

	t.Run("openai primary has no default hop but honours an anthropic pin", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		p := &mills.Policy{}
		p.Council.Ensemble.Editor = mills.CouncilAgent{Name: "editor", Model: "gpt-5.5", Backend: "openai"}
		outer := outerChain(t, buildCouncilEditor(p, flexClient, t.TempDir(), nil, logger))
		if _, ok := outer.Fallback.(*clients.FlexInferCouncilEditor); !ok {
			t.Fatalf("fallback = %T, want flexinfer editor directly (no default hop for openai)", outer.Fallback)
		}
		p.Council.Ensemble.EditorFallback = mills.CouncilAgent{Model: "claude-fable-5-1", Backend: "anthropic"}
		inner := outerChain(t, outerChain(t, buildCouncilEditor(p, flexClient, t.TempDir(), nil, logger)).Fallback)
		if _, ok := inner.Primary.(*clients.AnthropicCouncilEditor); !ok {
			t.Fatalf("cross-vendor hop = %T, want anthropic editor", inner.Primary)
		}
	})

	t.Run("no flexinfer client leaves primary → cross-vendor only", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		outer := outerChain(t, buildCouncilEditor(anthropicPolicy(), nil, t.TempDir(), nil, logger))
		if _, ok := outer.Fallback.(*clients.OpenAIResponsesCouncilEditor); !ok {
			t.Fatalf("fallback = %T, want the OpenAI editor as the only fallback", outer.Fallback)
		}
	})

	t.Run("spinning room frames inherit the cross-vendor hop", func(t *testing.T) {
		setKeys(t, "anthropic-key", "openai-key")
		frame := mills.CouncilAgent{Name: "mule", Model: "claude-fable-5-1", Backend: "anthropic"}
		fallback, _ := anthropicPolicy().SpinningRoomFallback(frame)
		outer := outerChain(t, buildEditorChain(frame, fallback, "", flexClient, t.TempDir(), nil, logger))
		inner := outerChain(t, outer.Fallback)
		if _, ok := inner.Primary.(*clients.OpenAIResponsesCouncilEditor); !ok {
			t.Fatalf("frame fallback = %T, want OpenAI editor", inner.Primary)
		}
	})
}

func TestOpenRouterEditorChainAndTiebreaker(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test")
	t.Setenv("OPENROUTER_BASE_URL", "http://openrouter.invalid/api/v1")
	t.Setenv("LOOM_ANTHROPIC_API_KEY", "test")
	t.Setenv("LOOM_MILLS_GATE_TIEBREAKER_MODEL", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	flex, err := clients.NewFlexInferClient(clients.FlexInferConfig{ProxyURL: "http://flexinfer.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	pin := mills.CouncilAgent{Backend: "openrouter", Model: "anthropic/test"}
	ed := buildEditorChain(mills.CouncilAgent{Backend: "anthropic", Model: "claude-test"}, pin, "local-model", flex, "", nil, logger)
	outer := ed.(*clients.FallbackCouncilEditor)
	inner := outer.Fallback.(*clients.FallbackCouncilEditor)
	if _, ok := inner.Primary.(*clients.OpenRouterCouncilEditor); !ok {
		t.Fatalf("%T", inner.Primary)
	}
	if inner.Fallback.(*clients.FlexInferCouncilEditor).Model != "local-model" {
		t.Fatal("remote model leaked")
	}
	ed = buildEditorForAgent(pin, "local-model", flex, "", nil, logger)
	if _, ok := ed.(*clients.FallbackCouncilEditor).Primary.(*clients.OpenRouterCouncilEditor); !ok {
		t.Fatalf("%T", ed)
	}
	policy := mills.GateTiebreakerPolicy{Backend: "openrouter", Model: pin.Model, Fallbacks: []mills.GateJudgeHop{}}
	chain := buildGateTiebreaker(policy, Config{}, nil, logger)
	if len(chain.Hops) != 1 || chain.Hops[0].Vendor != "openrouter" || chain.Hops[0].Judge == nil {
		t.Fatal(chain)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	ed = buildEditorForAgent(pin, "local-model", flex, "", nil, logger)
	if ed.(*clients.FlexInferCouncilEditor).Model != "local-model" {
		t.Fatal("missing-key fallback leaked remote model")
	}
	chain = buildGateTiebreaker(policy, Config{}, nil, logger)
	if chain.Hops[0].Unavailable != "not_configured" {
		t.Fatal(chain)
	}
}
