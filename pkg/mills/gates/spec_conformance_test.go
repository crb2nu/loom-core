package gates

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

type capturingRubricJudge struct {
	inputs []StageInput
}

func (j *capturingRubricJudge) Judge(_ context.Context, _ string, in StageInput) (RubricVerdict, error) {
	j.inputs = append(j.inputs, in)
	return RubricVerdict{Score: 0.9, Model: "fixture"}, nil
}

func TestSpecConformanceGateCanonicalizesJudgeInputWithoutMutation(t *testing.T) {
	a := []byte("diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-b\n+B\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+A\n")
	b := []byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+A\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-b\n+B\n")
	filesA := []string{"b.go", "a.go"}
	filesB := []string{"a.go", "b.go"}
	originalA := append([]byte(nil), a...)
	originalFilesA := append([]string(nil), filesA...)
	judge := &capturingRubricJudge{}
	gate := NewSpecConformanceGate(judge)

	first, err := gate.Evaluate(context.Background(), StageInput{FilesChanged: filesA, DiffPatch: a})
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.Evaluate(context.Background(), StageInput{FilesChanged: filesB, DiffPatch: b})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("verdicts differ: %+v != %+v", first, second)
	}
	if len(judge.inputs) != 2 || !reflect.DeepEqual(judge.inputs[0].FilesChanged, judge.inputs[1].FilesChanged) || !bytes.Equal(judge.inputs[0].DiffPatch, judge.inputs[1].DiffPatch) {
		t.Fatalf("judge inputs differ: %#v %#v", judge.inputs[0], judge.inputs[1])
	}
	if !reflect.DeepEqual(filesA, originalFilesA) || !bytes.Equal(a, originalA) {
		t.Fatalf("caller input mutated: files=%v diff=%q", filesA, a)
	}
}

func TestCanonicalizeUnifiedDiffEdgeCases(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"empty", "", ""},
		{"headerless", "--- a/a.go\n+++ b/a.go\n", "--- a/a.go\n+++ b/a.go\n"},
		{"malformed preserved", "diff --git incomplete\nbody\ndiff --git a/a b/a\n", "diff --git incomplete\nbody\ndiff --git a/a b/a\n"},
		{"new deleted binary rename and quoted", "preamble\ndiff --git a/z.bin b/z.bin\nBinary files a/z.bin and b/z.bin differ\ndiff --git a/old.go b/renamed.go\nsimilarity index 100%\nrename from old.go\nrename to renamed.go\ndiff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\ndiff --git \"a/a space.go\" \"b/a space.go\"\n@@ -1 +1 @@\n-x\n+y\n", "preamble\ndiff --git \"a/a space.go\" \"b/a space.go\"\n@@ -1 +1 @@\n-x\n+y\ndiff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\ndiff --git a/old.go b/renamed.go\nsimilarity index 100%\nrename from old.go\nrename to renamed.go\ndiff --git a/z.bin b/z.bin\nBinary files a/z.bin and b/z.bin differ\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(canonicalizeUnifiedDiff([]byte(tt.input))); got != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}
