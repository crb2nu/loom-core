package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

func TestNormalizeImageBase64_RawBase64(t *testing.T) {
	got, err := normalizeImageBase64("AQID")
	if err != nil {
		t.Fatalf("normalizeImageBase64 returned error: %v", err)
	}
	if got != "AQID" {
		t.Fatalf("unexpected payload\nwant: %q\ngot:  %q", "AQID", got)
	}
}

func TestNormalizeImageBase64_StripsDataURL(t *testing.T) {
	got, err := normalizeImageBase64("data:image/jpeg;base64,AQID")
	if err != nil {
		t.Fatalf("normalizeImageBase64 returned error: %v", err)
	}
	if got != "AQID" {
		t.Fatalf("expected raw base64 without data URL prefix\nwant: %q\ngot:  %q", "AQID", got)
	}
}

func TestNormalizeImageBase64_RawUnpadded(t *testing.T) {
	got, err := normalizeImageBase64("AQIDBA")
	if err != nil {
		t.Fatalf("normalizeImageBase64 returned error: %v", err)
	}
	if got != "AQIDBA==" {
		t.Fatalf("expected re-padded standard base64\nwant: %q\ngot:  %q", "AQIDBA==", got)
	}
}

func TestNormalizeImageBase64_InvalidData(t *testing.T) {
	if _, err := normalizeImageBase64("not-base64@@@"); err == nil {
		t.Fatalf("expected error for invalid base64 payload")
	}
}

func TestNormalizeImageBase64_InvalidDataURL(t *testing.T) {
	if _, err := normalizeImageBase64("data:image/png,abc"); err == nil {
		t.Fatalf("expected error for non-base64 data URL payload")
	}
}

func TestPythonCandidates_Order(t *testing.T) {
	venv := t.TempDir()
	binDir := filepath.Join(venv, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	venvPy := filepath.Join(binDir, "python3")
	if err := os.WriteFile(venvPy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BROWSERKIT_PYTHON", "/custom/python")
	t.Setenv("BROWSERKIT_VENV_DIR", venv)

	got := pythonCandidates()
	want := []string{"/custom/python", venvPy, "python3"}
	if len(got) != len(want) {
		t.Fatalf("unexpected candidates: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPythonCandidates_NoOverride(t *testing.T) {
	t.Setenv("BROWSERKIT_PYTHON", "")
	t.Setenv("BROWSERKIT_VENV_DIR", filepath.Join(t.TempDir(), "missing"))

	got := pythonCandidates()
	want := []string{"python3"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("unexpected candidates: %v, want %v", got, want)
	}
}

func TestHandleScreenshot_ValidatesParams(t *testing.T) {
	t.Run("rejects unsupported scheme", func(t *testing.T) {
		res, err := handleScreenshot(context.Background(), map[string]any{
			"url": "javascript:alert(1)",
		})
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected error result, got %+v", res)
		}
		if got := toolResultText(res); !strings.Contains(got, "invalid parameter 'url'") {
			t.Fatalf("expected url validation error, got: %q", got)
		}
	})

	t.Run("rejects invalid format", func(t *testing.T) {
		res, err := handleScreenshot(context.Background(), map[string]any{
			"url":    "https://example.com",
			"format": "gif",
		})
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected error result, got %+v", res)
		}
		if got := toolResultText(res); !strings.Contains(got, "invalid parameter 'format'") {
			t.Fatalf("expected format validation error, got: %q", got)
		}
	})
}

func toolResultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.Text)
	}
	return b.String()
}
