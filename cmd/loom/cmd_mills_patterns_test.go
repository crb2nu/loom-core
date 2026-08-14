package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func TestParseMaterialsArg_InlineJSON(t *testing.T) {
	mats, err := parseMaterialsArg(`{"service_name": "foo", "port": 8080}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mats["service_name"] != "foo" {
		t.Errorf("service_name = %v, want foo", mats["service_name"])
	}
	if _, ok := mats["port"]; !ok {
		t.Errorf("port missing from parsed materials: %v", mats)
	}
}

func TestParseMaterialsArg_AtFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "materials.json")
	if err := os.WriteFile(path, []byte(`{"service_name": "bar"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mats, err := parseMaterialsArg("@" + path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mats["service_name"] != "bar" {
		t.Errorf("service_name = %v, want bar", mats["service_name"])
	}
}

func TestParseMaterialsArg_BarePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "materials.json")
	if err := os.WriteFile(path, []byte(`{"service_name": "baz"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mats, err := parseMaterialsArg(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mats["service_name"] != "baz" {
		t.Errorf("service_name = %v, want baz", mats["service_name"])
	}
}

func TestParseMaterialsArg_Errors(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not-an-object", `["a", "b"]`},
		{"empty-object", `{}`},
		{"malformed", `{not json`},
		{"missing-file", "@/nonexistent/path/materials.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMaterialsArg(tc.arg); err == nil {
				t.Errorf("parseMaterialsArg(%q) = nil error, want error", tc.arg)
			}
		})
	}
}

// TestMillsStampCmd_RequiresPattern asserts the stamp command rejects a missing
// --pattern flag with a clear error before any daemon dial.
func TestMillsStampCmd_RequiresPattern(t *testing.T) {
	cmd := newMillsStampCmd()
	cmd.SetArgs([]string{"--materials", `{"service_name": "foo"}`})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --pattern, got nil")
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("error = %q, want it to mention pattern", err.Error())
	}
}

// TestMillsStampCmd_InvalidMaterialsFailsBeforeDial asserts a malformed
// --materials value errors out during arg parsing, not at the daemon.
func TestMillsStampCmd_InvalidMaterialsFailsBeforeDial(t *testing.T) {
	cmd := newMillsStampCmd()
	cmd.SetArgs([]string{"--pattern", "pattern-go-rest-service", "--materials", `{bad json`})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed --materials, got nil")
	}
	if !strings.Contains(err.Error(), "materials") {
		t.Errorf("error = %q, want it to mention materials", err.Error())
	}
}

// TestMillsPatternsCmd_FlagsRegistered asserts the patterns command exposes its
// --status flag (smoke test on command construction).
func TestMillsPatternsCmd_FlagsRegistered(t *testing.T) {
	cmd := newMillsPatternsCmd()
	if cmd.Flags().Lookup("status") == nil {
		t.Error("patterns command missing --status flag")
	}
	if cmd.Use != "patterns" {
		t.Errorf("Use = %q, want patterns", cmd.Use)
	}
}

// TestRenderPatternsTable asserts the table renderer emits a header and a row
// per pattern with the id and makes columns populated.
func TestRenderPatternsTable(t *testing.T) {
	var buf bytes.Buffer
	renderPatternsTable(&buf, []bridge.PatternInfo{
		{ID: "pattern-go-rest-service", Makes: "Go REST microservice", Version: "0.1", Status: "approved", MaterialsSchema: []bridge.PatternMaterialField{{Name: "service_name"}}},
	})
	out := buf.String()
	if !strings.Contains(out, "PATTERN ID") {
		t.Errorf("table missing header, got:\n%s", out)
	}
	if !strings.Contains(out, "pattern-go-rest-service") {
		t.Errorf("table missing pattern id row, got:\n%s", out)
	}
	if !strings.Contains(out, "Go REST microservice") {
		t.Errorf("table missing makes column, got:\n%s", out)
	}
}
