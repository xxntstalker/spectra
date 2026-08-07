package openapi

import "testing"

// Services in one estate are rarely on the same OpenAPI version, and a service
// that upgrades its framework changes version under the portal's feet. These
// tests pin down what the portal does across 2.0, 3.0 and 3.1.

// The same API, described in Swagger 2.0 and in OpenAPI 3.0.
const ordersSwagger2 = `{"swagger":"2.0","info":{"title":"Orders","version":"1.0.0"},
 "host":"orders.internal","basePath":"/","schemes":["https"],
 "consumes":["application/json"],"produces":["application/json"],
 "paths":{"/orders":{
   "get":{"operationId":"list","parameters":[{"name":"status","in":"query","required":false,"type":"string"}],
     "responses":{"200":{"description":"OK","schema":{"type":"array","items":{"$ref":"#/definitions/Order"}}}}},
   "post":{"operationId":"create","parameters":[{"name":"body","in":"body","required":true,"schema":{"$ref":"#/definitions/NewOrder"}}],
     "responses":{"201":{"description":"Created","schema":{"$ref":"#/definitions/Order"}}}}}},
 "definitions":{
   "Order":{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"total":{"type":"number"}}},
   "NewOrder":{"type":"object","required":["customerId"],"properties":{"customerId":{"type":"string"}}}}}`

const ordersOpenAPI3 = `{"openapi":"3.0.3","info":{"title":"Orders","version":"1.0.0"},
 "servers":[{"url":"https://orders.internal/"}],
 "paths":{"/orders":{
   "get":{"operationId":"list","parameters":[{"name":"status","in":"query","required":false,"schema":{"type":"string"}}],
     "responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"array","items":{"$ref":"#/components/schemas/Order"}}}}}}},
   "post":{"operationId":"create","requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/NewOrder"}}}},
     "responses":{"201":{"description":"Created","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Order"}}}}}}}},
 "components":{"schemas":{
   "Order":{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"total":{"type":"number"}}},
   "NewOrder":{"type":"object","required":["customerId"],"properties":{"customerId":{"type":"string"}}}}}}`

func TestAllFormatsAreParsedAndIndexed(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		version string
		ops     int
	}{
		{"Swagger 2.0", ordersSwagger2, "2.0", 2},
		{"OpenAPI 3.0", ordersOpenAPI3, "3.0.3", 2},
		{
			"OpenAPI 3.1", `{"openapi":"3.1.0","info":{"title":"T","version":"1"},
			  "paths":{"/x":{"get":{"responses":{"200":{"description":"OK"}}}}}}`,
			"3.1.0", 1,
		},
		{
			// YAML is as common as JSON for spec-first services.
			"YAML", "openapi: 3.0.3\ninfo:\n  title: T\n  version: '1'\npaths:\n  /x:\n    get:\n      responses:\n        '200':\n          description: OK\n",
			"3.0.3", 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := parse(t, tc.raw)
			if doc.SpecVersion() != tc.version {
				t.Errorf("version = %q, want %q", doc.SpecVersion(), tc.version)
			}
			if got := len(doc.BuildIndex().Ops); got != tc.ops {
				t.Errorf("indexed %d operations, want %d", got, tc.ops)
			}
		})
	}
}

// Upgrading the framework rewrites the whole document, but the API is the same.
// The description of it changes shape, so some churn is unavoidable — what must
// not happen is a false "breaking" verdict scaring everyone off the upgrade.
func TestMigrationFromSwagger2IsNotReportedAsBreaking(t *testing.T) {
	opts := HashOptions{IgnoreServers: true}
	before := parse(t, ordersSwagger2).Normalized(opts)
	after := parse(t, ordersOpenAPI3).Normalized(opts)

	diff := Compare(before, after)
	if diff.Breaking {
		t.Errorf("2.0 → 3.0 migration flagged as breaking: %+v", diff.Changes)
	}
	if diff.Removed != 0 || diff.Added != 0 {
		t.Errorf("migration invented added/removed endpoints: +%d -%d", diff.Added, diff.Removed)
	}
	var noted bool
	for _, n := range diff.Notes {
		if n.Code == "spec.openapi.changed" {
			noted = true
		}
	}
	if !noted {
		t.Error("the format change itself should be reported as a note")
	}
}

// The payload is a parameter in 2.0 and its own section in 3.0. Both have to
// land in the same place, or the two formats cannot be compared at all.
func TestRequestPayloadIsComparableAcrossFormats(t *testing.T) {
	v2 := parse(t, ordersSwagger2).BuildIndex().Ops["POST /orders"]
	v3 := parse(t, ordersOpenAPI3).BuildIndex().Ops["POST /orders"]

	for name, op := range map[string]*Operation{"2.0": v2, "3.0": v3} {
		if !op.BodyRequired {
			t.Errorf("%s: required request body not detected", name)
		}
		if len(op.BodyFields) != 1 || op.BodyFields[0] != "customerId" {
			t.Errorf("%s: body fields = %v, want [customerId]", name, op.BodyFields)
		}
		for _, p := range op.Params {
			if p.In == "body" || p.In == "formData" {
				t.Errorf("%s: payload counted twice — once as %s parameter %q, once as the body",
					name, p.In, p.Name)
			}
		}
	}
}

