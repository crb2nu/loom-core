// schema_engrams_builtin.go -- the builtin engram seed catalog (Slice A2).
//
// The engram engine (svc_engrams.go) shipped with an EMPTY catalog: nothing
// produced engrams. The Pattern Loop closes that gap — the building-block
// idioms a Pattern composes are seeded here as tier-1 engrams, and a green
// stamp VERIFIES them against the merged instance (svc_patterns_a2.go), turning
// unverified idioms into proven, repo-unlocked techniques.
//
// Seeding is idempotent and best-effort: seedBuiltinEngrams runs on service
// start alongside PatternSvc.SeedBuiltins; a duplicate URI (re-seed on restart)
// is a no-op, and a persistence failure is logged, never fatal.
//
// PROOF CHOICE: each builtin is a tier-1 file_ref keyed to a STABLE, entity-
// independent path in a stamped Go REST service (the pattern's slice template
// lists these exact paths). That makes the proof resolve against ANY correctly
// stamped instance, regardless of the entity name the materials chose. Stronger
// command: proofs (run the gauge) need the devbox sandbox and are deferred.
package agentcontext

import "context"

// builtinEngram is a compact, declarative seed record. seedBuiltinEngrams turns
// each into an agent_engram_add call.
type builtinEngram struct {
	Family   string
	Slug     string
	Title    string
	Problem  string
	Solution string
	Proof    string // tier-1: a file_ref relative to a stamped service's root
	Language string
	Scope    string
	Tags     []string
}

// uri returns the engram's canonical URI (engram://family/slug).
func (b builtinEngram) uri() string {
	return EngramURI{Family: b.Family, Slug: b.Slug}.String()
}

// allBuiltinEngrams concatenates every pattern's engram set. seedBuiltinEngrams
// iterates this; the per-pattern URI helpers below keep each Pattern's Engrams
// list and the seed catalog from drifting.
func allBuiltinEngrams() []builtinEngram {
	var out []builtinEngram
	out = append(out, builtinGoRESTEngrams()...)
	out = append(out, builtinGoMCPEngrams()...)
	out = append(out, builtinGoCLIEngrams()...)
	out = append(out, builtinPyFastAPIEngrams()...)
	// Repo-native dobby-card engrams (schema_engrams_loomcore.go).
	out = append(out, builtinLoomCIEngrams()...)
	out = append(out, builtinLoomRunbookEngrams()...)
	out = append(out, builtinLoomOpsEngrams()...)
	out = append(out, builtinLoomObsEngrams()...)
	out = append(out, builtinLoomHUDEngrams()...)
	return out
}

// engramURIs maps a builtin set to its canonical URIs, in declaration order.
func engramURIs(engrams []builtinEngram) []string {
	uris := make([]string, 0, len(engrams))
	for _, e := range engrams {
		uris = append(uris, e.uri())
	}
	return uris
}

// builtinGoRESTEngrams are the composed building blocks of
// pattern-go-rest-service. Each maps to a load-bearing pin and a stable layout
// file, so a stamped instance unlocks all three at once on a green merge.
//
// The stdlib-only-gomod engram is SHARED: every stdlib-only Go pattern
// (go-rest-service, go-mcp-server, go-cli) composes it, so its tags name all
// three composers and a green stamp of any of them can verify it.
func builtinGoRESTEngrams() []builtinEngram {
	const patternTag = "pattern:pattern-go-rest-service"
	return []builtinEngram{
		{
			Family:   "go-http",
			Slug:     "server-graceful-shutdown",
			Title:    "Go HTTP server: ServeMux wiring + graceful shutdown",
			Problem:  "A stdlib Go HTTP service needs request routing and a clean shutdown so in-flight requests drain on SIGINT/SIGTERM instead of being cut off.",
			Solution: "Compose repo→service→handler→*http.Server in internal/server.Run(); route with http.ServeMux Go 1.22+ method+wildcard patterns (no third-party router); trap SIGINT+SIGTERM and call http.Server.Shutdown with a 10s timeout (ReadHeaderTimeout 5s).",
			Proof:    "internal/server/server.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "go-config",
			Slug:     "env-config-struct",
			Title:    "Go service config from environment into a typed struct",
			Problem:  "Service configuration (port, etc.) should come from the environment with sane defaults, bound once into a typed struct rather than read via os.Getenv ad hoc.",
			Solution: "Read env (PORT default 8080) into a config struct in internal/config and pass the struct down the dependency chain; one place owns env parsing and defaults.",
			Proof:    "internal/config/config.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "go-build",
			Slug:     "stdlib-only-gomod",
			Title:    "Stdlib-only Go module (no require block)",
			Problem:  "A minimal service should avoid third-party dependencies so it builds anywhere with just the Go toolchain and has no supply-chain surface.",
			Solution: "Keep go.mod with only a module path + go directive and NO require block; rely on the standard library (net/http, log/slog, crypto/rand, encoding/json, …).",
			Proof:    "go.mod",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag, "pattern:pattern-go-mcp-server", "pattern:pattern-go-cli"},
		},
	}
}

