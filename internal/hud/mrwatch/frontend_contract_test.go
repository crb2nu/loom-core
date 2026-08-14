package mrwatch

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The frontend consumes this committed taxonomy fixture. Keep it exactly in
// lockstep with AllStates so adding a backend state cannot silently render as
// an unknown HUD badge.
func TestFrontendStateTaxonomyMatchesAllStates(t *testing.T) {
	data, err := os.ReadFile("../frontend/src/testdata/mrwatch-states.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	wantStates := AllStates()
	want := make([]string, len(wantStates))
	for i, state := range wantStates {
		want[i] = string(state)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frontend MR state taxonomy = %v, want %v", got, want)
	}
}
