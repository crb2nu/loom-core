package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestPlanCreateSchemaPublishesHandlerArguments(t *testing.T) {
	_, tools := testServer()
	tool := toolByName(tools, "agent_plan_create")
	if tool == nil {
		t.Fatal("agent_plan_create is not registered")
	}
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"title"}) {
		t.Fatalf("planning metadata must remain optional: required=%v", tool.InputSchema.Required)
	}

	// Check actual argument reads in PlanSvc.Create, not tool-name mentions in
	// prose or retired examples. New accepted fields must also be discoverable.
	source, err := parser.ParseFile(token.NewFileSet(), repoRelative(t, "pkg/agentcontext/svc_plans.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var handler *ast.FuncDecl
	for _, decl := range source.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "Create" {
			handler = fn
			break
		}
	}
	if handler == nil {
		t.Fatal("PlanSvc.Create handler not found")
	}
	checked := make(map[string]bool)
	assertField := func(expr ast.Expr, expectedType string) {
		t.Helper()
		literal, ok := expr.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return
		}
		name, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		checked[name] = true
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Errorf("handler accepts %q but agent_plan_create does not advertise it", name)
			return
		}
		if expectedType != "" && property["type"] != expectedType {
			t.Errorf("%s schema type=%v, handler expects %s", name, property["type"], expectedType)
		}
	}
	ast.Inspect(handler.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			selector, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || len(n.Args) == 0 {
				break
			}
			if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "v" {
				kinds := map[string]string{"Required": "string", "String": "string", "StringSlice": "array", "Int": "integer"}
				if kind, ok := kinds[selector.Sel.Name]; ok {
					assertField(n.Args[0], kind)
				}
			}
		case *ast.IndexExpr:
			if receiver, ok := n.X.(*ast.Ident); ok && receiver.Name == "args" {
				assertField(n.Index, "")
			}
		}
		return true
	})
	for _, field := range []string{"title", "riskiest_assumption", "kill_test", "success", "slices"} {
		if !checked[field] {
			t.Errorf("handler drift check did not inspect expected argument %q", field)
		}
	}
}

func TestPlanCreateDocumentedExampleMatchesPublishedSchema(t *testing.T) {
	_, tools := testServer()
	tool := toolByName(tools, "agent_plan_create")
	if tool == nil {
		t.Fatal("agent_plan_create is not registered")
	}
	doc, err := os.ReadFile(repoRelative(t, "docs/PLAN_STORE.md"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile("(?s)<!-- agent-plan-create-example -->\\s*```json\\s*(.*?)\\s*```")
	matches := pattern.FindAllSubmatch(doc, -1)
	if len(matches) != 1 {
		t.Fatalf("want one marked executable example, got %d", len(matches))
	}
	var args any
	if err := json.Unmarshal(matches[0][1], &args); err != nil {
		t.Fatalf("invalid documented JSON example: %v", err)
	}
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schemaDoc map[string]any
	if err := json.Unmarshal(raw, &schemaDoc); err != nil {
		t.Fatal(err)
	}
	// Discovery is a stricter documentation contract than runtime acceptance:
	// even when the server permits extra fields, examples must advertise them.
	var disallowUnknown func(map[string]any)
	disallowUnknown = func(node map[string]any) {
		if node["type"] == "object" {
			node["additionalProperties"] = false
		}
		for _, value := range node {
			if child, ok := value.(map[string]any); ok {
				disallowUnknown(child)
			}
		}
	}
	disallowUnknown(schemaDoc)
	raw, err = json.Marshal(schemaDoc)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := jsonschema.CompileString("plan-create.schema.json", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(args); err != nil {
		t.Fatalf("documented example does not match discoverable schema: %v", err)
	}
	if err := schema.Validate(map[string]any{"title": "Legacy minimal call"}); err != nil {
		t.Fatalf("minimal create call no longer valid: %v", err)
	}
}
