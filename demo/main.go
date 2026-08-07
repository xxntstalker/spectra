// Command demoservices runs a handful of fake services so the portal can be
// exercised locally.
//
// Each one publishes its spec the way a different framework does — a couple at
// conventional paths, one only reachable by reading its HTML docs page, one on
// Swagger 2.0, one in YAML. That variety is the point: it is what the portal's
// discovery has to survive in a real estate.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type docStyle string

const (
	docsNone          docStyle = ""
	docsSwaggerConfig docStyle = "swagger-ui-config" // springdoc: page → configUrl → urls[]
	docsSwaggerDirect docStyle = "swagger-ui-direct" // classic: url: "..." inline
	docsRedoc         docStyle = "redoc"             // <redoc spec-url="...">
	docsScalar        docStyle = "scalar"            // <script data-url="...">
)

// mutation changes a spec the way a deploy would, and describes what it did.
type mutation struct {
	label string
	apply func(spec map[string]any)
}

type service struct {
	id       string
	title    string
	port     int
	specPath string
	yamlSpec bool
	docPath  string
	docStyle docStyle

	mu        sync.RWMutex
	spec      map[string]any
	mutations []mutation
	applied   int
}

func main() {
	interval := flag.Duration("mutate-interval", 0,
		"apply one spec change to one service every interval (0 disables); use e.g. 45s to watch the change feed fill up")
	flag.Parse()

	services := buildServices()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []*http.Server
	for _, svc := range services {
		srv := &http.Server{Addr: fmt.Sprintf(":%d", svc.port), Handler: svc.handler()}
		servers = append(servers, srv)
		go func(svc *service, srv *http.Server) {
			log.Printf("%-18s http://localhost:%d  spec: %s%s", svc.id, svc.port, svc.specPath, docsNote(svc))
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("%s: %v", svc.id, err)
			}
		}(svc, srv)
	}

	log.Printf("демо-сервисы запущены; изменить спеку вручную: curl -X POST http://localhost:PORT/_demo/mutate")
	if *interval > 0 {
		go mutateLoop(ctx, services, *interval)
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
}

func docsNote(svc *service) string {
	if svc.docPath == "" {
		return ""
	}
	return fmt.Sprintf("  docs: %s (%s)", svc.docPath, svc.docStyle)
}

// mutateLoop walks the services in turn so changes are spread across the feed
// instead of piling onto one service.
func mutateLoop(ctx context.Context, services []*service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			svc := services[i%len(services)]
			i++
			log.Printf("mutate %s: %s", svc.id, svc.mutate())
		}
	}
}

// mutate applies the next scripted change. Once the script runs out it keeps
// bumping the patch version, so a long-running demo always has something new.
func (s *service) mutate() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.applied < len(s.mutations) {
		m := s.mutations[s.applied]
		s.applied++
		m.apply(s.spec)
		return m.label
	}
	bumpPatchVersion(s.spec)
	return "патч-версия увеличена"
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+s.specPath, func(w http.ResponseWriter, r *http.Request) {
		body, contentType := s.renderSpec()
		etag := `"` + shortHash(body) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", contentType)
		// Real services answer conditional requests; the portal relies on it
		// to keep frequent polling cheap, so the demo honours it too.
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(body)
	})

	if s.docStyle == docsSwaggerConfig {
		mux.HandleFunc("GET "+s.specPath+"/swagger-config", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"configUrl": s.specPath + "/swagger-config",
				"urls": []map[string]string{
					{"url": s.specPath, "name": s.id},
				},
				"validatorUrl": "",
			})
		})
	}

	if s.docPath != "" {
		mux.HandleFunc("GET "+s.docPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(s.docsHTML()))
		})
	}

	mux.HandleFunc("POST /_demo/mutate", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"service": s.id, "applied": s.mutate()})
	})

	// Everything else answers like a working API so "Try it out" in the portal
	// returns something instead of a connection error.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"_demo":   true,
			"service": s.id,
			"method":  r.Method,
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
			"note":    "Демо-ответ: реальной бизнес-логики здесь нет.",
		})
	})

	return mux
}

func (s *service) renderSpec() (body []byte, contentType string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.yamlSpec {
		out, err := yaml.Marshal(s.spec)
		if err != nil {
			return []byte("marshal error"), "text/plain"
		}
		return out, "application/yaml; charset=utf-8"
	}
	out, err := json.MarshalIndent(s.spec, "", "  ")
	if err != nil {
		return []byte("marshal error"), "text/plain"
	}
	return out, "application/json; charset=utf-8"
}

