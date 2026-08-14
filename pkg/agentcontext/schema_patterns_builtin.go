// schema_patterns_builtin.go -- the builtin Pattern seed catalog beyond the
// original Go REST seed (which lives in schema_pattern.go next to the Pattern
// schema it motivated).
//
// Each pattern here follows the discipline the S1 kill-test established: pin
// every load-bearing axis PLUS the stampability seams (error model, output
// contract per surface, wiring convention, full exit/status table) so a
// context-free follower's residual freedom collapses to material substitution.
// A pattern ships as `candidate` until its own kill-test passes (a fresh agent
// stamps it from the manifest alone and an independent gauge scores 100%), at
// which point it is seeded `approved` with the kill-test recorded in provenance
// — exactly how pattern-go-rest-service earned its status.
package agentcontext

// goMCPServerPattern is pattern-go-mcp-server v0.1: a stdlib-only Go MCP server
// speaking JSON-RPC 2.0 over stdio. Formalized from the loom-go-mcp-scaffold
// skill and loom-core's cmd/mcp-* conventions, but pinned to newline-delimited
// stdio + zero dependencies so a stamp builds anywhere the Go toolchain exists.
// Scaffolded tool handlers are deterministic echo stubs — real behavior lands
// in follow-up slices, mirroring how the REST pattern scaffolds CRUD.
func goMCPServerPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-go-mcp-server",
		Slug:        "go-mcp-server",
		Name:        "Go MCP server",
		Makes:       "Go MCP server (stdio JSON-RPC 2.0 tool server)",
		Description: "A stdlib-only Go MCP server over stdio: newline-delimited JSON-RPC 2.0 implementing initialize, tools/list, and tools/call with a declarative tool registry. Tool handlers scaffold as deterministic echo stubs. Formalized from loom-go-mcp-scaffold; validated by its kill-test (gauge 18/18).",
		Version:     "0.1",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "service_name", Type: "string", Required: true, Description: "Server + binary name (lowercase, conventionally mcp-<thing>).", Example: "mcp-echo"},
			{Name: "module_path", Type: "string", Description: "Go module path.", Default: "github.com/crb2nu/{{service_name}}"},
			{Name: "tools", Type: "list", Required: true, Description: "Tool definitions: [{name, description, input_fields:[{name,type,description,required}]}]. Field types: string|int|bool.", Example: `[{"name":"echo","description":"Echo the supplied message","input_fields":[{"name":"message","type":"string","required":true,"description":"Text to echo"}]}]`},
			{Name: "target_dir", Type: "string", Description: "Subdirectory of the host repo to stamp into (repo-relative; empty = repo root of a fresh, dedicated repo).", Example: "examples/{{service_name}}"},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "go", Kind: "toolchain", Required: true, Check: "go version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "transport", Value: "JSON-RPC 2.0 over stdio; exactly one compact JSON object per line (newline-delimited, NO Content-Length framing); requests read from stdin, responses written to stdout"},
			{Axis: "protocol", Value: `MCP protocol version "2025-06-18"; initialize result = {protocolVersion, capabilities:{tools:{listChanged:false}}, serverInfo:{name,version}}`},
			{Axis: "dependencies", Value: "standard library only; go.mod has NO require block"},
			{Axis: "methods", Value: "initialize, notifications/initialized (accepted, no response), tools/list, tools/call, ping (returns empty object result); nothing else"},
			{Axis: "error_model", Value: "protocol failures use JSON-RPC error codes: -32700 parse error, -32601 method not found, -32602 invalid params (incl. unknown tool name); tool-level handler failures return result.isError=true with a text content item, NEVER a protocol error"},
			{Axis: "tool_results", Value: `tools/call returns {content:[{type:"text",text:<compact JSON>}]}; scaffolded handlers echo {"tool":"<name>","args":<arguments>}`},
			{Axis: "tool_schema", Value: `each tool's inputSchema is a JSON Schema object (type object, properties from input_fields with their types/descriptions, required array) rendered by the registry; material field types map string->"string", int->"integer", bool->"boolean"`},
			{Axis: "id_handling", Value: "response id mirrors the request id verbatim (number or string); requests without an id are notifications and never get a response"},
			{Axis: "layout", Value: "cmd/<service_name>/main.go + internal/{mcpserver,tools}; protocol loop in internal/mcpserver, tool registry in internal/tools"},
			{Axis: "wiring", Value: "cmd/<service_name>/main.go is a thin entrypoint: build the internal/tools registry, then call mcpserver.Run(os.Stdin, os.Stdout, registry)"},
			{Axis: "logging", Value: "log/slog JSON handler to STDERR only — stdout is exclusively the protocol channel"},
			{Axis: "shutdown", Value: "exit 0 on stdin EOF after finishing the in-flight response; no signal handling"},
			{Axis: "config", Value: "no environment config; server name and version are compiled-in constants (version defaults to 0.1.0)"},
		},
		Gauge: &PatternGauge{
			Description: "Builds, vets, tests, then black-box JSON-RPC round-trips by piping newline-delimited requests into the built binary.",
			Commands:    []string{"go build ./...", "go vet ./...", "go test ./..."},
			Assertions: []string{
				"initialize -> result.protocolVersion == \"2025-06-18\" and result.serverInfo.name == service_name",
				"tools/list -> result.tools contains every declared tool, each with description + inputSchema (type object, required fields listed)",
				"tools/call on a declared tool with valid arguments -> result.content[0].text is compact JSON echoing {\"tool\":name,\"args\":arguments}",
				"tools/call with an unknown tool name -> JSON-RPC error code -32602",
				"an unknown method -> JSON-RPC error code -32601",
				"a notification (no id) produces no response line; stdout carries protocol JSON only",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "scaffold {{service_name}} MCP server",
				Goal:               "Create the full stdlib-only Go MCP server {{service_name}} over stdio, implementing the declared tools as echo stubs: {{tools}}. Satisfy every pinned axis and the gauge.",
				Files:              []string{"cmd/{{service_name}}/main.go", "internal/mcpserver/server.go", "internal/mcpserver/server_test.go", "internal/tools/registry.go", "internal/tools/registry_test.go", "go.mod", "Makefile", "README.md"},
				AcceptanceCriteria: "go build/vet/test pass; the pattern gauge passes 100%; go.mod has no require block; stdout emits protocol JSON only; no architecture beyond the pins.",
				Engrams:            goMCPEngramURIs(),
			},
		},
		DeployContract: "merged MR + green CI + the built binary answers initialize and tools/list over stdio (stdio server: no k8s deploy)",
		Engrams:        goMCPEngramURIs(),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-07-03",
			Notes:      "Validated by .loom/kill-test-pattern-expansion-2026-07-03.md: context-free follower, independent gauge 18/18, zero unrequested architecture; the one architecture-class gap (JSON Schema type mapping) is now pinned.",
		},
		Tags: []string{"go", "mcp", "server", "stdio", "scaffold"},
	}
}

