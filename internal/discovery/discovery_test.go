package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const miniSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "Mini", "version": "1.0.0"},
  "paths": {"/ping": {"get": {"operationId": "ping", "responses": {"200": {"description": "OK"}}}}}
}`

func testClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func TestDiscoverFindsConventionalPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/swagger/v1/swagger.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(miniSpec))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	res, err := testClient().Discover(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Source != SourceProbe {
		t.Errorf("source = %q, want %q", res.Source, SourceProbe)
	}
	if !strings.HasSuffix(res.SpecURL, "/swagger/v1/swagger.json") {
		t.Errorf("spec URL = %q", res.SpecURL)
	}
}

// The realistic hard case: the spec sits at a path no convention covers, and
// the only pointer to it is inside the docs page.
func TestDiscoverReadsDocsPageAndIgnoresForeignURLs(t *testing.T) {
	const specPath = "/internal/contract.json"
	var served bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger":
			w.Header().Set("Content-Type", "text/html")
			// The petstore URL is the default that ships with Swagger UI and
			// is left behind in real deployments. Following it would make the
			// portal aggregate someone else's public API.
			w.Write([]byte(`<html><body>
				<script>SwaggerUIBundle({url: "https://petstore.swagger.io/v2/swagger.json"});</script>
				<redoc spec-url="` + specPath + `"></redoc>
			</body></html>`))
		case specPath:
			served = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(miniSpec))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	res, err := testClient().Discover(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.Source != SourceSwaggerHTML {
		t.Errorf("source = %q, want %q", res.Source, SourceSwaggerHTML)
	}
	if !served || !strings.HasSuffix(res.SpecURL, specPath) {
		t.Errorf("spec URL = %q, want the same-origin one", res.SpecURL)
	}
}

// Springdoc's page names a config endpoint rather than the spec itself.
func TestDiscoverFollowsConfigURL(t *testing.T) {
	const specPath = "/internal/openapi-v3"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body><script>
				SwaggerUIBundle({configUrl: "` + specPath + `/swagger-config"});
			</script></body></html>`))
		case specPath + "/swagger-config":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"urls":[{"url":"` + specPath + `","name":"api"}]}`))
		case specPath:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(miniSpec))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	res, err := testClient().Discover(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !strings.HasSuffix(res.SpecURL, specPath) {
		t.Errorf("spec URL = %q, want %q", res.SpecURL, specPath)
	}
}

// A JSON endpoint that is not a spec must not be mistaken for one.
func TestDiscoverRejectsNonSpecJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"UP","components":{"db":{"status":"UP"}}}`))
	}))
	defer server.Close()

	res, err := testClient().Discover(context.Background(), server.URL, "")
	if err == nil {
		t.Fatalf("expected failure, got spec at %q", res.SpecURL)
	}
	if !res.Reached {
		t.Error("the service answered, so Reached should be true")
	}
}

func TestDiscoverSeparatesOutageFromMissingSpec(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens on that port any more

	res, err := testClient().Discover(context.Background(), url, "")
	if err == nil {
		t.Fatal("expected failure against a closed port")
	}
	if res.Reached {
		t.Error("nothing answered, so Reached must be false — otherwise an " +
			"outage is reported as a missing spec")
	}
}

// The path lists are hand-maintained and grow as new frameworks appear; a
// duplicate would silently cost every unknown service an extra request.
func TestProbePathsAreWellFormed(t *testing.T) {
	for name, paths := range map[string][]string{
		"candidatePaths": candidatePaths,
		"docPagePaths":   docPagePaths,
	} {
		seen := map[string]bool{}
		for _, p := range paths {
			if !strings.HasPrefix(p, "/") {
				t.Errorf("%s: %q must start with a slash, it is joined onto baseUrl verbatim", name, p)
			}
			if seen[p] {
				t.Errorf("%s: %q listed twice", name, p)
			}
			seen[p] = true
		}
	}
}

func TestGetHonoursConditionalRequests(t *testing.T) {
	const etag = `"v1"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(miniSpec))
	}))
	defer server.Close()

	client := testClient()
	first, err := client.Get(context.Background(), server.URL+"/openapi.json", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ETag != etag {
		t.Fatalf("ETag = %q", first.ETag)
	}

	second, err := client.Get(context.Background(), server.URL+"/openapi.json", first.ETag, "")
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified {
		t.Error("expected 304 to be reported as NotModified")
	}
}

// A service that ignores conditional requests answers 200 with the whole
// document every time. Nothing breaks — the revision hash still suppresses
// duplicates — but the body crosses the network on every poll, so it had
// better be compressed.
func TestClientAcceptsCompression(t *testing.T) {
	var accepted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(miniSpec))
	}))
	defer server.Close()

	if _, err := testClient().Get(context.Background(), server.URL+"/openapi.json", "", ""); err != nil {
		t.Fatal(err)
	}
	// The transport adds this itself and decompresses transparently, as long as
	// we do not set Accept-Encoding ourselves. Setting it would silently turn
	// compression off and make every poll carry the full document.
	if !strings.Contains(accepted, "gzip") {
		t.Errorf("Accept-Encoding = %q, want gzip", accepted)
	}
}

// Conditional headers must only go out once there is something to validate
// against; sending an empty If-None-Match would be a malformed request.
func TestConditionalHeadersOnlyWhenKnown(t *testing.T) {
	var sawIfNoneMatch, sawIfModifiedSince bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawIfNoneMatch = r.Header["If-None-Match"]
		_, sawIfModifiedSince = r.Header["If-Modified-Since"]
		w.Write([]byte(miniSpec))
	}))
	defer server.Close()

	client := testClient()
	if _, err := client.Get(context.Background(), server.URL+"/openapi.json", "", ""); err != nil {
		t.Fatal(err)
	}
	if sawIfNoneMatch || sawIfModifiedSince {
		t.Error("conditional headers sent with no stored validators")
	}

	if _, err := client.Get(context.Background(), server.URL+"/openapi.json", `"v1"`, ""); err != nil {
		t.Fatal(err)
	}
	if !sawIfNoneMatch {
		t.Error("If-None-Match not sent when an ETag is known")
	}
}
