package daemon

import (
	"testing"
	"time"
)

func TestGetResidentServers(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("LOOM_RESIDENT_SERVERS", "")
		got := (&ResourceConfig{}).GetResidentServers()
		if !got["agent_context"] || !got["codebase_memory"] {
			t.Fatalf("defaults should include agent_context + codebase_memory, got %v", got)
		}
		if len(got) != 2 {
			t.Fatalf("defaults should be exactly the two stateful servers, got %v", got)
		}
	})

	t.Run("config override replaces defaults", func(t *testing.T) {
		t.Setenv("LOOM_RESIDENT_SERVERS", "")
		got := (&ResourceConfig{ResidentServers: []string{"foo", "bar"}}).GetResidentServers()
		if !got["foo"] || !got["bar"] || got["agent_context"] {
			t.Fatalf("config override should replace defaults, got %v", got)
		}
	})

	t.Run("env overrides config", func(t *testing.T) {
		t.Setenv("LOOM_RESIDENT_SERVERS", "baz, qux")
		got := (&ResourceConfig{ResidentServers: []string{"foo"}}).GetResidentServers()
		if !got["baz"] || !got["qux"] || got["foo"] {
			t.Fatalf("env should override config, got %v", got)
		}
		if len(got) != 2 {
			t.Fatalf("whitespace around env entries should be trimmed, got %v", got)
		}
	})

	t.Run("none disables the exemption", func(t *testing.T) {
		t.Setenv("LOOM_RESIDENT_SERVERS", "none")
		got := (&ResourceConfig{ResidentServers: []string{"foo"}}).GetResidentServers()
		if len(got) != 0 {
			t.Fatalf("\"none\" should disable exemptions, got %v", got)
		}
	})
}

func TestShouldReapIdleServer(t *testing.T) {
	resident := map[string]bool{"agent_context": true, "codebase_memory": true}
	minute := time.Minute

	cases := []struct {
		name    string
		server  string
		idle    time.Duration
		timeout time.Duration
		want    bool
	}{
		{"resident never reaped even when very idle", "agent_context", 60 * time.Minute, minute, false},
		{"non-resident past timeout is reaped", "gitlab", 10 * time.Minute, minute, true},
		{"non-resident under timeout is kept", "gitlab", 30 * time.Second, minute, false},
		{"non-resident exactly at timeout is kept", "gitlab", minute, minute, false},
		{"second resident never reaped", "codebase_memory", 60 * time.Minute, minute, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldReapIdleServer(tc.server, tc.idle, tc.timeout, resident); got != tc.want {
				t.Fatalf("shouldReapIdleServer(%q, %v, %v) = %v, want %v", tc.server, tc.idle, tc.timeout, got, tc.want)
			}
		})
	}
}