// goCLIPattern is pattern-go-cli v0.1: a stdlib-only Go CLI with flag-based
// subcommands. Scaffolded subcommands print a deterministic JSON echo of their
// resolved flag values, so the gauge can verify dispatch + flag parsing without
// domain behavior; real command logic lands in follow-up slices.
func goCLIPattern() *Pattern {
	return &Pattern{
		ID:          "pattern-go-cli",
		Slug:        "go-cli",
		Name:        "Go CLI tool",
		Makes:       "Go CLI tool (subcommand interface over stdlib flag)",
		Description: "A stdlib-only Go command-line tool with flag.FlagSet subcommands, a builtin version command with -ldflags injection, pinned exit codes, and stderr/stdout separation. Scaffolded subcommands echo their parsed flags as JSON. Validated by its kill-test (gauge 17/17).",
		Version:     "0.1",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "tool_name", Type: "string", Required: true, Description: "Binary + command name (lowercase).", Example: "sprockctl"},
			{Name: "module_path", Type: "string", Description: "Go module path.", Default: "github.com/crb2nu/{{tool_name}}"},
			{Name: "commands", Type: "list", Required: true, Description: "Subcommand definitions: [{name, description, flags:[{name,type,default,description}]}]. Flag types: string|int|bool.", Example: `[{"name":"greet","description":"Print a greeting","flags":[{"name":"name","type":"string","default":"world","description":"Who to greet"}]}]`},
			{Name: "target_dir", Type: "string", Description: "Subdirectory of the host repo to stamp into (repo-relative; empty = repo root of a fresh, dedicated repo).", Example: "tools/{{tool_name}}"},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "go", Kind: "toolchain", Required: true, Check: "go version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
		},
		Pins: []PatternPin{
			{Axis: "dependencies", Value: "standard library only; go.mod has NO require block (no cobra/urfave/kong)"},
			{Axis: "dispatch", Value: "os.Args[1] selects the subcommand; each subcommand owns a flag.NewFlagSet(name, flag.ContinueOnError) whose output writer is the CLI's stderr"},
			{Axis: "exit_codes", Value: "0 success; 2 usage error (no/unknown subcommand, flag parse failure); 1 runtime failure; help requested via -h/--help exits 0"},
			{Axis: "usage", Value: "bare invocation or an unknown subcommand prints usage to stderr (tool name, one line per subcommand with its description) and exits 2"},
			{Axis: "version", Value: `a builtin "version" subcommand prints the version string to stdout; the version lives in var Version = "dev" in internal/cli/version.go, overridable via -ldflags "-X <module_path>/internal/cli.Version=<v>"`},
			{Axis: "output", Value: `scaffolded subcommands print exactly one compact JSON object to stdout: {"command":"<name>","flags":{<flag>:<resolved value>,...}} with flag values in their declared types`},
			{Axis: "logging", Value: "diagnostics and errors go to stderr; stdout carries command output only"},
			{Axis: "layout", Value: "cmd/<tool_name>/main.go + internal/cli/{root.go,commands.go,version.go}; internal/cli owns dispatch, flag sets, and all subcommand implementations"},
			{Axis: "wiring", Value: "cmd/<tool_name>/main.go is a thin entrypoint: os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)); Run returns the exit code and never calls os.Exit itself"},
			{Axis: "testing", Value: "internal/cli tests drive Run() with bytes.Buffer writers and assert exit codes + output; unit tests never exec the built binary"},
			{Axis: "config", Value: "flags only; no config files, no environment variables"},
		},
		Gauge: &PatternGauge{
			Description: "Builds, vets, tests, then black-box invocations of the built binary asserting exit codes and stdout/stderr separation.",
			Commands:    []string{"go build ./...", "go vet ./...", "go test ./..."},
			Assertions: []string{
				"bare invocation -> exit 2, usage on stderr listing every declared subcommand, nothing on stdout",
				"`version` subcommand -> exit 0, prints the version to stdout",
				"a declared subcommand with a flag set -> exit 0, stdout is one compact JSON object echoing the command name + resolved flags",
				"a declared subcommand with flags omitted -> exit 0, JSON echoes the declared defaults",
				"an unknown subcommand -> exit 2, usage on stderr",
				"a declared subcommand with -h -> exit 0",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "scaffold {{tool_name}} CLI",
				Goal:               "Create the full stdlib-only Go CLI {{tool_name}} with the declared subcommands as flag-echo stubs: {{commands}}. Satisfy every pinned axis and the gauge.",
				Files:              []string{"cmd/{{tool_name}}/main.go", "internal/cli/root.go", "internal/cli/commands.go", "internal/cli/version.go", "internal/cli/root_test.go", "go.mod", "Makefile", "README.md"},
				AcceptanceCriteria: "go build/vet/test pass; the pattern gauge passes 100%; go.mod has no require block; no architecture beyond the pins.",
				Engrams:            goCLIEngramURIs(),
			},
		},
		DeployContract: "merged MR + green CI + the built binary passes the gauge (usage, version, subcommand flag round-trip)",
		Engrams:        goCLIEngramURIs(),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-07-03",
			Notes:      "Validated by .loom/kill-test-pattern-expansion-2026-07-03.md: context-free follower, independent gauge 17/17, ZERO architecture-class gaps, zero unrequested architecture.",
		},
		Tags: []string{"go", "cli", "tool", "scaffold"},
	}
}

