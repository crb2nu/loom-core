package shiftreport

import (
	"os"
	"testing"
)

func assertGolden(t *testing.T, path, got string) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("markdown mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
