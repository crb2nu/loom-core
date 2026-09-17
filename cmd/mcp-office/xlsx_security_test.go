package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// Exercise both shared-string paths: the advisory was fixed for in-memory
// strings in v2.11.0, but disk-backed strings still panicked before PR #2366.
func TestXLSXSharedStringIndexBounds(t *testing.T) {
	for _, limit := range []int64{0, 1} {
		for _, index := range []string{"0", "-1", "-2147483648"} {
			for _, method := range []string{"cell", "rows", "iterator"} {
				t.Run(fmt.Sprintf("limit=%d/index=%s/%s", limit, index, method), func(t *testing.T) {
					f, err := excelize.OpenReader(bytes.NewReader(sharedStringWorkbook(t, index)), excelize.Options{UnzipXMLSizeLimit: limit})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = f.Close() })
					var value string
					switch method {
					case "cell":
						value, err = f.GetCellValue("Sheet1", "A1")
					case "rows":
						var rows [][]string
						rows, err = f.GetRows("Sheet1")
						if len(rows) > 0 && len(rows[0]) > 0 {
							value = rows[0][0]
						}
					case "iterator":
						var rows *excelize.Rows
						rows, err = f.Rows("Sheet1")
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = rows.Close() }()
						if !rows.Next() {
							t.Fatal("missing fixture row")
						}
						var cells []string
						cells, err = rows.Columns()
						if len(cells) > 0 {
							value = cells[0]
						}
					}
					if index == "0" {
						if err != nil || value != "ok" {
							t.Fatalf("valid string: value=%q err=%v", value, err)
						}
					} else {
						// GetRows discards column errors upstream; it must at least avoid a
						// panic and never return the indexed string for malformed input.
						if value != "" {
							t.Fatalf("malformed index returned %q", value)
						}
						if method == "cell" && (err == nil || !strings.Contains(err.Error(), "invalid shared string index")) {
							t.Fatalf("expected invalid-index error, got %v", err)
						}
					}
				})
			}
		}
	}
}

func sharedStringWorkbook(t *testing.T, index string) []byte {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetCellStr("Sheet1", "A1", "ok"); err != nil {
		t.Fatal(err)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	changed := false
	for _, entry := range zr.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == "xl/worksheets/sheet1.xml" {
			if !bytes.Contains(data, []byte("<v>0</v>")) {
				t.Fatal("fixture shared-string reference missing")
			}
			data = bytes.Replace(data, []byte("<v>0</v>"), []byte("<v>"+index+"</v>"), 1)
			changed = true
		}
		w, err := zw.Create(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if !changed {
		t.Fatal("fixture worksheet missing")
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
