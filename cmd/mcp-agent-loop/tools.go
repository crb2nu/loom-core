package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/agentloop"
	"github.com/crb2nu/loom/pkg/validate"
)

const defaultSystemPrompt = "You are a helpful coding assistant operating in an append-only tool loop."

func handleAgentLoopRun(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	model := v.Required("model")
	prompt := v.Required("prompt")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}

	workdir := v.String("workdir", ".")
	endpoint := v.String("endpoint", defaultEndpoint())
	system := v.String("system", defaultSystemPrompt)
	session := v.String("session", "")
	if session == "" {
		session = newSessionID()
	}
	maxModelLen := v.Int("max_model_len", 20480)
	maxTokens := v.Int("max_tokens", 512)
	maxRounds := v.Int("max_rounds", 20)
	systemTokens := v.Int("system_tokens", 0)
	temperature := v.Float("temperature", 0)
	wantPrefixHit := v.Bool("want_prefix_hit", true)
	if systemTokens <= 0 {
		systemTokens = agentloop.EstimateTokens(system)
	}

	tools, err := agentloop.FSTools(workdir)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("workdir: %w", err)), nil
	}
	reg, err := agentloop.NewRegistry(tools...)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	client, err := agentloop.NewChatClient(agentloop.ChatClientConfig{
		Endpoint:      endpoint,
		Model:         model,
		CacheKey:      session,
		Temperature:   temperature,
		WantPrefixHit: wantPrefixHit,
	})
	if err != nil {
		return mcp.ErrorResult(err), nil
	}

	budget := agentloop.Budget{
		MaxModelLen:   maxModelLen,
		SystemTokens:  systemTokens,
		OutputReserve: maxTokens,
	}
	eng := &agentloop.Engine{
		Client:       client,
		Registry:     reg,
		Budget:       budget,
		MaxRounds:    maxRounds,
		OutputTokens: maxTokens,
	}

	conv := agentloop.NewConversation(system)
	res, err := eng.Run(ctx, conv, prompt)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("agent loop: %w", err)), nil
	}

	return mcp.JSONResult(map[string]any{
		"schema":     "loom.agent_loop.v1",
		"model":      model,
		"session_id": session,
		"endpoint":   endpoint,
		"budget": map[string]any{
			"max_model_len":  budget.MaxModelLen,
			"system_tokens":  budget.SystemTokens,
			"output_reserve": budget.OutputReserve,
			"usable":         budget.Usable(),
		},
		"result": res,
	})
}

func handleAgentLoopSelfCheck(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
	rep, err := agentloop.SelfCheck(ctx)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("self-check infra error: %w", err)), nil
	}
	return mcp.JSONResult(rep)
}

// defaultEndpoint resolves the proxy base URL from the environment, falling
// back to the local port-forward convention.
func defaultEndpoint() string {
	if v := os.Getenv("FLEXINFER_PROXY_URL"); v != "" {
		return v
	}
	return "http://localhost:18080"
}

// newSessionID returns a random hex id used to pin the prefix-cache routing
// key when the caller does not supply one.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "agent-loop-session"
	}
	return "agent-loop-" + hex.EncodeToString(b[:])
}
