package internal

import (
	"errors"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// CIClassification is the machine-readable result emitted by `loom ci classify`.
type CIClassification struct {
	Class     string   `json:"class"`
	Retryable bool     `json:"retryable"`
	FreeRetry bool     `json:"free_retry"`
	Terminal  bool     `json:"terminal"`
	Summary   string   `json:"summary"`
	Bytes     int      `json:"bytes"`
	Lines     int      `json:"lines"`
	Evidence  []string `json:"evidence,omitempty"`
}

// ClassifyCILog applies Mills' existing failure taxonomy to a CI log. Unknown
// logs intentionally classify as code so callers fail closed.
func ClassifyCILog(data []byte) CIClassification {
	text := string(data)
	rec := pipeline.ClassifyFailureRecord(errors.New(text))
	out := CIClassification{
		Class:     string(rec.Class),
		Retryable: rec.Retryable,
		FreeRetry: rec.FreeRetry,
		Terminal:  rec.Terminal,
		Summary:   summaryForClass(rec.Class),
		Bytes:     len(data),
		Lines:     countLines(text),
		Evidence:  evidenceLines(text, 5),
	}
	if strings.TrimSpace(text) == "" {
		out.Summary = "empty log; classified as code by fail-closed default"
	}
	return out
}

func summaryForClass(class pipeline.FailureClass) string {
	switch class {
	case pipeline.FailureTransient:
		return "transient infrastructure or transport failure; retry is likely useful"
	case pipeline.FailureTransientQuota:
		return "quota or capacity failure; retry with backoff"
	case pipeline.FailureInfrastructure:
		return "persistent infrastructure failure; operator or platform fix likely needed"
	case pipeline.FailureConfiguration:
		return "terminal configuration failure; retrying the same job is unlikely to help"
	case pipeline.FailureCode:
		return "code or test failure; inspect the job output and fix the change"
	default:
		return "unknown failure; classified as code by fail-closed default"
	}
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	lines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		lines++
	}
	return lines
}

func evidenceLines(s string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	needles := []string{
		"error", "fail", "failed", "failure", "fatal", "panic", "timeout", "deadline",
		"too many requests", "rate limit", "quota", "429", "500", "502", "503", "504",
		"bad gateway", "service unavailable", "gateway timeout", "method not allowed",
		"status 405", "buildah", "sandbox", "forbidden", "already exists",
		"broken pipe", "connection refused", "connection reset", "unexpected eof",
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				out = append(out, trimmed)
				break
			}
		}
		if len(out) >= limit {
			return out
		}
	}
	return out
}