func (s *service) docsHTML() string {
	switch s.docStyle {
	case docsSwaggerConfig:
		return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>%[1]s — Swagger UI</title>
<link rel="stylesheet" href="/webjars/swagger-ui/swagger-ui.css"></head>
<body><div id="swagger-ui"></div>
<script src="/webjars/swagger-ui/swagger-ui-bundle.js"></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({
    configUrl: "%[2]s/swagger-config",
    dom_id: "#swagger-ui",
    deepLinking: true
  });
};
</script></body></html>`, s.title, s.specPath)

	case docsSwaggerDirect:
		return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>%[1]s — Swagger UI</title>
<link rel="stylesheet" href="/swagger-ui/swagger-ui.css"></head>
<body><div id="swagger-ui"></div>
<script src="/swagger-ui/swagger-ui-bundle.js"></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "%[2]s",
    dom_id: "#swagger-ui"
  });
};
</script></body></html>`, s.title, s.specPath)

	case docsRedoc:
		// The stale petstore URL left over from the starter template is not an
		// accident: the portal must ignore cross-origin candidates, or it would
		// aggregate the public petstore instead of this service.
		return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>%[1]s — API</title></head>
<body>
<!-- TODO: убрать пример из шаблона
     SwaggerUIBundle({ url: "https://petstore.swagger.io/v2/swagger.json" }) -->
<redoc spec-url="%[2]s"></redoc>
<script src="/static/redoc.standalone.js"></script>
</body></html>`, s.title, s.specPath)

	case docsScalar:
		return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>%[1]s — API Reference</title></head>
<body>
<script id="api-reference" data-url="%[2]s"></script>
<script src="/static/scalar.standalone.js"></script>
</body></html>`, s.title, s.specPath)
	}
	return "<!DOCTYPE html><html><body>no docs</body></html>"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func mustParse(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(fmt.Sprintf("demo spec is not valid JSON: %v", err))
	}
	return m
}

func buildServices() []*service {
	return []*service{
		{
			id: "orders", title: "Orders API", port: 7101,
			specPath: "/v3/api-docs", // Spring Boot + springdoc
			docPath:  "/swagger-ui/index.html", docStyle: docsSwaggerConfig,
			spec: mustParse(ordersSpec),
			mutations: []mutation{
				{"добавлен GET /orders/{id}/events", addOrderEventsEndpoint},
				{"добавлен необязательный параметр createdAfter", func(s map[string]any) {
					addParam(operation(s, "/orders", "get"), map[string]any{
						"name": "createdAfter", "in": "query", "required": false,
						"schema": map[string]any{"type": "string", "format": "date-time"},
					})
				}},
				{"версия API 2.4.1 → 2.5.0", func(s map[string]any) { setVersion(s, "2.5.0") }},
			},
		},
		{
			id: "billing", title: "Billing API", port: 7102,
			specPath: "/swagger/v1/swagger.json", // ASP.NET Core
			docPath:  "/swagger", docStyle: docsSwaggerDirect,
			spec: mustParse(billingSpec),
			mutations: []mutation{
				{"параметр currency стал обязательным (ломающее)", func(s map[string]any) {
					addParam(operation(s, "/invoices", "get"), map[string]any{
						"name": "currency", "in": "query", "required": true,
						"schema": map[string]any{"type": "string", "enum": []any{"RUB", "USD", "EUR"}},
					})
				}},
				{"добавлен ответ 429 в POST /invoices/{id}/pay", func(s map[string]any) {
					addResponse(operation(s, "/invoices/{id}/pay", "post"), "429", "Too many requests")
				}},
			},
		},
		{
			id: "inventory", title: "Inventory API", port: 7103,
			specPath: "/openapi.json", // FastAPI
			docPath:  "/docs", docStyle: docsSwaggerDirect,
			spec: mustParse(inventorySpec),
			mutations: []mutation{
				// Item is referenced from three operations, so this single edit
				// shows up as three changed endpoints — the reason the differ
				// resolves $refs before hashing.
				{"в общую схему Item добавлено поле reservedQuantity", func(s map[string]any) {
					addProperty(schema(s, "Item"), "reservedQuantity",
						map[string]any{"type": "integer", "minimum": 0})
				}},
				{"поле title в Item стало обязательным (ломающее)", func(s map[string]any) {
					addRequired(schema(s, "Item"), "title")
				}},
			},
		},
		{
			id: "notifications", title: "Notifications API", port: 7104,
			// Deliberately off the beaten path: only the docs page knows where
			// this spec lives.
			specPath: "/internal/contracts/notifications-v1.json",
			docPath:  "/swagger", docStyle: docsRedoc,
			spec: mustParse(notificationsSpec),
			mutations: []mutation{
				{"удалён DELETE /subscriptions/{id} (ломающее)", func(s map[string]any) {
					delete(paths(s), "/subscriptions/{id}")
				}},
				{"POST /notifications/send помечен deprecated", func(s map[string]any) {
					if op := operation(s, "/notifications/send", "post"); op != nil {
						op["deprecated"] = true
					}
				}},
			},
		},
		{
			id: "legacy-payments", title: "Legacy Payments", port: 7105,
			specPath: "/v2/api-docs", // springfox, Swagger 2.0
			spec:     mustParse(legacyPaymentsSpec),
			mutations: []mutation{
				{"GET /payments помечен deprecated", func(s map[string]any) {
					if op := operation(s, "/payments", "get"); op != nil {
						op["deprecated"] = true
					}
				}},
				{"убран ответ 400 из POST /payments", func(s map[string]any) {
					if op := operation(s, "/payments", "post"); op != nil {
						delete(mapAt(op, "responses"), "400")
					}
				}},
			},
		},
		{
			id: "analytics", title: "Analytics API", port: 7106,
			specPath: "/openapi.yaml", yamlSpec: true,
			docPath: "/swagger", docStyle: docsScalar,
			spec: mustParse(analyticsSpec),
			mutations: []mutation{
				{"добавлен ответ 429 в POST /reports/{id}/run", func(s map[string]any) {
					addResponse(operation(s, "/reports/{id}/run", "post"), "429", "Too many requests")
				}},
				{"добавлен DELETE /reports/{id}", func(s map[string]any) {
					item := mapAt(paths(s), "/reports/{id}")
					if item == nil {
						return
					}
					item["delete"] = map[string]any{
						"operationId": "deleteReport",
						"summary":     "Удалить отчёт",
						"responses": map[string]any{
							"204": map[string]any{"description": "Deleted"},
							"404": map[string]any{"description": "Not found"},
						},
					}
				}},
			},
		},
		{
			id: "warehouse", title: "Warehouse API", port: 7107,
			// Spec hidden behind an in-house prefix that no probe covers. The
			// only thing that knows where it lives is the Swagger UI page, and
			// even that points at a config endpoint rather than the spec — the
			// springdoc arrangement, and the last resort of discovery.
			specPath: "/internal/openapi-v3",
			docPath:  "/swagger", docStyle: docsSwaggerConfig,
			spec: mustParse(warehouseSpec),
			mutations: []mutation{
				{"добавлен параметр carrier в GET /shipments", func(s map[string]any) {
					addParam(operation(s, "/shipments", "get"), map[string]any{
						"name": "carrier", "in": "query", "required": false,
						"schema": map[string]any{"type": "string"},
					})
				}},
			},
		},
	}
}

