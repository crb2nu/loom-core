package agentcontext

import "testing"

// TestLoadConfigFromEnv_SessionReaperMaxAgeDefault guards the !341 port: the
// ended/summarized session retention default dropped 168h → 72h. Cluster
// median session age is ~3 days, so 168h kept ~700 stale points warm for no
// listener. The env override still wins.
func TestLoadConfigFromEnv_SessionReaperMaxAgeDefault(t *testing.T) {
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.SessionReaperMaxAge != 72 {
		t.Fatalf("SessionReaperMaxAge default = %d, want 72", cfg.SessionReaperMaxAge)
	}
}

// TestLoadConfigFromEnv_SessionReaperMaxAgeOverride confirms the env var still
// overrides the lowered default.
func TestLoadConfigFromEnv_SessionReaperMaxAgeOverride(t *testing.T) {
	t.Setenv("AGENT_CONTEXT_SESSION_REAPER_MAX_AGE_HOURS", "240")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.SessionReaperMaxAge != 240 {
		t.Fatalf("SessionReaperMaxAge override = %d, want 240", cfg.SessionReaperMaxAge)
	}
}
