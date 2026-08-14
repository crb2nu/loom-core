package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetryAccountingVocabulary(t *testing.T) {
	payload, err := json.Marshal(RetryAccountingSnapshot{
		Total:   RetryAccountingTotals{RetryCostUSD: 1.5, AutoRequeues: 2},
		ByClass: []RetryAccountingByClass{{Class: "code", RetryCostUSD: 1.5, AutoRequeues: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{RetryCostUSDField, AutoRequeuesField, FailureClassField} {
		if !strings.Contains(string(payload), `"`+field+`"`) {
			t.Errorf("payload %s missing %q", payload, field)
		}
	}
}