// goRESTEngramURIs returns the URIs of the Go REST pattern's composed engrams,
// in declaration order. goRESTServicePattern() references this so the pattern
// and the seed catalog can never drift.
func goRESTEngramURIs() []string {
	return engramURIs(builtinGoRESTEngrams())
}

// stdlibGomodEngramURI is the shared stdlib-only go.mod engram (declared in the
// Go REST set) that every stdlib-only Go pattern composes.
func stdlibGomodEngramURI() string {
	return EngramURI{Family: "go-build", Slug: "stdlib-only-gomod"}.String()
}

// builtinGoMCPEngrams are the composed building blocks of
// pattern-go-mcp-server: the stdio JSON-RPC protocol loop and the declarative
// tool registry. Proofs are stable, material-independent layout files.
func builtinGoMCPEngrams() []builtinEngram {
	const patternTag = "pattern:pattern-go-mcp-server"
	return []builtinEngram{
		{
			Family:   "go-jsonrpc",
			Slug:     "stdio-line-protocol",
			Title:    "JSON-RPC 2.0 over stdio, one message per line",
			Problem:  "An MCP-style tool server must exchange JSON-RPC messages over stdin/stdout with no transport library, while keeping stdout protocol-clean and shutting down on EOF.",
			Solution: "Read stdin line-by-line (bufio.Scanner, one compact JSON object per line), dispatch by method, write one response line to stdout mirroring the request id verbatim; never answer notifications (no id); log via log/slog to STDERR only; return cleanly on stdin EOF.",
			Proof:    "internal/mcpserver/server.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "go-mcp",
			Slug:     "tool-registry-inputschema",
			Title:    "Declarative MCP tool registry with generated inputSchema",
			Problem:  "tools/list must advertise every tool with a JSON Schema and tools/call must dispatch by name, without hand-writing schema JSON per tool.",
			Solution: "Hold tools in a registry in internal/tools where each tool declares name/description/typed fields; the registry renders inputSchema (type object, properties, required) for tools/list and resolves tools/call handlers by name, returning -32602 for unknown tools.",
			Proof:    "internal/tools/registry.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
	}
}

// goMCPEngramURIs returns pattern-go-mcp-server's composed engram URIs: its own
// set plus the shared stdlib-only go.mod engram.
func goMCPEngramURIs() []string {
	return append(engramURIs(builtinGoMCPEngrams()), stdlibGomodEngramURI())
}

// builtinGoCLIEngrams are the composed building blocks of pattern-go-cli:
// FlagSet subcommand dispatch and ldflags version injection.
func builtinGoCLIEngrams() []builtinEngram {
	const patternTag = "pattern:pattern-go-cli"
	return []builtinEngram{
		{
			Family:   "go-cli",
			Slug:     "flagset-subcommand-dispatch",
			Title:    "Stdlib subcommand dispatch via flag.FlagSet",
			Problem:  "A CLI needs git-style subcommands with per-command flags, pinned exit codes, and testability, without pulling in cobra/urfave.",
			Solution: "internal/cli.Run(args, stdout, stderr) returns an exit code instead of exiting: os.Args[1] selects a flag.NewFlagSet(name, flag.ContinueOnError) with Output set to stderr; parse failures and unknown subcommands print usage to stderr and return 2; tests drive Run with bytes.Buffer writers.",
			Proof:    "internal/cli/root.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "go-cli",
			Slug:     "ldflags-version-injection",
			Title:    "Build-time version injection via -ldflags -X",
			Problem:  "A CLI binary should report the version it was built from without a hardcoded constant drifting from tags.",
			Solution: `Keep var Version = "dev" in internal/cli/version.go (a stable, package-scoped var) and override at build time with -ldflags "-X <module>/internal/cli.Version=<v>"; a builtin version subcommand prints it to stdout.`,
			Proof:    "internal/cli/version.go",
			Language: "go",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
	}
}

// goCLIEngramURIs returns pattern-go-cli's composed engram URIs: its own set
// plus the shared stdlib-only go.mod engram.
func goCLIEngramURIs() []string {
	return append(engramURIs(builtinGoCLIEngrams()), stdlibGomodEngramURI())
}

// builtinPyFastAPIEngrams are the composed building blocks of
// pattern-python-fastapi-service. The package directory is pinned to app/ so
// these proofs stay entity- and service-name-independent.
func builtinPyFastAPIEngrams() []builtinEngram {
	const patternTag = "pattern:pattern-python-fastapi-service"
	return []builtinEngram{
		{
			Family:   "py-fastapi",
			Slug:     "app-factory-di",
			Title:    "FastAPI app factory wiring repository -> service -> router",
			Problem:  "A FastAPI service needs one composition root so tests can build isolated apps and uvicorn gets a stable import path, instead of module-level singletons wired ad hoc.",
			Solution: "create_app() in app/main.py constructs repository -> service -> APIRouter, registers error handlers, and returns the app; `app = create_app()` at module level serves `uvicorn app.main:app`; tests call create_app() for a fresh store per test.",
			Proof:    "app/main.py",
			Language: "python",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "py-fastapi",
			Slug:     "error-envelope-handlers",
			Title:    "Uniform JSON error envelope via FastAPI exception handlers",
			Problem:  `Consumers need one error shape ({"error": msg}) and a 400 on bad request bodies, but FastAPI defaults to {"detail": ...} and 422 for validation failures.`,
			Solution: "app/errors.py defines NotFoundError/ValidationError plus register_error_handlers(app): handlers map domain errors to 404/400 and RequestValidationError to 400, all returning JSONResponse({\"error\": msg}) — routes contain no status-code logic.",
			Proof:    "app/errors.py",
			Language: "python",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
		{
			Family:   "py-config",
			Slug:     "env-dataclass-config",
			Title:    "Python service config from environment into a dataclass",
			Problem:  "Service configuration (port, etc.) should be read from the environment once with defaults, not via os.environ lookups scattered through the code.",
			Solution: "app/config.py owns a plain dataclass loaded from the environment (PORT default 8000) plus the JSON-line logging setup; everything downstream takes the config object.",
			Proof:    "app/config.py",
			Language: "python",
			Scope:    "workspace",
			Tags:     []string{patternTag},
		},
	}
}

// pyFastAPIEngramURIs returns pattern-python-fastapi-service's composed engram
// URIs, in declaration order.
func pyFastAPIEngramURIs() []string {
	return engramURIs(builtinPyFastAPIEngrams())
}

// seedBuiltinEngrams upserts the builtin engram catalog idempotently. Mirrors
// PatternSvc.SeedBuiltins: called best-effort on service start. A duplicate URI
// (already seeded) and any add failure are logged at debug/warn and skipped —
// seeding must never block startup.
func (s *Service) seedBuiltinEngrams(ctx context.Context) {
	for _, e := range allBuiltinEngrams() {
		args := map[string]any{
			"title":    e.Title,
			"problem":  e.Problem,
			"solution": e.Solution,
			"proof":    e.Proof,
			"family":   e.Family,
			"slug":     e.Slug,
			"tier":     1,
			"language": e.Language,
			"scope":    e.Scope,
			"tags":     toAnySlice(e.Tags),
		}
		res, err := s.HandleEngramAdd(ctx, args)
		if err != nil {
			s.logger.Warn("seed engram failed", "uri", e.uri(), "error", err)
			continue
		}
		if res != nil && res.IsError {
			// Almost always "already exists" — idempotent re-seed on restart.
			s.logger.Debug("seed engram skipped", "uri", e.uri())
			continue
		}
		s.logger.Debug("seeded builtin engram", "uri", e.uri())
	}
}
