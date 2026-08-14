package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/pbkdf2"
)

// masterKeyAccount is the keychain account under which the master key is stored.
const masterKeyAccount = "_loom_master_key"

// masterKeyStore is the minimal keychain surface used for master-key persistence.
type masterKeyStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

// newMasterKeyStore returns the keychain used for master-key persistence.
// Package variable so tests can substitute a mock and assert (non-)interaction.
var newMasterKeyStore = func() (masterKeyStore, error) {
	return NewKeychainBackend()
}

// machineIDFn returns a stable machine identifier; overridable in tests.
var machineIDFn = machineID

// FileBackend stores secrets in an encrypted file.
// Uses AES-256-GCM for encryption.
type FileBackend struct {
	path   string
	mu     sync.RWMutex
	cache  map[string]string // In-memory cache of decrypted secrets
	loaded bool

	keyMu sync.Mutex
	key   []byte // Derived encryption key, resolved lazily on first read/write
}

// defaultFilePath returns the default secrets file path.
func defaultFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "loom", "secrets.enc")
}

// NewFileBackend creates a new encrypted file backend.
// If path is empty, uses ~/.config/loom/secrets.enc
// The encryption key is resolved lazily on first read of an existing secrets
// file or first write; construction never touches the keychain. Key priority:
// LOOM_MASTER_KEY env var, macOS Keychain, machine-ID derivation.
func NewFileBackend(path string) (*FileBackend, error) {
	if path == "" {
		path = defaultFilePath()
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}

	return &FileBackend{
		path:  path,
		cache: make(map[string]string),
	}, nil
}

// NewFileBackendContext creates an encrypted file backend. Construction never
// touches the keychain (the master key resolves lazily on first read/write),
// so ctx only gates construction itself; it is kept for callers that bound
// backend discovery with a deadline.
func NewFileBackendContext(ctx context.Context, path string) (*FileBackend, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewFileBackend(path)
}

// encryptionKey returns the derived encryption key, resolving it on first use.
// forWrite permits minting and persisting a new master key when none exists.
func (b *FileBackend) encryptionKey(forWrite bool) ([]byte, error) {
	b.keyMu.Lock()
	defer b.keyMu.Unlock()

	if b.key != nil {
		return b.key, nil
	}

	key, err := resolveMasterKey(forWrite)
	if err != nil {
		return nil, fmt.Errorf("get master key: %w", err)
	}
	b.key = key
	return key, nil
}

// resolveMasterKey retrieves or derives the master encryption key.
// Priority:
//  1. LOOM_MASTER_KEY environment variable (for CI/CD)
//  2. Key stored in macOS Keychain
//  3. On write with no existing key: mint a random key and persist it in the
//     keychain; if persisting fails, fall back to machine-ID derivation so the
//     key is stable across restarts (an unpersisted random key would make the
//     secrets file undecryptable after restart).
//  4. Derive from machine ID (stable fallback, no keychain required)
func resolveMasterKey(forWrite bool) ([]byte, error) {
	// Check environment variable first
	if envKey := os.Getenv("LOOM_MASTER_KEY"); envKey != "" {
		return deriveKey(envKey), nil
	}

	// Try to get from keychain (macOS)
	if kb, err := newMasterKeyStore(); err == nil {
		if key, err := kb.Get(masterKeyAccount); err == nil && key != "" {
			return deriveKey(key), nil
		}
	}

	if forWrite {
		key, err := mintKeychainMasterKey()
		if err == nil {
			return key, nil
		}
		slog.Warn("secrets: keychain unavailable for master key storage, falling back to machine-ID derived key", "error", err)
	}

	return deriveMachineKey()
}

// mintKeychainMasterKey generates a random master key and persists it in the
// keychain. Returns an error if the key cannot be persisted: an ephemeral
// random key must never be used to encrypt the secrets file.
func mintKeychainMasterKey() ([]byte, error) {
	kb, err := newMasterKeyStore()
	if err != nil {
		return nil, fmt.Errorf("open keychain: %w", err)
	}

	key, err := generateMasterKey()
	if err != nil {
		return nil, err
	}

	if err := kb.Set(masterKeyAccount, key); err != nil {
		return nil, fmt.Errorf("store master key: %w", err)
	}

	return deriveKey(key), nil
}

// generateMasterKey generates a random master key.
func generateMasterKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", bytes), nil
}