func TestSwagger2FormDataBecomesBodyFields(t *testing.T) {
	const spec = `{"swagger":"2.0","info":{"title":"T","version":"1"},
	 "paths":{"/upload":{"post":{"operationId":"up","consumes":["multipart/form-data"],
	   "parameters":[
	     {"name":"file","in":"formData","required":true,"type":"file"},
	     {"name":"comment","in":"formData","required":false,"type":"string"}],
	   "responses":{"201":{"description":"Created"}}}}}}`

	op := parse(t, spec).BuildIndex().Ops["POST /upload"]
	if !op.BodyRequired {
		t.Error("a required form field should make the payload required")
	}
	if len(op.BodyFields) != 1 || op.BodyFields[0] != "file" {
		t.Errorf("body fields = %v, want [file]", op.BodyFields)
	}
}

// 3.1 moved service-initiated calls into their own section. They are part of
// the contract, so a change in one must not pass unnoticed.
func TestWebhooksAreIndexedAndDiffed(t *testing.T) {
	const before = `{"openapi":"3.1.0","info":{"title":"T","version":"1"},
	 "paths":{"/ping":{"get":{"operationId":"ping","responses":{"200":{"description":"OK"}}}}},
	 "webhooks":{"orderCreated":{"post":{"operationId":"hook","responses":{"200":{"description":"OK"}}}}}}`
	const after = `{"openapi":"3.1.0","info":{"title":"T","version":"1"},
	 "paths":{"/ping":{"get":{"operationId":"ping","responses":{"200":{"description":"OK"}}}}}}`

	index := parse(t, before).BuildIndex()
	if len(index.Ops) != 2 {
		t.Fatalf("indexed %d operations, want 2 (one path, one webhook): %v", len(index.Ops), index.Keys())
	}
	if index.Ops["POST webhook:orderCreated"] == nil {
		t.Fatalf("webhook not indexed under its own namespace: %v", index.Keys())
	}

	opts := HashOptions{IgnoreServers: true}
	diff := Compare(parse(t, before).Normalized(opts), parse(t, after).Normalized(opts))
	if diff.Removed != 1 || !diff.Breaking {
		t.Errorf("dropping a webhook should be a breaking removal: %+v", diff)
	}
}

// A revision is recorded whenever the hash moves. If the diff cannot explain
// why, the UI shows an empty change card.
func TestChangesOutsidePathsAreExplained(t *testing.T) {
	const before = `{"openapi":"3.0.3","info":{"title":"T","version":"1"},
	 "paths":{"/x":{"get":{"responses":{"200":{"description":"OK"}}}}},
	 "security":[{"apiKey":[]}]}`
	const after = `{"openapi":"3.0.3","info":{"title":"T","version":"1"},
	 "paths":{"/x":{"get":{"responses":{"200":{"description":"OK"}}}}},
	 "security":[{"oauth2":["read"]}]}`

	opts := HashOptions{IgnoreServers: true}
	first, second := parse(t, before).Normalized(opts), parse(t, after).Normalized(opts)

	hashBefore, _ := first.Hash()
	hashAfter, _ := second.Hash()
	if hashBefore == hashAfter {
		t.Fatal("test premise broken: the specs should hash differently")
	}

	diff := Compare(first, second)
	if diff.Empty() {
		t.Error("a recorded revision produced no explanation at all — the change card would be blank")
	}
}

// Swagger 2.0 has no servers array; the address lives in host/basePath/schemes.
func TestServerFieldsAreIgnoredInBothFormats(t *testing.T) {
	moved := `{"swagger":"2.0","info":{"title":"Orders","version":"1.0.0"},
	 "host":"orders-new.internal","basePath":"/api","schemes":["http"],
	 "consumes":["application/json"],"produces":["application/json"],
	 "paths":{"/orders":{"get":{"operationId":"list","responses":{"200":{"description":"OK"}}}}}}`
	same := `{"swagger":"2.0","info":{"title":"Orders","version":"1.0.0"},
	 "host":"orders.internal","basePath":"/","schemes":["https"],
	 "consumes":["application/json"],"produces":["application/json"],
	 "paths":{"/orders":{"get":{"operationId":"list","responses":{"200":{"description":"OK"}}}}}}`

	opts := HashOptions{IgnoreServers: true}
	a, _ := parse(t, moved).Normalized(opts).Hash()
	b, _ := parse(t, same).Normalized(opts).Hash()
	if a != b {
		t.Error("moving a Swagger 2.0 service to another host changed its contract hash")
	}
}
