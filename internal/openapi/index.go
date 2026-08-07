package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// httpMethods are the keys of a path item that denote an operation. Anything
// else there ("parameters", "summary", "servers", vendor extensions) is not.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Param is the identity of a parameter as a caller experiences it.
type Param struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

// Operation is one endpoint, flattened for comparison.
//
// Hash covers the operation with every local $ref inlined. That is the point of
// this whole file: a change to a shared component schema is invisible if you
// only compare the operation objects literally, but it is a real API change for
// every endpoint that references it, and the resolved hash catches it.
type Operation struct {
	Key          string   `json:"key"`
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	Summary      string   `json:"summary,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Deprecated   bool     `json:"deprecated,omitempty"`
	Hash         string   `json:"hash"`
	Params       []Param  `json:"params,omitempty"`
	Responses    []string `json:"responses,omitempty"`
	BodyRequired bool     `json:"bodyRequired,omitempty"`
	BodyFields   []string `json:"bodyFields,omitempty"`
}

// Index is the set of operations in a document, keyed by "METHOD /path".
type Index struct {
	Ops map[string]*Operation
}

// Keys returns operation keys in stable order.
func (ix *Index) Keys() []string {
	keys := make([]string, 0, len(ix.Ops))
	for k := range ix.Ops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// BuildIndex flattens the document into comparable operations.
func (d *Doc) BuildIndex() *Index {
	r := &resolver{root: d.Root}
	ix := &Index{Ops: map[string]*Operation{}}

	paths, _ := d.Root["paths"].(map[string]any)
	ix.addPathItems(r, paths, "")

	// OpenAPI 3.1 moved callbacks the service initiates into their own
	// top-level section. They are as much a part of the contract as paths —
	// something has to receive them — so they belong in the index too. The
	// prefix keeps a webhook named like a route from colliding with one.
	webhooks, _ := d.Root["webhooks"].(map[string]any)
	ix.addPathItems(r, webhooks, "webhook:")

	return ix
}

func (ix *Index) addPathItems(r *resolver, items map[string]any, prefix string) {
	for path, rawItem := range items {
		item, ok := r.resolve(rawItem, map[string]bool{}).(map[string]any)
		if !ok {
			continue
		}
		sharedParams, _ := item["parameters"].([]any)
		for _, method := range httpMethods {
			rawOp, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			op := buildOperation(strings.ToUpper(method), prefix+path, rawOp, sharedParams)
			ix.Ops[op.Key] = op
		}
	}
}

func buildOperation(method, path string, resolved map[string]any, sharedParams []any) *Operation {
	op := &Operation{
		Key:    method + " " + path,
		Method: method,
		Path:   path,
	}
	op.Summary, _ = resolved["summary"].(string)
	if op.Summary == "" {
		op.Summary, _ = resolved["operationId"].(string)
	}
	op.Deprecated, _ = resolved["deprecated"].(bool)
	for _, t := range toSlice(resolved["tags"]) {
		if s, ok := t.(string); ok {
			op.Tags = append(op.Tags, s)
		}
	}

	// Path-level parameters apply to every operation unless the operation
	// redeclares the same (name, in) pair.
	own, _ := resolved["parameters"].([]any)
	merged := map[string]Param{}
	for _, list := range [][]any{sharedParams, own} {
		for _, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			in, _ := m["in"].(string)
			if name == "" {
				continue
			}
			// Swagger 2.0 models the request payload as a parameter; OpenAPI 3
			// gives it its own section. Both end up in BodyRequired/BodyFields
			// below, so keeping them here as well would double-count them —
			// and would make a 2.0 → 3.0 migration look like every endpoint
			// lost a required parameter.
			if in == "body" || in == "formData" {
				continue
			}
			required, _ := m["required"].(bool)
			if in == "path" {
				required = true // path parameters are required by definition
			}
			merged[in+":"+name] = Param{Name: name, In: in, Required: required}
		}
	}
	for _, k := range sortedKeys(merged) {
		op.Params = append(op.Params, merged[k])
	}

	if body, ok := resolved["requestBody"].(map[string]any); ok {
		op.BodyRequired, _ = body["required"].(bool)
		op.BodyFields = requiredFields(firstMediaSchema(body))
	} else {
		op.BodyRequired, op.BodyFields = swagger2Body(append(append([]any{}, sharedParams...), own...))
	}

	if responses, ok := resolved["responses"].(map[string]any); ok {
		for code := range responses {
			op.Responses = append(op.Responses, code)
		}
		sort.Strings(op.Responses)
	}

	op.Hash = hashValue(resolved)
	return op
}

// swagger2Body reconstructs the request payload from Swagger 2.0 parameters,
// so that it lines up with what OpenAPI 3 states directly in requestBody.
// Without this alignment the two descriptions of the same API do not compare.
func swagger2Body(params []any) (required bool, fields []string) {
	for _, raw := range params {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch m["in"] {
		case "body":
			required, _ = m["required"].(bool)
			fields = append(fields, requiredFields(m["schema"])...)
		case "formData":
			// Each required form field is a required field of the payload,
			// which is how OpenAPI 3 expresses the same thing.
			if isRequired, _ := m["required"].(bool); isRequired {
				required = true
				if name, _ := m["name"].(string); name != "" {
					fields = append(fields, name)
				}
			}
		}
	}
	sort.Strings(fields)
	return required, fields
}

// firstMediaSchema picks a schema out of a requestBody's content map. Comparing
// one representative media type is enough to notice payload changes; services
// that offer several rarely diverge between them.
func firstMediaSchema(body map[string]any) any {
	content, ok := body["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, mt := range sortedKeys(content) {
		if media, ok := content[mt].(map[string]any); ok {
			if s, ok := media["schema"]; ok {
				return s
			}
		}
	}
	return nil
}

func requiredFields(schema any) []string {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range toSlice(m["required"]) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func hashValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resolver inlines local $refs. External refs are left as a marker rather than
// fetched: the portal reads specs from many unrelated services and must not
// start chasing arbitrary URLs on their behalf.
type resolver struct {
	root map[string]any
}

func (r *resolver) resolve(node any, active map[string]bool) any {
	switch t := node.(type) {
	case map[string]any:
		ref, isRef := t["$ref"].(string)
		if !isRef {
			out := make(map[string]any, len(t))
			for k, v := range t {
				out[k] = r.resolve(v, active)
			}
			return out
		}
		if active[ref] {
			// Recursive schema (a tree node referencing itself). Cut the walk
			// but keep the ref visible so the hash still reflects it.
			return map[string]any{"$circular": ref}
		}
		target, found := r.lookup(ref)
		if !found {
			return map[string]any{"$unresolved": ref}
		}
		active[ref] = true
		resolved := r.resolve(target, active)
		delete(active, ref)

		if len(t) == 1 {
			return resolved
		}
		// OpenAPI 3.1 allows keywords next to $ref; they override the target.
		merged, ok := resolved.(map[string]any)
		if !ok {
			merged = map[string]any{}
		}
		out := make(map[string]any, len(merged)+len(t))
		for k, v := range merged {
			out[k] = v
		}
		for k, v := range t {
			if k == "$ref" {
				continue
			}
			out[k] = r.resolve(v, active)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = r.resolve(v, active)
		}
		return out
	default:
		return node
	}
}

func (r *resolver) lookup(ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = r.root
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(token, "~1", "/")
		token = strings.ReplaceAll(token, "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[token]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
