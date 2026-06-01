package daemon

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvBoolTrue(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"true", true}, {"TRUE", true}, {"True", true},
		{"1", true},
		{"yes", true}, {"YES", true},
		{"on", true},
		{"  true  ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"enabled", false}, // explicit allowlist — not a fuzzy match
		{"1.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := envBoolTrue(tt.in); got != tt.want {
				t.Errorf("envBoolTrue(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstNonEmpty_ReturnsFirst(t *testing.T) {
	got := firstNonEmpty("hello", "world")
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestFirstNonEmpty_SkipsEmpty(t *testing.T) {
	got := firstNonEmpty("", "second")
	if got != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}
}

func TestFirstNonEmpty_SkipsWhitespace(t *testing.T) {
	got := firstNonEmpty("  ", "\t", "real")
	if got != "real" {
		t.Errorf("got %q, want %q", got, "real")
	}
}

func TestFirstNonEmpty_AllEmpty(t *testing.T) {
	got := firstNonEmpty("", "  ", "\t")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestFirstNonEmpty_NoArgs(t *testing.T) {
	got := firstNonEmpty()
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestFirstNonEmpty_SingleValue(t *testing.T) {
	got := firstNonEmpty("only")
	if got != "only" {
		t.Errorf("got %q, want %q", got, "only")
	}
}

func TestFirstPositiveInt(t *testing.T) {
	if got := firstPositiveInt(2, "3"); got != 2 {
		t.Fatalf("configured value should win, got %d", got)
	}
	if got := firstPositiveInt(0, "3"); got != 3 {
		t.Fatalf("env value not parsed, got %d", got)
	}
	if got := firstPositiveInt(0, "-1"); got != 0 {
		t.Fatalf("non-positive env value should be ignored, got %d", got)
	}
}

func TestFirstPositiveFloat(t *testing.T) {
	if got := firstPositiveFloat(0.5, "1.5"); got != 0.5 {
		t.Fatalf("configured value should win, got %f", got)
	}
	if got := firstPositiveFloat(0, "0.75"); got != 0.75 {
		t.Fatalf("env value not parsed, got %f", got)
	}
	if got := firstPositiveFloat(0, "nope"); got != 0 {
		t.Fatalf("invalid env value should be ignored, got %f", got)
	}
}

// TestBuildEmbeddedHUDConfig_HarvesterFromEnv pins the A1 fix: the prod
// daemon path (loomd → startEmbeddedHUD → buildEmbeddedHUDConfig) must read
// the SPAWN_HARVESTER_* env vars into hud.Config so initSpawnOrchestrator
// can register the harvester-vm substrate. Before this, the daemon dropped
// every harvester field and substrate-targeted spawns silently fell back to
// k8s regardless of deployment env.
func TestBuildEmbeddedHUDConfig_HarvesterFromEnv(t *testing.T) {
	t.Setenv("SPAWN_HARVESTER_KUBECONFIG", "/etc/harvester/kubeconfig")
	t.Setenv("SPAWN_HARVESTER_BASE_IMAGE", "mills-devbox-base-2026-06-01")
	t.Setenv("SPAWN_HARVESTER_NAMESPACE", "default")
	t.Setenv("SPAWN_HARVESTER_STORAGE_CLASS", "longhorn-image-abc123")
	t.Setenv("SPAWN_HARVESTER_NAD", "default/lan10g")
	t.Setenv("SPAWN_HARVESTER_DEFAULT_VCPUS", "2")
	t.Setenv("SPAWN_HARVESTER_DEFAULT_MEM_MI", "4096")
	t.Setenv("SPAWN_HARVESTER_DEFAULT_DISK_GI", "20")

	got := buildEmbeddedHUDConfig(EmbeddedHUDConfig{}, "")

	if got.SpawnHarvesterKubeconfig != "/etc/harvester/kubeconfig" {
		t.Errorf("Kubeconfig = %q, want /etc/harvester/kubeconfig", got.SpawnHarvesterKubeconfig)
	}
	if got.SpawnHarvesterBaseImage != "mills-devbox-base-2026-06-01" {
		t.Errorf("BaseImage = %q", got.SpawnHarvesterBaseImage)
	}
	if got.SpawnHarvesterNamespace != "default" {
		t.Errorf("Namespace = %q", got.SpawnHarvesterNamespace)
	}
	if got.SpawnHarvesterStorageClass != "longhorn-image-abc123" {
		t.Errorf("StorageClass = %q", got.SpawnHarvesterStorageClass)
	}
	if got.SpawnHarvesterNetworkAttachDef != "default/lan10g" {
		t.Errorf("NAD = %q", got.SpawnHarvesterNetworkAttachDef)
	}
	if got.SpawnHarvesterDefaultVCPUs != 2 || got.SpawnHarvesterDefaultMemMi != 4096 || got.SpawnHarvesterDefaultDiskGi != 20 {
		t.Errorf("resource defaults = %d/%d/%d, want 2/4096/20",
			got.SpawnHarvesterDefaultVCPUs, got.SpawnHarvesterDefaultMemMi, got.SpawnHarvesterDefaultDiskGi)
	}
	// SSHUser intentionally unset → empty, so the backend applies its
	// "agent" default for home-parity. We must NOT inject "ubuntu" here.
	if got.SpawnHarvesterSSHUser != "" {
		t.Errorf("SSHUser = %q, want empty (backend defaults to agent)", got.SpawnHarvesterSSHUser)
	}
}

// TestBuildEmbeddedHUDConfig_HarvesterUnsetLeavesEmpty confirms the safe
// default: with no env and no file config, SpawnHarvesterKubeconfig is empty,
// which leaves the substrate unregistered (k8s-only) — the current prod
// behavior, so this change is inert until the deployment opts in.
func TestBuildEmbeddedHUDConfig_HarvesterUnsetLeavesEmpty(t *testing.T) {
	for _, k := range []string{
		"SPAWN_HARVESTER_KUBECONFIG", "SPAWN_HARVESTER_BASE_IMAGE",
		"SPAWN_HARVESTER_STORAGE_CLASS", "SPAWN_HARVESTER_NAD",
	} {
		t.Setenv(k, "")
	}
	got := buildEmbeddedHUDConfig(EmbeddedHUDConfig{}, "")
	if got.SpawnHarvesterKubeconfig != "" {
		t.Errorf("Kubeconfig = %q, want empty (substrate stays unregistered)", got.SpawnHarvesterKubeconfig)
	}
}

// TestBuildEmbeddedHUDConfig_HarvesterFileConfigWins confirms file config
// takes precedence over env, matching every other spawn field's contract.
func TestBuildEmbeddedHUDConfig_HarvesterFileConfigWins(t *testing.T) {
	t.Setenv("SPAWN_HARVESTER_KUBECONFIG", "/from/env")
	got := buildEmbeddedHUDConfig(EmbeddedHUDConfig{
		SpawnHarvesterKubeconfig: "/from/file",
	}, "")
	if got.SpawnHarvesterKubeconfig != "/from/file" {
		t.Errorf("Kubeconfig = %q, want /from/file (file config wins)", got.SpawnHarvesterKubeconfig)
	}
}

func TestWriteEmbeddedHUDPortFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	writeEmbeddedHUDPortFile(logger, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4312})

	portFile := filepath.Join(tmpDir, "loom", "hud.port")
	data, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatalf("expected embedded HUD port file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "4312" {
		t.Fatalf("port file contents = %q, want %q", got, "4312")
	}
}
