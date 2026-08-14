package main

import (
	"regexp"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

const redactedValue = "[REDACTED]"

var sensitiveTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)glpat-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)(PRIVATE-TOKEN\s*[:=]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(Authorization\s*[:=]\s*Bearer\s+)[^\s,;]+`),
}

// sanitizeGitLabResponse recursively removes credential-bearing fields before
// GitLab API data reaches MCP results, caches, audit logs, or callers.
func sanitizeGitLabResponse(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveGitLabField(key) {
				typed[key] = redactedValue
				continue
			}
			typed[key] = sanitizeGitLabResponse(child)
		}
	case []any:
		for i, child := range typed {
			typed[i] = sanitizeGitLabResponse(child)
		}
	}
	return value
}

func isSensitiveGitLabField(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "import_url" {
		return true
	}
	return normalized == "token" ||
		normalized == "password" ||
		normalized == "secret" ||
		strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_password") ||
		strings.HasSuffix(normalized, "_secret")
}

func sanitizeGitLabErrorText(text, configuredToken string) string {
	if configuredToken != "" {
		text = strings.ReplaceAll(text, configuredToken, redactedValue)
	}
	for _, pattern := range sensitiveTextPatterns {
		text = pattern.ReplaceAllString(text, "${1}"+redactedValue)
	}
	return text
}

func gitlabJSONResult(value any) (*mcp.CallToolResult, error) {
	return mcp.JSONResult(sanitizeGitLabResponse(value))
}
