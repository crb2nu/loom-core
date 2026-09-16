package hud

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestValidateConfig_WebhookSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled with no secrets", Config{}, false},
		{"enabled with no secrets refuses", Config{WebhookInboundEnabled: true}, true},
		{"enabled with gitlab secret only", Config{WebhookInboundEnabled: true, WebhookGitLabSecret: "s"}, false},
		{"enabled with github secret only", Config{WebhookInboundEnabled: true, WebhookGitHubSecret: "s"}, false},
		{"enabled with both secrets", Config{WebhookInboundEnabled: true, WebhookGitLabSecret: "a", WebhookGitHubSecret: "b"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestNewApp_RefusesInboundWebhooksWithoutSecret proves the validation is
// wired into the constructor every boot path shares (embedded daemon,
// standalone `loom hud`, and Run) — a secretless-but-enabled deployment
// must fail at boot, not silently 401 every webhook forever.
func TestNewApp_RefusesInboundWebhooksWithoutSecret(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := NewApp(Config{WebhookInboundEnabled: true}, nil, logger)
	if err == nil {
		t.Fatal("expected NewApp error for inbound webhooks with no secret")
	}
	if app != nil {
		t.Fatal("expected nil app on validation failure")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_GITLAB_SECRET") {
		t.Errorf("error should name the env vars to set, got: %v", err)
	}
}
