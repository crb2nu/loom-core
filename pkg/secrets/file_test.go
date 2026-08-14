package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// mockMasterKeyStore records keychain interactions for master-key tests.
type mockMasterKeyStore struct {
	getCalls int
	setCalls int
	stored   map[string]string
	setErr   error
}

func (m *mockMasterKeyStore) Get(key string) (string, error) {
	m.getCalls++
	return m.stored[key], nil
}

func (m *mockMasterKeyStore) Set(key, value string) error {
	m.setCalls++
	if m.setErr != nil {
		return m.setErr
	}
	if m.stored == nil {
		m.stored = make(map[string]string)
	}
	m.stored[key] = value
	return nil
}

// withMasterKeyStore swaps the master-key keychain factory for the test.
func withMasterKeyStore(t *testing.T, fn func() (masterKeyStore, error)) {
	t.Helper()
	orig := newMasterKeyStore
	newMasterKeyStore = fn
	t.Cleanup(func() { newMasterKeyStore = orig })
}

// withMachineID pins the machine identifier for deterministic derivation.
func withMachineID(t *testing.T, id string) {
	t.Helper()
	orig := machineIDFn
	machineIDFn = func() (string, error) { return id, nil }
	t.Cleanup(func() { machineIDFn = orig })
}

func TestFileBackend_ConstructionNoKeychainInteraction(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")

	store := &mockMasterKeyStore{}
	withMasterKeyStore(t, func() (masterKeyStore, error) { return store, nil })

	path := filepath.Join(t.TempDir(), "secrets.enc")
	if _, err := NewFileBackend(path); err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}

	if store.getCalls != 0 || store.setCalls != 0 {
		t.Errorf("construction touched keychain: %d gets, %d sets, want 0/0",
			store.getCalls, store.setCalls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("construction created secrets file, want no file")
	}
}

func TestFileBackend_ReadMissingFileNoKeychainInteraction(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")

	store := &mockMasterKeyStore{}
	withMasterKeyStore(t, func() (masterKeyStore, error) { return store, nil })

	backend, err := NewFileBackend(filepath.Join(t.TempDir(), "secrets.enc"))
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}

	value, err := backend.Get("anything")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if value != "" {
		t.Errorf("Get() = %q, want empty", value)
	}
	if store.getCalls != 0 || store.setCalls != 0 {
		t.Errorf("read of missing file touched keychain: %d gets, %d sets, want 0/0",
			store.getCalls, store.setCalls)
	}
}

func TestFileBackend_KeychainSetFails_StableKeyAcrossRestarts(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")
	withMachineID(t, "test-machine-id")

	store := &mockMasterKeyStore{setErr: fmt.Errorf("no default keychain")}
	withMasterKeyStore(t, func() (masterKeyStore, error) { return store, nil })

	path := filepath.Join(t.TempDir(), "secrets.enc")

	backend1, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}
	if err := backend1.Set("api_token", "hunter2"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Simulate a daemon restart: fresh backend, same failing keychain.
	backend2, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() after restart error = %v", err)
	}
	value, err := backend2.Get("api_token")
	if err != nil {
		t.Fatalf("Get() after restart error = %v (secrets file undecryptable — ephemeral key was used)", err)
	}
	if value != "hunter2" {
		t.Errorf("Get() after restart = %q, want hunter2", value)
	}
}

func TestFileBackend_KeychainUnavailable_StableKeyAcrossRestarts(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")
	withMachineID(t, "test-machine-id")
	withMasterKeyStore(t, func() (masterKeyStore, error) {
		return nil, fmt.Errorf("keychain backend only available on macOS")
	})

	path := filepath.Join(t.TempDir(), "secrets.enc")

	backend1, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}
	if err := backend1.Set("api_token", "hunter2"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	backend2, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() after restart error = %v", err)
	}
	value, err := backend2.Get("api_token")
	if err != nil {
		t.Fatalf("Get() after restart error = %v", err)
	}
	if value != "hunter2" {
		t.Errorf("Get() after restart = %q, want hunter2", value)
	}
}

func TestFileBackend_KeychainMint_PersistsAndRoundTrips(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")

	store := &mockMasterKeyStore{}
	withMasterKeyStore(t, func() (masterKeyStore, error) { return store, nil })

	path := filepath.Join(t.TempDir(), "secrets.enc")

	backend1, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}
	if err := backend1.Set("api_token", "hunter2"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if store.stored[masterKeyAccount] == "" {
		t.Fatal("first write did not persist a master key in the keychain")
	}

	// Restart: the persisted keychain key must decrypt the file.
	backend2, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() after restart error = %v", err)
	}
	value, err := backend2.Get("api_token")
	if err != nil {
		t.Fatalf("Get() after restart error = %v", err)
	}
	if value != "hunter2" {
		t.Errorf("Get() after restart = %q, want hunter2", value)
	}
}

func TestFileBackend_ExistingKeychainKeyPreferred(t *testing.T) {
	t.Setenv("LOOM_MASTER_KEY", "")

	store := &mockMasterKeyStore{stored: map[string]string{masterKeyAccount: "pre-existing-key"}}
	withMasterKeyStore(t, func() (masterKeyStore, error) { return store, nil })

	path := filepath.Join(t.TempDir(), "secrets.enc")

	backend1, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() error = %v", err)
	}
	if err := backend1.Set("api_token", "hunter2"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if store.setCalls != 0 {
		t.Errorf("write with existing keychain key minted a new one: %d sets, want 0", store.setCalls)
	}

	backend2, err := NewFileBackend(path)
	if err != nil {
		t.Fatalf("NewFileBackend() after restart error = %v", err)
	}
	value, err := backend2.Get("api_token")
	if err != nil {
		t.Fatalf("Get() after restart error = %v", err)
	}
	if value != "hunter2" {
		t.Errorf("Get() after restart = %q, want hunter2", value)
	}
}

func TestMachineID_Stable(t *testing.T) {
	id1, err := machineID()
	if err != nil {
		t.Fatalf("machineID() error = %v", err)
	}
	id2, err := machineID()
	if err != nil {
		t.Fatalf("machineID() second call error = %v", err)
	}
	if id1 == "" {
		t.Error("machineID() returned empty identifier")
	}
	if id1 != id2 {
		t.Errorf("machineID() not stable: %q != %q", id1, id2)
	}
}

func TestParseIOPlatformUUID(t *testing.T) {
	out := `  "IOPlatformSerialNumber" = "C02ABC123"
  "IOPlatformUUID" = "12345678-ABCD-EF01-2345-6789ABCDEF01"
`
	if got := parseIOPlatformUUID(out); got != "12345678-ABCD-EF01-2345-6789ABCDEF01" {
		t.Errorf("parseIOPlatformUUID() = %q", got)
	}
	if got := parseIOPlatformUUID("no uuid here"); got != "" {
		t.Errorf("parseIOPlatformUUID(no match) = %q, want empty", got)
	}
}
