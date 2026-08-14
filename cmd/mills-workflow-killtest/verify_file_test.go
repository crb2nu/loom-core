package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStrictRegularJSONWithSHA256AcceptsFileUnderLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	blob := []byte(`{"value":"under-limit"}`)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	var destination struct {
		Value string `json:"value"`
	}
	gotDigest, err := readStrictRegularJSONWithSHA256(path, &destination)
	if err != nil {
		t.Fatalf("readStrictRegularJSONWithSHA256() error = %v", err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(blob))
	if gotDigest != wantDigest || destination.Value != "under-limit" {
		t.Fatalf("read result = (%q, %q), want (%q, %q)",
			gotDigest, destination.Value, wantDigest, "under-limit")
	}
}

func TestReadStrictRegularJSONWithSHA256RejectsOversizedSparseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-evidence.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxVerifierEvidenceFileBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var destination any
	_, err = readStrictRegularJSONWithSHA256(path, &destination)
	want := fmt.Sprintf("exceeds the %d-byte evidence file limit", maxVerifierEvidenceFileBytes)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("readStrictRegularJSONWithSHA256() error = %v, want %q", err, want)
	}
}

func TestDecodeStrictJSONRejectsArrayAllocationAmplification(t *testing.T) {
	var raw strings.Builder
	raw.WriteString(`{"items":[`)
	for index := 0; index <= maxVerifierJSONContainerEntries; index++ {
		if index > 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{}`)
	}
	raw.WriteString(`]}`)
	var destination struct {
		Items []struct{} `json:"items"`
	}
	err := decodeStrictJSON([]byte(raw.String()), &destination)
	if err == nil || !strings.Contains(err.Error(), "array element count exceeds") {
		t.Fatalf("amplified array accepted: %v", err)
	}
	if destination.Items != nil {
		t.Fatalf("typed array allocated before cardinality rejection: len=%d", len(destination.Items))
	}
}

func TestDecodeStrictJSONRejectsMapAllocationAmplification(t *testing.T) {
	var raw strings.Builder
	raw.WriteString(`{"values":{`)
	for index := 0; index <= maxVerifierJSONContainerEntries; index++ {
		if index > 0 {
			raw.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&raw, `%q:0`, fmt.Sprintf("key-%d", index))
	}
	raw.WriteString(`}}`)
	var destination struct {
		Values map[string]int `json:"values"`
	}
	err := decodeStrictJSON([]byte(raw.String()), &destination)
	if err == nil || !strings.Contains(err.Error(), "object field count exceeds") {
		t.Fatalf("amplified map accepted: %v", err)
	}
	if destination.Values != nil {
		t.Fatalf("typed map allocated before cardinality rejection: len=%d", len(destination.Values))
	}
}

func TestDecodeStrictJSONRejectsAggregateTokenAmplification(t *testing.T) {
	var raw strings.Builder
	raw.WriteString(`{"groups":[`)
	groupCount := maxVerifierJSONTokens/maxVerifierJSONContainerEntries + 1
	for group := 0; group < groupCount; group++ {
		if group > 0 {
			raw.WriteByte(',')
		}
		raw.WriteByte('[')
		for index := 0; index < maxVerifierJSONContainerEntries; index++ {
			if index > 0 {
				raw.WriteByte(',')
			}
			raw.WriteByte('0')
		}
		raw.WriteByte(']')
	}
	raw.WriteString(`]}`)
	var destination struct {
		Groups [][]int `json:"groups"`
	}
	err := decodeStrictJSON([]byte(raw.String()), &destination)
	if err == nil || !strings.Contains(err.Error(), "token count exceeds") {
		t.Fatalf("aggregate token amplification accepted: %v", err)
	}
	if destination.Groups != nil {
		t.Fatalf("typed nested arrays allocated before token rejection: len=%d", len(destination.Groups))
	}
}

func TestDecodeStrictJSONRejectsScalarAndDepthAmplification(t *testing.T) {
	for name, raw := range map[string]string{
		"string": `{"value":"` + strings.Repeat("x", maxVerifierJSONScalarBytes+1) + `"}`,
		"number": `{"value":1` + strings.Repeat("0", maxVerifierJSONScalarBytes) + `}`,
		"depth":  strings.Repeat("[", maxVerifierJSONNestingDepth+1) + "0" + strings.Repeat("]", maxVerifierJSONNestingDepth+1),
	} {
		t.Run(name, func(t *testing.T) {
			var destination any
			if err := decodeStrictJSON([]byte(raw), &destination); err == nil ||
				(!strings.Contains(err.Error(), "token exceeds") && !strings.Contains(err.Error(), "nesting depth exceeds")) {
				t.Fatalf("%s amplification accepted: %v", name, err)
			}
			if destination != nil {
				t.Fatalf("typed destination allocated before %s rejection: %#v", name, destination)
			}
		})
	}
}

func TestDecodeStrictJSONPreservesUnknownAndTrailingRejection(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":   `{"value":1,"unknown":true}`,
		"multiple values": `{"value":1} {"value":2}`,
		"trailing junk":   `{"value":1} attacker`,
	} {
		t.Run(name, func(t *testing.T) {
			var destination struct {
				Value int `json:"value"`
			}
			if err := decodeStrictJSON([]byte(raw), &destination); err == nil ||
				(!strings.Contains(err.Error(), "unknown field") &&
					!strings.Contains(err.Error(), "multiple JSON values") &&
					!strings.Contains(err.Error(), "trailing JSON")) {
				t.Fatalf("strict JSON rejection changed for %s: %v", name, err)
			}
		})
	}
}
