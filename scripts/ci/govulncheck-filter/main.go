// govulncheck-filter applies a temporary, exact-version advisory correction.
// The filtered stream MUST still pass through govulncheck -mode=convert.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const advisory = "GO-2026-6452"
const module = "github.com/xuri/excelize/v2"
const fixedPin = "v2.11.1-0.20260728235842-f98df08a8f6a"
const reviewedModified = "2026-09-16T18:00:43Z"

// See docs/security/excelize-advisory-correction.md for upstream fixes and scope.
var expires = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: govulncheck-filter INPUT.json OUTPUT.json")
		os.Exit(1)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(input, output string) error {
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	messages, count, err := filter(in, time.Now().UTC())
	if err != nil {
		return err
	}
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	for _, m := range messages {
		if err = encoder.Encode(m); err != nil {
			_ = out.Close()
			return err
		}
	}
	if err = out.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "govulncheck: applied %d exact %s correction(s); expires %s; native conversion still required\n", count, advisory, expires.Format(time.DateOnly))
	return nil
}

func filter(reader io.Reader, now time.Time) ([]json.RawMessage, int, error) {
	decoder := json.NewDecoder(reader)
	var messages []json.RawMessage
	configCount, sbomCount := 0, 0
	reviewed := false
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, 0, err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, 0, err
		}
		if len(m) != 1 {
			return nil, 0, errors.New("expected one govulncheck event per object")
		}
		for key, payload := range m {
			if data := bytes.TrimSpace(payload); len(data) == 0 || data[0] != '{' {
				return nil, 0, fmt.Errorf("invalid govulncheck %s event payload", key)
			}
			switch key {
			case "config", "SBOM", "progress", "osv", "finding":
			default:
				return nil, 0, fmt.Errorf("unknown govulncheck event %q", key)
			}
		}
		if data, ok := m["config"]; ok {
			configCount++
			var c struct {
				Protocol string `json:"protocol_version"`
				Scanner  string `json:"scanner_name"`
				Level    string `json:"scan_level"`
				Mode     string `json:"scan_mode"`
			}
			if err := json.Unmarshal(data, &c); err != nil {
				return nil, 0, err
			}
			if c.Protocol != "v1.0.0" || c.Scanner != "govulncheck" || c.Level != "package" || c.Mode != "source" {
				return nil, 0, errors.New("requires govulncheck source/package report with protocol v1.0.0")
			}
		}
		if data, ok := m["SBOM"]; ok {
			sbomCount++
			var sbom struct {
				Modules []json.RawMessage `json:"modules"`
			}
			if err := json.Unmarshal(data, &sbom); err != nil {
				return nil, 0, err
			}
			if len(sbom.Modules) == 0 {
				return nil, 0, errors.New("empty govulncheck SBOM")
			}
		}
		if data, ok := m["osv"]; ok {
			var osv struct {
				ID       string `json:"id"`
				Modified string `json:"modified"`
			}
			if err := json.Unmarshal(data, &osv); err != nil {
				return nil, 0, err
			}
			if osv.ID == advisory {
				reviewed = osv.Modified == reviewedModified
			}
		}
		messages = append(messages, raw)
	}
	if configCount != 1 || sbomCount != 1 {
		return nil, 0, errors.New("incomplete govulncheck report: exactly one config and SBOM required")
	}
	kept := make([]json.RawMessage, 0, len(messages))
	count := 0
	for _, raw := range messages {
		var m struct {
			Finding *struct {
				OSV   string `json:"osv"`
				Trace []struct {
					Module   string `json:"module"`
					Version  string `json:"version"`
					Package  string `json:"package"`
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, 0, err
		}
		if f := m.Finding; f != nil {
			if f.OSV == "" || len(f.Trace) == 0 || f.Trace[0].Module == "" {
				return nil, 0, errors.New("invalid govulncheck finding")
			}
			frame := f.Trace[0]
			if reviewed && f.OSV == advisory && len(f.Trace) == 1 && frame.Module == module && frame.Version == fixedPin && (frame.Package == module || frame.Package == "") && frame.Function == "" {
				if !now.Before(expires) {
					return nil, 0, errors.New("excelize advisory correction expired; review upstream metadata")
				}
				count++
				continue
			}
		}
		kept = append(kept, raw)
	}
	return kept, count, nil
}
