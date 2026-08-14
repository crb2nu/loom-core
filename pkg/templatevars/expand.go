// Package templatevars provides shared template variable expansion for MCP configs.
// It resolves ${env:VAR}, ${env:VAR:-default}, ${keychain:VAR}, and ${secret:VAR}
// patterns using the registry's env alias fallbacks and the secrets manager.
//
// This logic was extracted from internal/daemon/daemon.go to allow reuse during
// config generation (for platforms like Codex that lack runtime resolvers).
package templatevars

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/crb2nu/loom/pkg/registry"
	"github.com/crb2nu/loom/pkg/secrets"
)

func looksLikeSecretKey(name string) bool {
	// Keep this intentionally small and suffix-based; it is used as a fallback
	// signal when an ${env:...} reference is actually intended to be a secret.
	//
	// This improves robustness for GUI-launched processes (launchd, VS Code, etc.)
	// where shell-exported env vars are often missing, but secrets may exist in
	// Keychain or loom's encrypted file backend.
	secretSuffixes := []string{
		"_TOKEN", "_KEY", "_SECRET", "_PASSWORD", "_PAT",
		"_API_KEY", "_API_TOKEN", "_ACCESS_TOKEN",
	}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// Expander resolves template variable patterns in strings.
type Expander struct {
	registry   *registry.Registry
	secretsMu  sync.Mutex
	secretsMgr *secrets.Manager
	lazy       bool // if true, defer secrets.DefaultManager() to first use
}

// Option configures an Expander.
type Option func(*Expander)

// WithRegistry provides a registry for env alias fallback resolution.
func WithRegistry(reg *registry.Registry) Option {
	return func(e *Expander) {
		e.registry = reg
	}
}

// WithSecretsManager provides an explicit secrets manager.
func WithSecretsManager(mgr *secrets.Manager) Option {
	return func(e *Expander) {
		e.secretsMgr = mgr
	}
}

// WithLazySecrets defers secrets.DefaultManager() initialization until first use.
func WithLazySecrets() Option {
	return func(e *Expander) {
		e.lazy = true
	}
}

// New creates an Expander with the given options.
// It does NOT resolve ${repo} or ${HOME} — those are handled by generator.ResolveTokens.
func New(opts ...Option) *Expander {
	e := &Expander{}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// resolveEnv resolves an environment variable with registry alias fallbacks.
func (e *Expander) resolveEnvContext(ctx context.Context, name string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return ""
	}
	if e.registry != nil {
		val := e.registry.GetEnvWithFallback(name)
		if val != "" {
			return val
		}
		// If this looks like a secret and the process env is missing it,
		// fall back to the secrets manager (env backend -> keychain -> file).
		if looksLikeSecretKey(name) {
			return e.resolveSecretContext(ctx, name)
		}
		return ""
	}
	val := os.Getenv(name)
	if val != "" {
		return val
	}
	if looksLikeSecretKey(name) {
		return e.resolveSecretContext(ctx, name)
	}
	return ""
}

// resolveSecret resolves a secret using the secrets manager.
func (e *Expander) resolveSecretContext(ctx context.Context, key string) string {
	mgr := e.getSecretsManagerContext(ctx)
	if mgr == nil {
		return ""
	}
	val, err := mgr.GetValueContext(ctx, key)
	if err != nil {
		if ctx == nil || ctx.Err() == nil {
			slog.Debug("failed to resolve secret", "key", key, "error", err)
		}
		return ""
	}
	if val == "" {
		slog.Debug("secret not found", "key", key)
	} else {
		slog.Debug("secret resolved", "key", key, "length", len(val))
	}
	return val
}

// getSecretsManager returns the secrets manager, lazily initializing if configured.
func (e *Expander) getSecretsManagerContext(ctx context.Context) *secrets.Manager {
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	if e.secretsMgr != nil {
		return e.secretsMgr
	}
	if !e.lazy {
		return nil
	}
	mgr, err := secrets.DefaultManagerContext(ctx)
	if err != nil {
		slog.Debug("failed to initialize secrets manager", "error", err)
		return nil
	}
	// Cache only a successful initialization. In particular, a canceled first
	// caller must not permanently poison this Expander for later refreshes.
	e.secretsMgr = mgr
	return e.secretsMgr
}

// Expand resolves ${env:VAR}, ${env:VAR:-default}, ${keychain:VAR}, and
// ${secret:VAR} patterns in s. It does NOT touch ${repo} or ${HOME}.
func (e *Expander) Expand(s string) string {
	return e.ExpandContext(context.Background(), s)
}

// ExpandContext resolves template patterns while allowing subprocess-backed
// secret stores to be canceled.
func (e *Expander) ExpandContext(ctx context.Context, s string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	// Expand ${env:VAR} and ${env:VAR:-default} patterns
	s = e.expandEnvContext(ctx, s)

	// Expand ${keychain:VAR} patterns
	s = e.expandKeychainContext(ctx, s)

	// Expand ${secret:VAR} patterns
	s = e.expandSecretContext(ctx, s)

	return s
}

func (e *Expander) expandEnvContext(ctx context.Context, s string) string {
	for ctx.Err() == nil {
		start := strings.Index(s, "${env:")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], "}")
		if end == -1 {
			break
		}
		end += start
		varExpr := s[start+len("${env:") : end]

		var varName, defaultVal string
		if idx := strings.Index(varExpr, ":-"); idx != -1 {
			varName = varExpr[:idx]
			defaultVal = varExpr[idx+2:]
		} else {
			varName = varExpr
		}

		value := e.resolveEnvContext(ctx, varName)
		if value == "" {
			value = defaultVal
		}
		s = s[:start] + value + s[end+1:]
	}
	return s
}

func (e *Expander) expandKeychainContext(ctx context.Context, s string) string {
	for ctx.Err() == nil {
		start := strings.Index(s, "${keychain:")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], "}")
		if end == -1 {
			break
		}
		end += start
		varName := s[start+len("${keychain:") : end]

		// Try secrets manager first, fall back to env
		value := e.resolveSecretContext(ctx, varName)
		if value == "" {
			value = e.resolveEnvContext(ctx, varName)
		}
		s = s[:start] + value + s[end+1:]
	}
	return s
}

func (e *Expander) expandSecretContext(ctx context.Context, s string) string {
	for ctx.Err() == nil {
		start := strings.Index(s, "${secret:")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], "}")
		if end == -1 {
			break
		}
		end += start
		varName := s[start+len("${secret:") : end]

		value := e.resolveSecretContext(ctx, varName)
		s = s[:start] + value + s[end+1:]
	}
	return s
}

// ExpandMap applies Expand to all values in a map, returning a new map.
func (e *Expander) ExpandMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = e.Expand(v)
	}
	return out
}
