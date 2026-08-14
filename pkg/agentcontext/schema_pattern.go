// schema_pattern.go -- Pattern entity: a vetted, worktree-resilient "product
// archetype" in the shared agent-context store. A Pattern is the software analog
// of a textile pattern: an instruction book that, given Materials (typed user
// inputs) and the required basic tools, is STAMPED into a deterministic Plan
// that Mills executes to a deployed instance.
//
// A Pattern composes engrams (atomic proof-gated techniques) and, crucially,
// PINS the load-bearing architecture so the residual freedom collapses to
// material substitution. The S1 kill-test (.loom/kill-test-pattern-go-rest-
// service-2026-06-28.md) showed a pattern is "stampable" only when it pins not
// just transport/storage but the error envelope, per-endpoint bodies, the wiring
// convention, the error model, and the full status-code table — so those are
// first-class pins here.
//
// Like Plan, a Pattern lives in the global Qdrant keyed by a stable, human-
// meaningful pattern_id (pattern-<slug>) and is read cross-agent (never filtered
// by agent_id), so any agent/Mills pod resolves the live catalog by id.
package agentcontext

import (
	"encoding/json"
	"strings"
	"time"
)

// Pattern lifecycle/approval status. A pattern is registered as a candidate and
// promoted to approved once it has shipped enough green instances (the taste
// gate, Slice B2). Mills' rails + front door offer approved patterns by default.
const (
	PatternStatusCandidate  = "candidate"
	PatternStatusApproved   = "approved"
	PatternStatusDeprecated = "deprecated"
)

// patternStatusValid reports whether s is a known pattern status.
func patternStatusValid(s string) bool {
	switch s {
	case PatternStatusCandidate, PatternStatusApproved, PatternStatusDeprecated:
		return true
	default:
		return false
	}
}

// MaterialField is one typed input the user supplies (the "fabric"). The set of
// fields is a Pattern's materials_schema; a Stamp validates supplied Materials
// against it and renders the front-door form (Slice B1) from it.
type MaterialField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // string | int | bool | enum | list | object
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`    // for type=enum
	Default     string   `json:"default,omitempty"` // default expression (may reference other materials)
	Example     string   `json:"example,omitempty"`
}

// ToolRequirement is one capability the environment must provide (a "basic
// tool"). A Stamp aborts loudly if a Required tool is absent — a pattern names
// exactly which tools the follower needs, like "size 8 needles and scissors".
type ToolRequirement struct {
	Name     string `json:"name"`           // e.g. "go", "devbox", "gitlab", "flux"
	Kind     string `json:"kind,omitempty"` // toolchain | mcp_server | deploy_target | secret
	Required bool   `json:"required,omitempty"`
	Check    string `json:"check,omitempty"` // how to verify presence (command or mcp tool name)
}

// PatternPin is a closed architecture decision — what makes a pattern stampable
// rather than a suggestion. Axes range from the obvious (transport, storage) to
// the seams the S1 kill-test proved must also be pinned (error_envelope,
// endpoint_bodies, wiring, error_model, status_codes).
type PatternPin struct {
	Axis  string `json:"axis"`
	Value string `json:"value"`
}

// PatternGauge is the "swatch": the smallest end-to-end check that proves a
// stamp produces a correct result in THIS environment before the full build is
// trusted. Owned by the pattern; run independently of the implementer.
type PatternGauge struct {
	Description string   `json:"description,omitempty"`
	Commands    []string `json:"commands,omitempty"`   // build/test commands that must exit 0
	Assertions  []string `json:"assertions,omitempty"` // black-box behavioral checks
}

// PatternSliceTpl is one slice blueprint. On Stamp it is expanded with Materials
// (placeholder substitution) into a concrete PlanSlice (Slice S1).
type PatternSliceTpl struct {
	Name               string   `json:"name"`
	Goal               string   `json:"goal,omitempty"`
	Files              []string `json:"files,omitempty"` // may contain {{material}} placeholders
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	Engrams            []string `json:"engrams,omitempty"`
}

// PatternProvenance is the taste gate: who authored/approved the pattern and
// proof it has shipped green instances. InstancesShippedGreen drives candidate→
// approved promotion (Slice B2).
type PatternProvenance struct {
	Author                string `json:"author,omitempty"`
	ApprovedBy            string `json:"approved_by,omitempty"`
	InstancesShippedGreen int    `json:"instances_shipped_green,omitempty"`
	Notes                 string `json:"notes,omitempty"`
}

// Pattern is the authoritative record for a product archetype.
type Pattern struct {
	ID          string `json:"id"` // pattern-<slug>, human-meaningful + stable
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Makes       string `json:"makes"` // the type of thing, e.g. "Go REST microservice"
	Description string `json:"description,omitempty"`
	Version     string `json:"version"` // e.g. "0.1"
	Status      string `json:"status"`  // candidate | approved | deprecated

	MaterialsSchema []MaterialField   `json:"materials_schema,omitempty"`
	ToolsManifest   []ToolRequirement `json:"tools_manifest,omitempty"`
	Pins            []PatternPin      `json:"pins,omitempty"`
	Gauge           *PatternGauge     `json:"gauge,omitempty"`
	SliceTemplate   []PatternSliceTpl `json:"slice_template,omitempty"`
	DeployContract  string            `json:"deploy_contract,omitempty"`
	Engrams         []string          `json:"engrams,omitempty"` // composed engram URIs

	Provenance *PatternProvenance `json:"provenance,omitempty"`

	Tags      []string  `json:"tags,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// patternSlug derives a stable kebab slug from a name (defaults to "pattern").
func patternSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "pattern"
	}
	return slug
}

// GeneratePatternID returns a stable "pattern-<slug>" id. Patterns are a curated
// catalog with human-meaningful ids (no random suffix), so the same name always
// resolves to the same pattern.
func GeneratePatternID(name string) string {
	return "pattern-" + patternSlug(name)
}

// patternToPayload flattens a Pattern into a Qdrant payload. Indexed scalars
// live at the top level for keyword filtering; structured sub-objects round-trip
// via JSON blobs (mirrors planToPayload).
func patternToPayload(p *Pattern) map[string]any {
	return map[string]any{
		"id":                    p.ID,
		"slug":                  p.Slug,
		"name":                  p.Name,
		"makes":                 p.Makes,
		"description":           p.Description,
		"version":               p.Version,
		"status":                p.Status, // indexed as "status" for consistency
		"materials_schema_json": marshalJSON(p.MaterialsSchema),
		"tools_manifest_json":   marshalJSON(p.ToolsManifest),
		"pins_json":             marshalJSON(p.Pins),
		"gauge_json":            marshalJSON(p.Gauge),
		"slice_template_json":   marshalJSON(p.SliceTemplate),
		"deploy_contract":       p.DeployContract,
		"engrams":               p.Engrams,
		"provenance_json":       marshalJSON(p.Provenance),
		"tags":                  p.Tags,
		"created_by":            p.CreatedBy,
		"created_at":            p.CreatedAt.Format(time.RFC3339),
		"updated_at":            p.UpdatedAt.Format(time.RFC3339),
		"_record_type":          "pattern",
	}
}

