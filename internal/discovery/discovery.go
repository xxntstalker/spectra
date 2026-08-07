// Package discovery locates the machine-readable OpenAPI document of a service.
//
// The registry only knows a base URL. What sits at /swagger is an HTML page,
// not a spec, and every framework publishes the actual JSON somewhere else. So
// we try the well-known paths first and, failing that, read the docs page and
// extract the URL it feeds to its own viewer.
package discovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"swaggeragg/internal/openapi"
)

// candidatePaths are the conventional spec locations, ordered by how much of a
// typical estate each one covers.
//
// The list is long on purpose. Probing is a one-off cost — the resolved URL is
// cached in the database — and a refused connection comes back instantly, so a
// service that is simply down does not pay for the length of this list. What
// the length buys is that most services need no configuration at all.
var candidatePaths = []string{
	"/v3/api-docs",             // Spring Boot + springdoc
	"/openapi.json",            // FastAPI, Flask-smorest, Connexion, самописные
	"/swagger/v1/swagger.json", // ASP.NET Core + Swashbuckle
	"/swagger.json",            // drf-yasg, Flask-RESTX, go-swagger, Dropwizard
	"/v2/api-docs",             // Spring Boot + springfox (Swagger 2.0)
	"/api-json",                // NestJS
	"/swagger/doc.json",        // Go + swaggo
	"/openapi.yaml",            // spec-first сервисы, отдающие YAML

	// JVM, помимо Spring
	"/q/openapi", // Quarkus
	"/openapi",   // MicroProfile: Helidon, Open Liberty, Payara, WildFly

	// .NET
	"/openapi/v1.json",        // ASP.NET Core 9+, встроенный Microsoft.AspNetCore.OpenApi
	"/api/specification.json", // NSwag

	// Python
	"/api/schema/",         // Django REST Framework + drf-spectacular
	"/schema/openapi.json", // Litestar
	"/api/openapi.json",    // Flask-smorest под префиксом

	// PHP
	"/docs/api-docs.json", // Laravel + l5-swagger (darkaonline)
	"/docs.openapi",       // Laravel + Scribe
	"/api/docs.json",      // API Platform (Symfony и Laravel)
	"/api/doc.json",       // Symfony + NelmioApiDocBundle

	// Node, помимо NestJS
	"/api-docs.json",      // Express + swagger-ui-express
	"/documentation/json", // Fastify + @fastify/swagger
	"/docs/json",          // Fastify под префиксом /docs
	"/api-docs-json",      // NestJS с нестандартным префиксом

	// Ruby, Rust
	"/api-docs/v1/swagger.json", // Rails + rswag
	"/api-docs/v1/swagger.yaml", // Rails + rswag, вариант с YAML
	"/swagger_doc.json",         // Grape + grape-swagger
	"/api-docs/openapi.json",    // Rust + utoipa

	// Общие варианты напоследок
	"/api-docs",
	"/docs/openapi.json",
	"/openapi.yml",
	"/.well-known/openapi.json",
}

// docPagePaths are HTML documentation pages worth scraping when no
// conventional path answered. Every generator has to tell the browser where
// the spec lives, so the answer is always somewhere in the page.
var docPagePaths = []string{
	"/swagger",
	"/swagger/index.html",
	"/swagger-ui/index.html",
	"/swagger-ui.html",
	"/swagger-ui",
	"/q/swagger-ui",      // Quarkus
	"/docs",              // FastAPI, Scribe, Fastify
	"/api/documentation", // Laravel + l5-swagger
	"/api/doc",           // Symfony + NelmioApiDocBundle
	"/api/docs",          // API Platform
	"/documentation",     // Fastify
	"/schema/swagger",    // Litestar
	"/api-docs",
	"/redoc",
}

// Sources of a resolved spec URL, recorded so the UI can show how we found it.
const (
	SourceConfig      = "config"
	SourceProbe       = "probe"
	SourceSwaggerHTML = "swagger-html"
)

