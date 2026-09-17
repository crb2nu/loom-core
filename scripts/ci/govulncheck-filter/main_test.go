package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const header = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scan_level":"package","scan_mode":"source"}}
{"SBOM":{"modules":[{"path":"github.com/xuri/excelize/v2"}]}}
`

func report(id, mod, version, modified string) string {
	osv, _ := json.Marshal(map[string]any{"osv": map[string]any{"id": advisory, "modified": modified}})
	finding, _ := json.Marshal(map[string]any{"finding": map[string]any{"osv": id, "trace": []any{map[string]any{"module": mod, "version": version, "package": mod}}}})
	return header + string(osv) + "\n" + string(finding)
}
func TestCorrectionScope(t *testing.T) {
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, id, mod, version, modified string
		count                            int
	}{
		{"fixed", advisory, module, fixedPin, reviewedModified, 1},
		{"old", advisory, module, "v2.11.0", reviewedModified, 0},
		{"different advisory", "GO-2026-9999", module, fixedPin, reviewedModified, 0},
		{"different module", advisory, "example.com/other", fixedPin, reviewedModified, 0},
		{"future pin", advisory, module, "v2.12.0", reviewedModified, 0},
		{"updated advisory", advisory, module, fixedPin, "2026-09-18T00:00:00Z", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := report(tt.id, tt.mod, tt.version, tt.modified)
			got, n, err := filter(strings.NewReader(raw), now)
			if err != nil || n != tt.count || len(got) != 4-tt.count {
				t.Fatalf("events=%d count=%d err=%v", len(got), n, err)
			}
			if tt.count == 0 && !strings.Contains(string(got[3]), tt.id) {
				t.Fatal("finding was not preserved")
			}
		})
	}
}
func TestCorrectionFailsClosed(t *testing.T) {
	valid := report(advisory, module, fixedPin, reviewedModified)
	for _, raw := range []string{"", "null", "{}", "{", strings.Replace(valid, "package", "symbol", 1), strings.Replace(valid, "source", "binary", 1), strings.Replace(valid, "govulncheck", "other", 1), valid + "\n{", valid + `{"error":"scanner failed"}`, valid + `{"finding":{"osv":"unknown"}}`, header + `{"finding":null}`, header + `{"osv":null}`, header + `{"finding":{"osv":"unknown","trace":[{}]}}`, header + valid} {
		if _, _, err := filter(strings.NewReader(raw), time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("accepted invalid report: %.120s", raw)
		}
	}
	if _, _, err := filter(strings.NewReader(valid), expires); err == nil {
		t.Fatal("accepted expired correction")
	}
	if _, n, err := filter(strings.NewReader(header), expires); err != nil || n != 0 {
		t.Fatalf("clean report should not need correction: n=%d err=%v", n, err)
	}
}
func TestOtherFindingsSurviveCorrection(t *testing.T) {
	raw := report(advisory, module, fixedPin, reviewedModified) + `{"finding":{"osv":"GO-OTHER","trace":[{"module":"stdlib","version":"v1.0.0","package":"net/http"}]}}`
	got, n, err := filter(strings.NewReader(raw), time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 || len(got) != 4 || !strings.Contains(string(got[3]), "GO-OTHER") {
		t.Fatalf("count=%d events=%d err=%v", n, len(got), err)
	}
}
