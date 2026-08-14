package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCIClassifyCommand_TextFromStdin(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	in := strings.NewReader("fatal: connection refused while cloning\n")
	if err := runCIClassifyCommand(&out, in, "", false); err != nil {
		t.Fatalf("runCIClassifyCommand: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"CI classification: transient",
		"Retryable: true",
		"Evidence:",
		"connection refused",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunCIClassifyCommand_JSONFromFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(path, []byte("merge failed: status 405 Method Not Allowed\n"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out bytes.Buffer
	if err := runCIClassifyCommand(&out, nil, path, true); err != nil {
		t.Fatalf("runCIClassifyCommand: %v", err)
	}
	var got struct {
		Class    string `json:"class"`
		Terminal bool   `json:"terminal"`
		Lines    int    `json:"lines"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got.Class != "configuration" {
		t.Fatalf("Class = %q, want configuration", got.Class)
	}
	if !got.Terminal {
		t.Fatal("Terminal = false, want true")
	}
	if got.Lines != 1 {
		t.Fatalf("Lines = %d, want 1", got.Lines)
	}
}

func TestReadCIClassifyInput_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := readCIClassifyInput(nil, filepath.Join(t.TempDir(), "missing.log"))
	if err == nil {
		t.Fatal("expected missing-file error")
	}
	if !strings.Contains(err.Error(), "read CI log") {
		t.Fatalf("error = %v, want read hint", err)
	}
}
