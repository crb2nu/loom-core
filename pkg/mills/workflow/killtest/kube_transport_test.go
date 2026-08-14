package killtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

func writeKubectlHelperBytes(destination io.Writer, count int) {
	chunk := strings.Repeat("x", 32<<10)
	for count > 0 {
		write := len(chunk)
		if write > count {
			write = count
		}
		_, _ = io.WriteString(destination, chunk[:write])
		count -= write
	}
}

func TestKubectlTransportHelper(t *testing.T) {
	if os.Getenv("GO_WANT_S1C_KUBECTL_HELPER") != "1" {
		return
	}
	stdoutSize, _ := strconv.Atoi(os.Getenv("S1C_KUBECTL_HELPER_STDOUT"))
	stderrSize, _ := strconv.Atoi(os.Getenv("S1C_KUBECTL_HELPER_STDERR"))
	exitCode, _ := strconv.Atoi(os.Getenv("S1C_KUBECTL_HELPER_EXIT"))
	writeKubectlHelperBytes(os.Stdout, stdoutSize)
	writeKubectlHelperBytes(os.Stderr, stderrSize)
	os.Exit(exitCode)
}

func runKubectlTransportHelper(t *testing.T, stdoutSize, stderrSize, exitCode int) (string, error) {
	t.Helper()
	t.Setenv("GO_WANT_S1C_KUBECTL_HELPER", "1")
	t.Setenv("S1C_KUBECTL_HELPER_STDOUT", strconv.Itoa(stdoutSize))
	t.Setenv("S1C_KUBECTL_HELPER_STDERR", strconv.Itoa(stderrSize))
	t.Setenv("S1C_KUBECTL_HELPER_EXIT", strconv.Itoa(exitCode))
	h := New(Config{KubectlBin: os.Args[0]})
	return h.kubectl(context.Background(), "-test.run=^TestKubectlTransportHelper$", "--")
}

func TestKubectlRejectsBoundOverflowWithoutPartialOutput(t *testing.T) {
	tests := []struct {
		name                 string
		stdout, stderr, exit int
		want                 string
	}{
		{"stdout overflow", int(maxKubectlStdoutSize) + 1, 0, 0, fmt.Sprintf("stdout exceeds %d bytes", maxKubectlStdoutSize)},
		{"stderr overflow", 0, int(maxKubectlStderrSize) + 1, 0, fmt.Sprintf("stderr exceeds %d bytes", maxKubectlStderrSize)},
		{"failed command stdout", 32, 4, 7, "exit status 7"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := runKubectlTransportHelper(t, test.stdout, test.stderr, test.exit)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("kubectl error = %v, want substring %q", err, test.want)
			}
			if output != "" {
				t.Fatalf("kubectl returned %d partial bytes on rejection", len(output))
			}
		})
	}
}
