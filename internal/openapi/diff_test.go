package openapi

import (
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, raw string) *Doc {
	t.Helper()
	doc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

const baseSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "T", "version": "1.0.0"},
  "servers": [{"url": "http://a.internal"}],
  "paths": {
    "/items": {
      "get": {
        "operationId": "list",
        "parameters": [{"name": "q", "in": "query", "schema": {"type": "string"}}],
        "responses": {"200": {"description": "OK", "content": {"application/json": {"schema": {
          "type": "array", "items": {"$ref": "#/components/schemas/Item"}}}}}}
      },
      "post": {
        "operationId": "create",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {"201": {"description": "Created"}, "400": {"description": "Bad"}}
      }
    },
    "/other": {
      "get": {"operationId": "other", "responses": {"200": {"description": "OK"}}}
    }
  },
  "components": {"schemas": {
    "Item": {"type": "object", "required": ["sku"], "properties": {
      "sku": {"type": "string"}, "name": {"type": "string"}}}
  }}
}`

func TestHashIgnoresServerAddress(t *testing.T) {
	a := parse(t, baseSpec)
	b := parse(t, strings.Replace(baseSpec, "http://a.internal", "http://b.internal", 1))

	ignored, err := a.Normalized(HashOptions{IgnoreServers: true}).Hash()
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.Normalized(HashOptions{IgnoreServers: true}).Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ignored != other {
		t.Error("moving a service to another host changed the contract hash; " +
			"every redeploy behind a new address would show up as an API change")
	}

	kept, _ := a.Normalized(HashOptions{}).Hash()
	keptOther, _ := b.Normalized(HashOptions{}).Hash()
	if kept == keptOther {
		t.Error("with ignoreServers=false the server URL must affect the hash")
	}
}

// The regression this whole normalisation exists for: a service that
// regenerates its spec on every build, with no change to the API.
func TestRegeneratedSpecIsNotAChange(t *testing.T) {
	const withVersionStamp = `{
      "openapi": "3.0.3",
      "info": {"title": "T", "version": "1.0.0-build.417"},
      "paths": {"/x": {"post": {"operationId": "x",
        "parameters": [
          {"name": "a", "in": "query", "schema": {"type": "string"}},
          {"name": "b", "in": "query", "schema": {"type": "string"}}],
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/R"}}}},
        "responses": {"200": {"description": "OK"}}}}},
      "components": {"schemas": {"R": {"type": "object", "required": ["alpha", "beta"],
        "properties": {"alpha": {"type": "string"}, "beta": {"type": "string"}},
        "enum": null}}}
    }`
	// Same contract, rebuilt: arrays came out of the generator in another
	// order and the build number moved on.
	rebuilt := strings.NewReplacer(
		`"required": ["alpha", "beta"]`, `"required": ["beta", "alpha"]`,
		`{"name": "a", "in": "query", "schema": {"type": "string"}},
          {"name": "b", "in": "query", "schema": {"type": "string"}}`,
		`{"name": "b", "in": "query", "schema": {"type": "string"}},
          {"name": "a", "in": "query", "schema": {"type": "string"}}`,
		`1.0.0-build.417`, `1.0.0-build.418`,
	).Replace(withVersionStamp)

	opts := HashOptions{IgnoreServers: true, IgnoreFields: []string{"info.version"}}
	before := parse(t, withVersionStamp).Normalized(opts)
	after := parse(t, rebuilt).Normalized(opts)

	hashBefore, _ := before.Hash()
	hashAfter, _ := after.Hash()
	if hashBefore != hashAfter {
		t.Error("a rebuild with no API change produced a new revision")
	}
	if diff := Compare(before, after); !diff.Empty() {
		t.Errorf("a rebuild with no API change produced a diff: %+v", diff.Changes)
	}
}

// Sorting must not swallow changes that matter.
func TestSortingStillDetectsRealChanges(t *testing.T) {
	opts := HashOptions{IgnoreServers: true}
	before := parse(t, baseSpec).Normalized(opts)
	after := parse(t, strings.Replace(baseSpec,
		`"required": ["sku"]`, `"required": ["sku", "name"]`, 1)).Normalized(opts)

	diff := Compare(before, after)
	if !diff.Breaking {
		t.Errorf("a newly required body field must still be breaking: %+v", diff.Changes)
	}
}

func TestIgnoreFieldsRemovesNestedPath(t *testing.T) {
	doc := parse(t, baseSpec).Normalized(HashOptions{IgnoreFields: []string{"info.version"}})
	if doc.APIVersion() != "" {
		t.Errorf("info.version survived removal: %q", doc.APIVersion())
	}
	if doc.Title() == "" {
		t.Error("removing info.version must not take the rest of info with it")
	}
}

// A change to a shared component is invisible when operation objects are
// compared literally, which is why operations are hashed with refs resolved.
func TestSharedSchemaChangeReachesEveryOperationUsingIt(t *testing.T) {
	before := parse(t, baseSpec)
	after := parse(t, strings.Replace(baseSpec,
		`"name": {"type": "string"}`,
		`"name": {"type": "string"}, "price": {"type": "number"}`, 1))

	diff := Compare(before, after)
	if diff.Modified != 2 {
		t.Fatalf("expected both operations that reference Item to be reported, got %d: %+v",
			diff.Modified, diff.Changes)
	}
	touched := map[string]bool{}
	for _, c := range diff.Changes {
		touched[c.Key] = true
	}
	if !touched["GET /items"] || !touched["POST /items"] {
		t.Errorf("wrong operations reported: %v", touched)
	}
	if touched["GET /other"] {
		t.Error("an operation that does not reference Item was reported as changed")
	}
}

func TestBreakingChangeDetection(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(string) string
		breaking bool
		code     string
	}{
		{
			name: "новый обязательный параметр",
			mutate: func(s string) string {
				return strings.Replace(s, `"name": "q", "in": "query"`, `"name": "q", "in": "query", "required": true`, 1)
			},
			breaking: true,
			code:     "param.required.on",
		},
		{
			name: "удалён эндпоинт",
			mutate: func(s string) string {
				return strings.Replace(s, `"/other": {
      "get": {"operationId": "other", "responses": {"200": {"description": "OK"}}}
    }`, `"/none": {}`, 1)
			},
			breaking: true,
			code:     "op.removed",
		},
		{
			name:     "убран успешный ответ",
			mutate:   func(s string) string { return strings.Replace(s, `"201": {"description": "Created"}, `, "", 1) },
			breaking: true,
			code:     "response.removed.success",
		},
		{
			name: "новое обязательное поле в теле",
			mutate: func(s string) string {
				return strings.Replace(s, `"required": ["sku"]`, `"required": ["sku", "name"]`, 1)
			},
			breaking: true,
			code:     "body.field.added.required",
		},
		{
			name:     "убран ответ об ошибке — не ломающее",
			mutate:   func(s string) string { return strings.Replace(s, `, "400": {"description": "Bad"}`, "", 1) },
			breaking: false,
			code:     "response.removed",
		},
		{
			name: "новый необязательный параметр — не ломающее",
			mutate: func(s string) string {
				return strings.Replace(s, `{"name": "q", "in": "query", "schema": {"type": "string"}}`, `{"name": "q", "in": "query", "schema": {"type": "string"}}, {"name": "page", "in": "query", "schema": {"type": "integer"}}`, 1)
			},
			breaking: false,
			code:     "param.added",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := parse(t, baseSpec)
			mutated := tc.mutate(baseSpec)
			if mutated == baseSpec {
				t.Fatal("test mutation did not change the spec")
			}
			diff := Compare(before, parse(t, mutated))

			if diff.Breaking != tc.breaking {
				t.Errorf("breaking = %v, want %v (%+v)", diff.Breaking, tc.breaking, diff.Changes)
			}
			var found bool
			for _, c := range diff.Changes {
				for _, d := range c.Details {
					if d.Code == tc.code {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("detail %q not reported; got %+v", tc.code, diff.Changes)
			}
		})
	}
}

func TestIdenticalSpecsProduceNoDiff(t *testing.T) {
	diff := Compare(parse(t, baseSpec), parse(t, baseSpec))
	if !diff.Empty() {
		t.Errorf("expected no differences, got %+v", diff)
	}
}

// A self-referencing schema (a tree, a linked list) must not send the resolver
// into an infinite descent.
func TestRecursiveSchemaTerminates(t *testing.T) {
	const recursive = `{
      "openapi": "3.0.3",
      "info": {"title": "T", "version": "1"},
      "paths": {"/tree": {"get": {"operationId": "tree", "responses": {"200": {
        "description": "OK", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}}}}}},
      "components": {"schemas": {"Node": {"type": "object", "properties": {
        "value": {"type": "string"},
        "children": {"type": "array", "items": {"$ref": "#/components/schemas/Node"}}}}}}
    }`

	done := make(chan *Index, 1)
	go func() { done <- parse(t, recursive).BuildIndex() }()

	select {
	case index := <-done:
		if len(index.Ops) != 1 {
			t.Errorf("expected 1 operation, got %d", len(index.Ops))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BuildIndex did not terminate on a recursive schema")
	}
}

func TestSwagger2BodyParameterIsUnderstood(t *testing.T) {
	const swagger2 = `{
      "swagger": "2.0",
      "info": {"title": "T", "version": "1"},
      "paths": {"/pay": {"post": {
        "operationId": "pay",
        "parameters": [{"name": "body", "in": "body", "required": true, "schema": {"$ref": "#/definitions/Req"}}],
        "responses": {"201": {"description": "Created"}}}}},
      "definitions": {"Req": {"type": "object", "required": ["amount"], "properties": {
        "amount": {"type": "number"}, "currency": {"type": "string"}}}}
    }`

	index := parse(t, swagger2).BuildIndex()
	op := index.Ops["POST /pay"]
	if op == nil {
		t.Fatal("operation not indexed")
	}
	if !op.BodyRequired {
		t.Error("in:body parameter with required:true should mark the body as required")
	}
	if len(op.BodyFields) != 1 || op.BodyFields[0] != "amount" {
		t.Errorf("required body fields = %v, want [amount]", op.BodyFields)
	}
}
