// Package openapi keeps OpenAPI documents as generic JSON-compatible data
// instead of binding to a typed model. Services in the wild emit Swagger 2.0,
// OpenAPI 3.0 and 3.1, in JSON and YAML; for aggregating, hashing and diffing
// we only need structural access, so a schema-agnostic representation is both
// simpler and more tolerant of whatever a framework decides to produce.
package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Doc is a parsed OpenAPI document.
type Doc struct {
	Root map[string]any
}

// Parse accepts JSON or YAML and verifies the payload actually looks like an
// OpenAPI document. Discovery relies on that check to tell a real spec from an
// HTML error page or an unrelated JSON endpoint.
func Parse(raw []byte) (*Doc, error) {
	var v any
	jsonErr := json.Unmarshal(raw, &v)
	if jsonErr != nil {
		if yamlErr := yaml.Unmarshal(raw, &v); yamlErr != nil {
			return nil, fmt.Errorf("payload is neither JSON (%v) nor YAML (%v)", jsonErr, yamlErr)
		}
	}
	root, ok := normalizeKeys(v).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document root is %T, expected an object", v)
	}
	d := &Doc{Root: root}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Doc) validate() error {
	if d.SpecVersion() == "" {
		return fmt.Errorf(`missing "openapi"/"swagger" version field`)
	}
	for _, k := range []string{"paths", "webhooks", "components", "definitions"} {
		if _, ok := d.Root[k]; ok {
			return nil
		}
	}
	return fmt.Errorf(`no "paths", "webhooks" or "components" section`)
}

// SpecVersion returns the OpenAPI/Swagger version of the document itself
// ("3.0.1", "2.0"), not the version of the API it describes.
func (d *Doc) SpecVersion() string {
	for _, k := range []string{"openapi", "swagger"} {
		if s, ok := d.Root[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// IsSwagger2 reports whether this is a legacy Swagger 2.0 document, which uses
// host/basePath/schemes instead of the servers array.
func (d *Doc) IsSwagger2() bool {
	return strings.HasPrefix(d.SpecVersion(), "2.")
}

// Title and APIVersion come from the info block and are what humans recognise
// a service by in the portal list.
func (d *Doc) Title() string      { return d.infoString("title") }
func (d *Doc) APIVersion() string { return d.infoString("version") }

func (d *Doc) infoString(key string) string {
	info, _ := d.Root["info"].(map[string]any)
	s, _ := info[key].(string)
	return s
}

// Clone returns a deep copy, so that callers can rewrite a served copy of the
// spec (server URLs, for instance) without touching the stored original.
func (d *Doc) Clone() *Doc {
	return &Doc{Root: deepCopy(d.Root).(map[string]any)}
}

// JSON serialises the document. encoding/json sorts map keys, so the output is
// canonical: the same logical document always produces the same bytes.
func (d *Doc) JSON() ([]byte, error) { return json.Marshal(d.Root) }

// HashOptions controls what is considered part of the contract.
type HashOptions struct {
	// IgnoreServers drops the fields that carry the deployment address rather
	// than the contract, so a redeploy behind a new host is not a change.
	IgnoreServers bool
	// IgnoreFields are dotted paths ("info.version", "info.description")
	// removed before hashing, for generators that stamp build metadata into
	// the spec.
	IgnoreFields []string
}

// unorderedArrays are the array-valued keys whose element order carries no
// meaning in OpenAPI.
//
// This matters more than it sounds. Plenty of generators build these by
// iterating a hash map or reflecting over class members, so the order is
// unstable between builds. Without sorting them, a service that regenerates
// its spec on every build produces a new "revision" every time, with a diff
// that says only "schema changed" — and the change feed becomes noise nobody
// reads.
var unorderedArrays = map[string]bool{
	"required":   true,
	"enum":       true,
	"tags":       true,
	"security":   true,
	"parameters": true,
	"schemes":    true,
}

// Normalized returns a copy reduced to what the portal treats as the contract:
// deployment addresses and ignored fields removed, order-insensitive arrays
// sorted. Both the revision hash and the operation index are built from it, so
// that a cosmetic regeneration shows up neither as a new revision nor as a
// changed endpoint.
func (d *Doc) Normalized(opts HashOptions) *Doc {
	root := deepCopy(d.Root).(map[string]any)
	if opts.IgnoreServers {
		delete(root, "servers")
		delete(root, "host")
		delete(root, "basePath")
		delete(root, "schemes")
	}
	for _, path := range opts.IgnoreFields {
		deletePath(root, strings.Split(path, "."))
	}
	sortUnordered(root)
	return &Doc{Root: root}
}

// Hash identifies a spec revision. Call it on the result of Normalized.
func (d *Doc) Hash() (string, error) {
	b, err := json.Marshal(d.Root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func deletePath(node map[string]any, path []string) {
	if len(path) == 0 || node == nil {
		return
	}
	if len(path) == 1 {
		delete(node, path[0])
		return
	}
	child, _ := node[path[0]].(map[string]any)
	deletePath(child, path[1:])
}

// sortUnordered walks the document sorting the arrays listed in
// unorderedArrays. Elements are ordered by their canonical JSON, which is total
// and works whether the array holds strings, objects or a mix.
func sortUnordered(node any) {
	switch t := node.(type) {
	case map[string]any:
		for key, value := range t {
			if arr, ok := value.([]any); ok && unorderedArrays[key] {
				sortByJSON(arr)
			}
			sortUnordered(value)
		}
	case []any:
		for _, v := range t {
			sortUnordered(v)
		}
	}
}

func sortByJSON(arr []any) {
	// The sort key travels with its element: sorting the slice directly while
	// keeping keys in a parallel structure would break the pairing on the
	// first swap.
	type keyed struct {
		key   string
		value any
	}
	items := make([]keyed, len(arr))
	for i, v := range arr {
		b, err := json.Marshal(v)
		if err != nil {
			return // leave the order alone rather than sort by a partial key
		}
		items[i] = keyed{string(b), v}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	for i, it := range items {
		arr[i] = it.value
	}
}

// SetProxyServers points the document at the portal's proxy so that "Try it
// out" in the viewer reaches the upstream service through us. Swagger 2.0 has
// no servers array and needs the host/basePath treatment instead.
func (d *Doc) SetProxyServers(prefix, description string) {
	if d.IsSwagger2() {
		delete(d.Root, "host")
		delete(d.Root, "schemes")
		d.Root["basePath"] = prefix
		return
	}
	d.Root["servers"] = []any{
		map[string]any{"url": prefix, "description": description},
	}
}

// StripServers removes every address the document names.
//
// It is used when the proxy is off. What is left then is a document to read,
// not to execute: there is no address the portal is willing to send a request
// to, and a viewer that shows one invites the reader to fire it straight from
// their browser — past the portal, at an address that is usually reachable
// only from inside the cluster.
//
// It also closes a leak. Specifications routinely declare their own internal
// address in `servers`, and the specification endpoint is public. Everything
// else about the cluster is redacted for anonymous readers; this was the one
// place the address came back.
func (d *Doc) StripServers() {
	if d.IsSwagger2() {
		delete(d.Root, "host")
		delete(d.Root, "schemes")
		delete(d.Root, "basePath")
		return
	}
	delete(d.Root, "servers")
}

func normalizeKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeKeys(val)
		}
		return out
	case map[any]any:
		// yaml.v3 only produces this for non-string keys, which OpenAPI does
		// not use — but a malformed spec should degrade, not crash.
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeKeys(val)
		}
		return out
	default:
		return v
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopy(val)
		}
		return out
	default:
		return v
	}
}