// deriveKey derives a 32-byte AES key from a passphrase.
func deriveKey(passphrase string) []byte {
	salt := []byte("loom-secrets-v1")
	return pbkdf2.Key([]byte(passphrase), salt, 100000, 32, sha256.New)
}

// deriveMachineKey derives the master key from a stable machine identifier.
func deriveMachineKey() ([]byte, error) {
	id, err := machineIDFn()
	if err != nil {
		return nil, fmt.Errorf("derive machine key: %w", err)
	}
	return deriveKey("loom-machine:" + id), nil
}

// machineID returns a stable identifier for this machine.
// macOS: IOPlatformUUID; Linux: /etc/machine-id; fallback: hostname.
func machineID() (string, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(context.Background(), "ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			if id := parseIOPlatformUUID(string(out)); id != "" {
				return id, nil
			}
		}
	}

	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id, nil
			}
		}
	}

	// Last resort: hostname (stable per machine, weaker entropy).
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "", fmt.Errorf("no stable machine identifier available")
	}
	return host, nil
}

// parseIOPlatformUUID extracts the IOPlatformUUID value from ioreg output.
func parseIOPlatformUUID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		return strings.Trim(strings.TrimSpace(parts[1]), `"`)
	}
	return ""
}

// load reads and decrypts the secrets file.
func (b *FileBackend) load() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.loaded {
		return nil
	}

	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		b.cache = make(map[string]string)
		b.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read secrets file: %w", err)
	}

	// Decrypt
	plaintext, err := b.decrypt(data)
	if err != nil {
		return fmt.Errorf("decrypt secrets: %w", err)
	}

	// Parse JSON
	if err := json.Unmarshal(plaintext, &b.cache); err != nil {
		return fmt.Errorf("parse secrets: %w", err)
	}

	b.loaded = true
	return nil
}

// save encrypts and writes the secrets file.
func (b *FileBackend) save() error {
	// Marshal to JSON
	plaintext, err := json.MarshalIndent(b.cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal secrets: %w", err)
	}

	// Encrypt
	ciphertext, err := b.encrypt(plaintext)
	if err != nil {
		return fmt.Errorf("encrypt secrets: %w", err)
	}

	// Write atomically
	tmpPath := b.path + ".tmp"
	if err := os.WriteFile(tmpPath, ciphertext, 0600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}

	if err := os.Rename(tmpPath, b.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename secrets: %w", err)
	}

	return nil
}

// encrypt encrypts data using AES-256-GCM.
// Resolves the master key with write semantics: a new key may be minted and
// persisted if none exists yet.
func (b *FileBackend) encrypt(plaintext []byte) ([]byte, error) {
	key, err := b.encryptionKey(true)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt decrypts data using AES-256-GCM.
// Resolves the master key with read semantics: never mints or stores keys.
func (b *FileBackend) decrypt(ciphertext []byte) ([]byte, error) {
	key, err := b.encryptionKey(false)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce := ciphertext[:gcm.NonceSize()]
	ciphertext = ciphertext[gcm.NonceSize():]

	return gcm.Open(nil, nonce, ciphertext, nil)
}

// Get retrieves a secret from the encrypted file.
func (b *FileBackend) Get(key string) (string, error) {
	if err := b.load(); err != nil {
		return "", err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.cache[key], nil
}

// Set stores a secret in the encrypted file.
func (b *FileBackend) Set(key, value string) error {
	if err := b.load(); err != nil {
		return err
	}

	b.mu.Lock()
	b.cache[key] = value
	b.mu.Unlock()

	return b.save()
}

// Delete removes a secret from the encrypted file.
func (b *FileBackend) Delete(key string) error {
	if err := b.load(); err != nil {
		return err
	}

	b.mu.Lock()
	delete(b.cache, key)
	b.mu.Unlock()

	return b.save()
}

// List returns all secret keys in the encrypted file.
func (b *FileBackend) List() ([]string, error) {
	if err := b.load(); err != nil {
		return nil, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	keys := make([]string, 0, len(b.cache))
	for k := range b.cache {
		keys = append(keys, k)
	}
	return keys, nil
}

// Name returns the backend name.
func (b *FileBackend) Name() string {
	return "file"
}

// ReadOnly returns false since file backend supports writes.
func (b *FileBackend) ReadOnly() bool {
	return false
}