func addOrderEventsEndpoint(spec map[string]any) {
	paths(spec)["/orders/{id}/events"] = map[string]any{
		"parameters": []any{map[string]any{
			"name": "id", "in": "path", "required": true,
			"schema": map[string]any{"type": "string", "format": "uuid"},
		}},
		"get": map[string]any{
			"tags":        []any{"orders"},
			"operationId": "listOrderEvents",
			"summary":     "История событий заказа",
			"responses": map[string]any{
				"200": map[string]any{
					"description": "OK",
					"content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "array", "items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"at":   map[string]any{"type": "string", "format": "date-time"},
								"kind": map[string]any{"type": "string"},
							},
						}},
					}},
				},
				"404": map[string]any{"description": "Not found"},
			},
		},
	}
}

// Small navigation helpers. They return nil instead of panicking so a mutation
// written against a spec that later changed degrades into a no-op.

func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func paths(spec map[string]any) map[string]any { return mapAt(spec, "paths") }

func operation(spec map[string]any, path, method string) map[string]any {
	return mapAt(mapAt(paths(spec), path), method)
}

// schema finds a component schema in either OpenAPI 3 or Swagger 2 layout.
func schema(spec map[string]any, name string) map[string]any {
	if s := mapAt(mapAt(mapAt(spec, "components"), "schemas"), name); s != nil {
		return s
	}
	return mapAt(mapAt(spec, "definitions"), name)
}

func addParam(op map[string]any, param map[string]any) {
	if op == nil {
		return
	}
	existing, _ := op["parameters"].([]any)
	op["parameters"] = append(existing, param)
}

func addResponse(op map[string]any, code, description string) {
	if op == nil {
		return
	}
	responses := mapAt(op, "responses")
	if responses == nil {
		responses = map[string]any{}
		op["responses"] = responses
	}
	responses[code] = map[string]any{"description": description}
}

func addProperty(sch map[string]any, name string, definition map[string]any) {
	if sch == nil {
		return
	}
	props := mapAt(sch, "properties")
	if props == nil {
		props = map[string]any{}
		sch["properties"] = props
	}
	props[name] = definition
}

func addRequired(sch map[string]any, field string) {
	if sch == nil {
		return
	}
	required, _ := sch["required"].([]any)
	for _, v := range required {
		if v == field {
			return
		}
	}
	sch["required"] = append(required, field)
}

func setVersion(spec map[string]any, version string) {
	if info := mapAt(spec, "info"); info != nil {
		info["version"] = version
	}
}

func bumpPatchVersion(spec map[string]any) {
	info := mapAt(spec, "info")
	if info == nil {
		return
	}
	current, _ := info["version"].(string)
	parts := strings.Split(current, ".")
	if len(parts) != 3 {
		info["version"] = current + ".1"
		return
	}
	patch := 0
	fmt.Sscanf(parts[2], "%d", &patch)
	info["version"] = fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
}
