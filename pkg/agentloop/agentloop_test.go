package agentloop

import (
	"context"
	"testing"
)

func TestSelfCheckPasses(t *testing.T) {
	rep, err := SelfCheck(context.Background())
	if err != nil {
		t.Fatalf("SelfCheck infra error: %v", err)
	}
	if !rep.Passed {
		for _, c := range rep.Checks {
			if !c.Passed {
				t.Errorf("check %q failed: %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("SelfCheck did not pass")
	}
	if len(rep.Checks) != 5 {
		t.Fatalf("expected 5 checks, got %d", len(rep.Checks))
	}
}

func TestConversationAppendOnly(t *testing.T) {
	c := NewConversation("sys")
	c.Append(Message{Role: RoleUser, Content: "hi"})
	c.Append(Message{Role: RoleAssistant, Content: "yo"})
	msgs := c.Messages()
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (system+2), got %d", len(msgs))
	}
	if msgs[0].Role != RoleSystem {
		t.Errorf("first message role=%q want system", msgs[0].Role)
	}
	// Mutating the returned slice must not affect internal state.
	msgs[1].Content = "TAMPERED"
	if c.Messages()[1].Content != "hi" {
		t.Errorf("internal history was mutated through returned slice")
	}
}

func TestBudgetCheck(t *testing.T) {
	b := Budget{MaxModelLen: 1000, SystemTokens: 100, OutputReserve: 200}
	if got := b.Usable(); got != 700 {
		t.Errorf("Usable=%d want 700", got)
	}
	if got := b.PromptCeiling(); got != 800 {
		t.Errorf("PromptCeiling=%d want 800", got)
	}
	if be := b.Check(800); be != nil {
		t.Errorf("Check(800) at ceiling should be nil, got %v", be)
	}
	be := b.Check(801)
	if be == nil {
		t.Fatalf("Check(801) over ceiling should return a BudgetError")
	}
	if be.OverBy != 1 {
		t.Errorf("OverBy=%d want 1", be.OverBy)
	}
}

func TestBudgetClampsNegative(t *testing.T) {
	b := Budget{MaxModelLen: 100, SystemTokens: 200, OutputReserve: 50}
	if got := b.Usable(); got != 0 {
		t.Errorf("Usable=%d want 0 (clamped)", got)
	}
}

func TestRegistryDuplicate(t *testing.T) {
	mk := func(name string) Tool {
		return FunctionTool{Def: ToolDef{Function: ToolFunctionDef{Name: name}}}
	}
	if _, err := NewRegistry(mk("a"), mk("a")); err == nil {
		t.Fatalf("duplicate tool name should error")
	}
	if _, err := NewRegistry(mk("")); err == nil {
		t.Fatalf("empty tool name should error")
	}
	reg, err := NewRegistry(mk("a"), mk("b"))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if reg.Len() != 2 {
		t.Errorf("Len=%d want 2", reg.Len())
	}
	if defs := reg.Definitions(); len(defs) != 2 || defs[0].Function.Name != "a" {
		t.Errorf("Definitions order not preserved: %+v", defs)
	}
}

// budgetCompleter returns a *BudgetError on the first turn.
type budgetCompleter struct{}

func (budgetCompleter) Complete(_ context.Context, _ []Message, _ []ToolDef, _ int) (Message, TurnMetrics, error) {
	return Message{}, TurnMetrics{}, &BudgetError{PromptTokens: 999, PromptCeiling: 800, OverBy: 199}
}

func TestEngineBudgetStopIsCleanExit(t *testing.T) {
	eng := &Engine{Client: budgetCompleter{}, MaxRounds: 4, OutputTokens: 16}
	res, err := eng.Run(context.Background(), NewConversation("sys"), "go")
	if err != nil {
		t.Fatalf("budget overflow should be a clean stop, got err=%v", err)
	}
	if res.Stopped != StopBudget {
		t.Errorf("Stopped=%q want %q", res.Stopped, StopBudget)
	}
}

func TestResolveInRootRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveInRoot(root, "../../etc/passwd"); err == nil {
		t.Errorf("escape via .. should be rejected")
	}
	if _, err := ResolveInRoot(root, "/etc/passwd"); err == nil {
		t.Errorf("absolute path should be rejected")
	}
	if _, err := ResolveInRoot(root, "sub/file.txt"); err != nil {
		t.Errorf("legit relative path rejected: %v", err)
	}
}