// pyFastAPIServicePattern is pattern-python-fastapi-service v0.1: the Python
// sibling of the Go REST seed — a FastAPI single-entity CRUD service honoring
// the SAME external contract (error envelope, status-code table, endpoint
// bodies, 16-hex ids) so gauges and consumers are interchangeable across the
// two languages. The package directory is pinned to app/ (not the service
// name) so engram file_ref proofs stay entity-independent.
func pyFastAPIServicePattern() *Pattern {
	return &Pattern{
		ID:          "pattern-python-fastapi-service",
		Slug:        "python-fastapi-service",
		Name:        "Python FastAPI service",
		Makes:       "Python FastAPI microservice (single-entity CRUD over HTTP/JSON)",
		Description: "A FastAPI microservice exposing create/read/list/delete for one entity with in-memory storage, health/readiness, the workspace-standard error envelope, and uv-managed dependencies. Mirrors the Go REST pattern's external contract. Validated by its kill-test (gauge 15/15).",
		Version:     "0.1",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "service_name", Type: "string", Required: true, Description: "Service name (lowercase; the Python package is pinned to app/ regardless).", Example: "widget-api"},
			{Name: "entity", Type: "object", Required: true, Description: "The CRUD entity: {name, fields:[{name,type}]}. Field types: str|int|bool|float.", Example: `{"name":"Widget","fields":[{"name":"name","type":"str"},{"name":"quantity","type":"int"}]}`},
			{Name: "deploy_target", Type: "string", Description: "K8s namespace for the deploy contract.", Example: "default"},
			{Name: "target_dir", Type: "string", Description: "Subdirectory of the host repo to stamp into (repo-relative; empty = repo root of a fresh, dedicated repo).", Example: "examples/{{service_name}}"},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "python", Kind: "toolchain", Required: true, Check: "python3 --version"},
			{Name: "uv", Kind: "toolchain", Required: true, Check: "uv --version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
			{Name: "flux", Kind: "deploy_target", Required: false, Check: "flux_probe"},
		},
		Pins: []PatternPin{
			{Axis: "framework", Value: "FastAPI served by uvicorn; pydantic v2 models; no ORM, no database driver"},
			{Axis: "dependencies", Value: "pyproject.toml [project] dependencies exactly: fastapi, uvicorn; [dependency-groups] dev exactly: pytest, httpx; nothing else"},
			{Axis: "python_version", Value: "requires-python >=3.11"},
			{Axis: "package_manager", Value: "uv; uv.lock committed; run via `uv run uvicorn app.main:app`"},
			{Axis: "layout", Value: "app/{__init__.py,main.py,errors.py,config.py,models.py,repository.py,service.py,api.py} + tests/test_api.py — the package is literally app/ (entity- and service-name-independent)"},
			{Axis: "wiring", Value: "create_app() in app/main.py wires repository -> service -> APIRouter and registers error handlers; module level `app = create_app()` for uvicorn"},
			{Axis: "storage", Value: "in-memory dict guarded by threading.Lock behind a Repository class in app/repository.py"},
			{Axis: "id_scheme", Value: "16 lowercase hex chars = uuid.uuid4().hex[:16], generated server-side"},
			{Axis: "error_envelope", Value: `all non-2xx responses return {"error":"<message>"} as application/json — exception handlers in app/errors.py override FastAPI's default {"detail":...} shape`},
			{Axis: "validation_mapping", Value: "pydantic request-validation failures map to 400 with the standard error envelope (override FastAPI's default 422 via a RequestValidationError handler)"},
			{Axis: "endpoints", Value: "exactly POST /<plural>, GET /<plural>, GET /<plural>/{id}, DELETE /<plural>/{id}, GET /healthz, GET /readyz — NO update endpoint in the scaffold; <plural> = lowercased entity name + \"s\""},
			{Axis: "endpoint_bodies", Value: `every endpoint has a defined body; /healthz {"status":"ok"}; /readyz 200 {}`},
			{Axis: "status_codes", Value: "POST 201/400; GET-one 200/404; GET-list 200 ([] not null); DELETE 204/404; a GLOBAL app-wide exception handler maps any unexpected failure to 500 with the error envelope"},
			{Axis: "error_model", Value: "the service layer raises NotFoundError/ValidationError from app/errors.py; the registered handlers map them to 404/400 — routes contain no status-code logic; ValidationError is the scaffolded seam for business rules landing in follow-up slices (pydantic covers request-shape validation)"},
			{Axis: "logging", Value: "stdlib logging with a JSON-line formatter configured in app/config.py; logs to stdout; no third-party logging lib"},
			{Axis: "config", Value: "env PORT (default 8000) read once into a plain dataclass in app/config.py"},
		},
		Gauge: &PatternGauge{
			Description: "Syncs the environment, runs tests, then black-box CRUD + health round-trip against the running server.",
			Commands:    []string{"uv sync", "uv run pytest -q"},
			Assertions: []string{
				"GET /healthz -> 200 {\"status\":\"ok\"}",
				"POST /<plural> -> 201 echoing supplied fields + a server-generated 16-hex id",
				"GET /<plural>/{id} -> 200 round-trips the entity",
				"GET /<plural>/{unknown} -> 404 with the {\"error\":...} envelope",
				"POST with a missing required field -> 400 (not 422) with the {\"error\":...} envelope",
				"GET /<plural> on an empty store -> 200 []",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "scaffold {{service_name}} FastAPI service",
				Goal:               "Create the full FastAPI CRUD service for entity {{entity.name}} with in-memory storage, satisfying the pinned architecture and the gauge.",
				Files:              []string{"pyproject.toml", "uv.lock", "app/__init__.py", "app/main.py", "app/errors.py", "app/config.py", "app/models.py", "app/repository.py", "app/service.py", "app/api.py", "tests/test_api.py", "Makefile", "README.md"},
				AcceptanceCriteria: "uv sync + uv run pytest pass; the pattern gauge passes 100%; dependencies exactly as pinned; no architecture beyond the pins.",
				Engrams:            pyFastAPIEngramURIs(),
			},
		},
		DeployContract: "merged MR + green CI + a pod passing /healthz in materials.deploy_target",
		Engrams:        pyFastAPIEngramURIs(),
		Provenance: &PatternProvenance{
			Author:     "cody+claude",
			ApprovedBy: "kill-test 2026-07-03",
			Notes:      "Validated by .loom/kill-test-pattern-expansion-2026-07-03.md: context-free follower, independent gauge 15/15, zero unrequested architecture; its three architecture-class gaps (endpoint set incl. no-update, global 500 handler, ValidationError seam) are now pinned.",
		},
		Tags: []string{"python", "fastapi", "service", "rest", "crud", "scaffold"},
	}
}
