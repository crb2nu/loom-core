package clients

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

type rubricResponsesStub func(context.Context, openairesponses.TurnRequest) (openairesponses.TurnResponse, error)

func (f rubricResponsesStub) Create(c context.Context, r openairesponses.TurnRequest) (openairesponses.TurnResponse, error) {
	return f(c, r)
}

func TestOpenAIRubricJudge(t *testing.T) {
	for _, kind := range []string{"success", "parse", "refusal", "billing", "breaker"} {
		t.Run(kind, func(t *testing.T) {
			b := NewVendorBreaker(nil)
			calls := 0
			in := gates.StageInput{}
			j := &OpenAIRubricJudge{Model: "gpt-5.5", Breaker: b, Client: rubricResponsesStub(func(ctx context.Context, r openairesponses.TurnRequest) (openairesponses.TurnResponse, error) {
				calls++
				if llmusage.ComponentFrom(ctx) != ComponentJudge {
					t.Fatal("missing usage attribution")
				}
				if r.Model != "gpt-5.5" || r.Input != composePrompt(defaultRubricBody("spec_conformance"), in) || r.Context.Mode != openairesponses.ContextModeStateless {
					t.Fatal(r)
				}
				switch kind {
				case "billing":
					return openairesponses.TurnResponse{}, &openairesponses.APIError{Status: 429, Code: "insufficient_quota"}
				case "parse":
					return openairesponses.TurnResponse{OutputText: "oops"}, nil
				case "refusal":
					return openairesponses.TurnResponse{RawPayload: json.RawMessage(`{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`)}, nil
				}
				return openairesponses.TurnResponse{OutputText: `{"score":0.9,"reasons":["fine"]}`}, nil
			})}
			if kind == "breaker" {
				b.Trip("openai", VendorBilling, time.Minute)
			}
			v, err := j.Judge(context.Background(), "spec_conformance", in)
			switch kind {
			case "success":
				if err != nil || v.Score != .9 || v.Model != "gpt-5.5" {
					t.Fatal(v, err)
				}
			case "parse":
				if !errors.Is(err, ErrRubricUnparseable) {
					t.Fatal(err)
				}
			case "refusal":
				e, ok := AsVendorError(err)
				if !ok || e.Kind != VendorRefusal || b.Open("openai") {
					t.Fatal(err)
				}
			case "billing", "breaker":
				if !IsVendorBilling(err) || !b.Open("openai") {
					t.Fatal(err)
				}
			}
			if kind == "breaker" && calls != 0 {
				t.Fatal(calls)
			}
		})
	}
}