// payloadToPattern rebuilds a Pattern from a Qdrant payload.
func payloadToPattern(payload map[string]any) *Pattern {
	if payload == nil {
		return nil
	}
	id := toString(payload["id"])
	if id == "" {
		return nil
	}
	p := &Pattern{
		ID:             id,
		Slug:           toString(payload["slug"]),
		Name:           toString(payload["name"]),
		Makes:          toString(payload["makes"]),
		Description:    toString(payload["description"]),
		Version:        toString(payload["version"]),
		Status:         toString(payload["status"]),
		DeployContract: toString(payload["deploy_contract"]),
		Engrams:        toStringSlice(payload["engrams"]),
		Tags:           toStringSlice(payload["tags"]),
		CreatedBy:      toString(payload["created_by"]),
	}
	if raw := toString(payload["materials_schema_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.MaterialsSchema)
	}
	if raw := toString(payload["tools_manifest_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.ToolsManifest)
	}
	if raw := toString(payload["pins_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.Pins)
	}
	if raw := toString(payload["gauge_json"]); raw != "" {
		var g PatternGauge
		if json.Unmarshal([]byte(raw), &g) == nil {
			p.Gauge = &g
		}
	}
	if raw := toString(payload["slice_template_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.SliceTemplate)
	}
	if raw := toString(payload["provenance_json"]); raw != "" {
		var pr PatternProvenance
		if json.Unmarshal([]byte(raw), &pr) == nil {
			p.Provenance = &pr
		}
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["created_at"])); err == nil {
		p.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, toString(payload["updated_at"])); err == nil {
		p.UpdatedAt = t
	}
	return p
}

// BuiltinPatterns returns the seed catalog: the Go REST microservice seed
// (formalized from `go-service-scaffold`, validated by the S1 kill-test) plus
// the expansion set in schema_patterns_builtin.go (Go MCP server, Go CLI,
// Python FastAPI service). PatternSvc.SeedBuiltins upserts every builtin on
// startup (idempotent; the code is the source of truth for builtins).
func BuiltinPatterns() []*Pattern {
	return []*Pattern{
		goRESTServicePattern(),
		goMCPServerPattern(),
		goCLIPattern(),
		pyFastAPIServicePattern(),
		// Repo-native dobby cards (schema_patterns_loomcore.go) — candidates
		// until their kill-tests pass or J2 harvests a first green instance.
		loomRunbookPattern(),
		millsMetricPattern(),
		operatorReadEndpointPattern(),
		hudPanelPattern(),
	}
}

// goRESTServicePattern is pattern-go-rest-service v0.2. Its pins are exactly the
// load-bearing axes plus the five stampability seams the S1 kill-test surfaced.
// v0.2 adds the optional target_dir material (monorepo-safe stamping).
func goRESTServicePattern() *Pattern {
	return &Pattern{
		ID:          "pattern-go-rest-service",
		Slug:        "go-rest-service",
		Name:        "Go REST service",
		Makes:       "Go REST microservice (single-entity CRUD over HTTP/JSON)",
		Description: "A stdlib-only Go HTTP/JSON microservice exposing CRUD for one entity, with health/readiness, structured logging, and graceful shutdown. Formalized from go-service-scaffold; validated by the S1 kill-test.",
		Version:     "0.2",
		Status:      PatternStatusApproved,
		MaterialsSchema: []MaterialField{
			{Name: "service_name", Type: "string", Required: true, Description: "Service + binary name (lowercase).", Example: "widget"},
			{Name: "module_path", Type: "string", Description: "Go module path.", Default: "github.com/crb2nu/{{service_name}}"},
			{Name: "entity", Type: "object", Required: true, Description: "The CRUD entity: {name, fields:[{name,type,json}]}.", Example: `{"name":"Widget","fields":[{"name":"Name","type":"string"},{"name":"Quantity","type":"int"}]}`},
			{Name: "storage", Type: "enum", Required: true, Enum: []string{"memory", "postgres", "sqlite"}, Default: "memory", Description: "Repository backend."},
			{Name: "auth", Type: "enum", Enum: []string{"none", "jwt"}, Default: "none", Description: "Auth model."},
			{Name: "deploy_target", Type: "string", Description: "K8s namespace for the deploy contract.", Example: "default"},
			{Name: "target_dir", Type: "string", Description: "Subdirectory of the host repo to stamp into (repo-relative; empty = repo root of a fresh, dedicated repo). Required when stamping into an existing monorepo so the service's go.mod/Makefile/README don't collide with the host's.", Example: "examples/{{service_name}}"},
		},
		ToolsManifest: []ToolRequirement{
			{Name: "go", Kind: "toolchain", Required: true, Check: "go version"},
			{Name: "devbox", Kind: "mcp_server", Required: true, Check: "devbox_status"},
			{Name: "gitlab", Kind: "mcp_server", Required: true, Check: "verify_token"},
			{Name: "flux", Kind: "deploy_target", Required: false, Check: "flux_probe"},
		},
		Pins: []PatternPin{
			{Axis: "transport", Value: "net/http only; routing via http.ServeMux Go 1.22+ method+wildcard patterns"},
			{Axis: "dependencies", Value: "standard library only; go.mod has NO require block"},
			{Axis: "storage", Value: "Repository interface + backend per materials.storage (memory = map+sync.RWMutex)"},
			{Axis: "id_scheme", Value: "16 lowercase hex chars = hex.EncodeToString of 8 crypto/rand bytes"},
			{Axis: "logging", Value: "log/slog JSON handler to stdout"},
			{Axis: "config", Value: "env PORT (default 8080) bound to a config struct"},
			{Axis: "shutdown", Value: "trap SIGINT+SIGTERM; http.Server.Shutdown with 10s timeout; ReadHeaderTimeout 5s"},
			{Axis: "layout", Value: "cmd/<name>/main.go + internal/{config,repository,service,handler,server}"},
			{Axis: "error_envelope", Value: `all non-2xx responses return {"error":"<message>"} with Content-Type application/json`},
			{Axis: "endpoint_bodies", Value: "every endpoint has a defined body; /healthz {\"status\":\"ok\"}; /readyz 200 empty JSON"},
			{Axis: "wiring", Value: "DI (repo→service→handler→http.Server) lives in internal/server.Run(); cmd/<name>/main.go is a thin entrypoint"},
			{Axis: "error_model", Value: "service returns sentinel ErrValidation; handler maps via errors.Is to 400"},
			{Axis: "status_codes", Value: "POST 201/400; GET-one 200/404; GET-list 200 ([] not null); DELETE 204/404; non-validation create error 500"},
		},
		Gauge: &PatternGauge{
			Description: "Builds, vets, tests, then black-box CRUD + health round-trip against the running server.",
			Commands:    []string{"go build ./...", "go vet ./...", "go test ./..."},
			Assertions: []string{
				"GET /healthz -> 200",
				"POST /<plural> -> 201 echoing supplied fields + a server-generated id",
				"GET /<plural>/{id} -> 200 round-trips the entity",
				"GET /<plural>/{unknown} -> 404",
			},
		},
		SliceTemplate: []PatternSliceTpl{
			{
				Name:               "scaffold {{service_name}} service",
				Goal:               "Create the full stdlib-only Go REST service for entity {{entity.name}} with {{storage}} storage, satisfying the pinned architecture and the gauge.",
				Files:              []string{"cmd/{{service_name}}/main.go", "internal/config/config.go", "internal/repository/{{entity_lower}}_repo.go", "internal/service/{{entity_lower}}_service.go", "internal/handler/{{entity_lower}}_handler.go", "internal/handler/{{entity_lower}}_handler_test.go", "internal/server/server.go", "go.mod", "Makefile", "README.md"},
				AcceptanceCriteria: "go build/vet/test pass; the pattern gauge passes 100%; go.mod has no require block; no architecture beyond the pins.",
				Engrams:            goRESTEngramURIs(), // this slice composes all builtin Go REST engrams
			},
		},
		DeployContract: "merged MR + green CI + a pod passing /healthz in materials.deploy_target",
		Engrams:        goRESTEngramURIs(), // composed engrams (Slice A2); a green stamp verifies them against the merged instance
		Provenance: &PatternProvenance{
			Author:                "cody+claude",
			ApprovedBy:            "S1 kill-test 2026-06-28",
			InstancesShippedGreen: 0, // lean kill-test stamped+gauged; no merged instance yet
			Notes:                 "Validated by .loom/kill-test-pattern-go-rest-service-2026-06-28.md (gauge 10/10, zero unrequested architecture). Promote to >0 instances after the full Mills e2e stamp merges.",
		},
		Tags: []string{"go", "service", "rest", "crud", "scaffold"},
	}
}