type Client struct {
	HTTP    *http.Client
	Headers map[string]string
	// UserAgent identifies the portal in upstream access logs.
	UserAgent string
}

type Response struct {
	Body         []byte
	Status       int
	ContentType  string
	ETag         string
	LastModified string
	NotModified  bool
}

// Get fetches a URL, passing conditional-request headers when we have them.
// Most services answer 304 for an unchanged spec, which keeps a five-minute
// poll across a few dozen services essentially free.
func (c *Client) Get(ctx context.Context, target, etag, lastModified string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml, text/html;q=0.5, */*;q=0.1")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{
		Status:       resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
	}
	if out.NotModified {
		return out, nil
	}
	// 8 MiB is far above any real spec and keeps a misconfigured URL from
	// pulling an unbounded stream into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	out.Body = body
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// Result describes where a service's spec was found and what it contained.
type Result struct {
	SpecURL string
	Source  string
	Doc     *openapi.Doc
	Raw     []byte
	Resp    *Response
	// Tried lists the URLs probed, for diagnosing a service we could not read.
	Tried []string
	// Reached records whether anything answered at all. It separates "this
	// service is down" from "this service is up but publishes no spec" — two
	// very different problems that would otherwise look identical.
	Reached bool
}

// Discover resolves the spec for a service. configuredSpecURL may be absolute
// or a path relative to baseURL; when empty, probing and scraping take over.
func (c *Client) Discover(ctx context.Context, baseURL, configuredSpecURL string) (*Result, error) {
	res := &Result{}

	if configuredSpecURL != "" {
		target, err := resolveAgainst(baseURL, configuredSpecURL)
		if err != nil {
			return res, err
		}
		res.Tried = append(res.Tried, target)
		doc, raw, resp, err := c.fetchSpec(ctx, target)
		res.Reached = res.Reached || resp != nil
		if err != nil {
			return res, fmt.Errorf("configured specUrl %s: %w", target, err)
		}
		res.SpecURL, res.Source, res.Doc, res.Raw, res.Resp = target, SourceConfig, doc, raw, resp
		return res, nil
	}

	// The documentation page comes first, before any guessing.
	//
	// On real infrastructure the JSON lives at addresses no convention covers,
	// but /swagger is served almost everywhere — and that page has to tell its
	// own viewer where the spec is. Reading it turns guessing into looking up
	// the answer, and it costs two requests instead of thirty misses.
	for _, path := range docPagePaths {
		if ctx.Err() != nil {
			return res, c.deadlineError(res)
		}
		pageURL := baseURL + path
		res.Tried = append(res.Tried, pageURL)
		resp, err := c.Get(ctx, pageURL, "", "")
		res.Reached = res.Reached || resp != nil
		if err != nil || len(resp.Body) == 0 {
			continue
		}
		for _, target := range c.specURLsFromDocPage(ctx, pageURL, resp.Body) {
			res.Tried = append(res.Tried, target)
			doc, raw, specResp, err := c.fetchSpec(ctx, target)
			res.Reached = res.Reached || specResp != nil
			if err != nil {
				continue
			}
			res.SpecURL, res.Source, res.Doc, res.Raw, res.Resp = target, SourceSwaggerHTML, doc, raw, specResp
			return res, nil
		}
	}

	for _, path := range candidatePaths {
		// Once the budget is gone every remaining request fails instantly, so
		// carrying on would only inflate the "tried" count with probes that
		// never reached the network.
		if ctx.Err() != nil {
			return res, c.deadlineError(res)
		}
		target := baseURL + path
		res.Tried = append(res.Tried, target)
		doc, raw, resp, err := c.fetchSpec(ctx, target)
		res.Reached = res.Reached || resp != nil
		if err != nil {
			continue
		}
		res.SpecURL, res.Source, res.Doc, res.Raw, res.Resp = target, SourceProbe, doc, raw, resp
		return res, nil
	}

	if !res.Reached {
		return res, fmt.Errorf("service did not respond on any of %d probed URLs", len(res.Tried))
	}
	return res, fmt.Errorf("service responded but published no OpenAPI document (%d locations tried)", len(res.Tried))
}

// deadlineError says plainly that the search was cut short, so the message
// cannot contradict the status derived from the same condition.
func (c *Client) deadlineError(res *Result) error {
	return fmt.Errorf("ran out of time after probing %d of %d URLs; raise poll.budget",
		len(res.Tried), len(candidatePaths)+len(docPagePaths))
}

// fetchSpec returns the response even when it failed, so the caller can tell a
// refused connection from an unusable answer.
func (c *Client) fetchSpec(ctx context.Context, target string) (*openapi.Doc, []byte, *Response, error) {
	resp, err := c.Get(ctx, target, "", "")
	if err != nil {
		return nil, nil, resp, err
	}
	doc, err := openapi.Parse(resp.Body)
	if err != nil {
		return nil, nil, resp, err
	}
	return doc, resp.Body, resp, nil
}

var (
	reConfigURL = regexp.MustCompile(`configUrl\s*[:=]\s*["']([^"']+)["']`)
	reDataURL   = regexp.MustCompile(`data-url\s*=\s*["']([^"']+)["']`)
	reSpecURL   = regexp.MustCompile(`spec-url\s*=\s*["']([^"']+)["']`)
	reURLField  = regexp.MustCompile(`["']?url["']?\s*:\s*["']([^"']+)["']`)
	// Redoc and Elements pass the address as an attribute; Stoplight uses
	// apiDescriptionUrl. A plain href to a .json/.yaml on the page is the
	// last resort — it catches hand-written index pages that just link the
	// file, which no viewer-specific pattern would find.
	reAttrURL = regexp.MustCompile(`(?i)(?:apiDescriptionUrl|swagger-url|openapi-url|src)\s*=\s*["']([^"']+)["']`)
	reHref    = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+\.(?:json|ya?ml))["']`)
)

// specURLsFromDocPage extracts candidate spec URLs from a documentation page.
// Swagger UI, Redoc, Scalar and friends all have to tell the browser where the
// spec lives, so the answer is always in the page — just in a different shape
// for each of them.
func (c *Client) specURLsFromDocPage(ctx context.Context, pageURL string, body []byte) []string {
	html := string(body)
	var found []string

	for _, re := range []*regexp.Regexp{reDataURL, reSpecURL, reAttrURL, reURLField, reHref} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			found = append(found, m[1])
		}
	}

	// Springdoc's Swagger UI does not name the spec directly: it points at a
	// config endpoint that lists the available specs.
	for _, m := range reConfigURL.FindAllStringSubmatch(html, -1) {
		configURL, err := resolveAgainst(pageURL, m[1])
		if err != nil {
			continue
		}
		resp, err := c.Get(ctx, configURL, "", "")
		if err != nil {
			continue
		}
		for _, re := range []*regexp.Regexp{reURLField} {
			for _, cm := range re.FindAllStringSubmatch(string(resp.Body), -1) {
				found = append(found, cm[1])
			}
		}
	}

	return c.filterCandidates(pageURL, found)
}

// filterCandidates keeps plausible, same-origin URLs and drops duplicates.
//
// The same-origin rule matters more than it looks: an untouched Swagger UI
// build ships with the public petstore spec as its default URL, and without
// this check we would happily aggregate petstore instead of the service.
func (c *Client) filterCandidates(pageURL string, raw []string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, candidate := range raw {
		if !looksLikeSpecURL(candidate) {
			continue
		}
		resolved, err := base.Parse(candidate)
		if err != nil {
			continue
		}
		if resolved.Host != base.Host {
			continue
		}
		s := resolved.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func looksLikeSpecURL(s string) bool {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, ".html") || strings.HasSuffix(s, ".css") || strings.HasSuffix(s, ".js") {
		return false
	}
	for _, marker := range []string{"api-docs", "openapi", "swagger.json", "doc.json", "api-json", ".json", ".yaml", ".yml"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func resolveAgainst(base, ref string) (string, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref, nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := b.Parse(ref)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}
