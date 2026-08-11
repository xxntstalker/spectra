package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spectra/internal/config"
	"spectra/internal/store"
)

// fakeAPIServer stands in for the Kubernetes API. The portal speaks plain HTTP
// to it, so a test server is a complete substitute — no cluster needed.
func fakeAPIServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, apiServer string, tune func(*config.Kubernetes)) *Client {
	t.Helper()
	cfg := config.Kubernetes{
		APIServer:         apiServer,
		Token:             "test-token",
		CACertPath:        "/nonexistent", // plain http test server needs no CA
		Target:            config.TargetService,
		ExcludeNamespaces: []string{"kube-system"},
	}
	if tune != nil {
		tune(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func ingressJSON(items ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

// rule builds one Ingress object.
func rule(namespace, name, host, path, service string, port int, annotations map[string]string) map[string]any {
	meta := map[string]any{"name": name, "namespace": namespace}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	return map[string]any{
		"metadata": meta,
		"spec": map[string]any{
			"rules": []any{map[string]any{
				"host": host,
				"http": map[string]any{"paths": []any{map[string]any{
					"path": path,
					"backend": map[string]any{"service": map[string]any{
						"name": service,
						"port": map[string]any{"number": port},
					}},
				}}},
			}},
		},
	}
}

func discover(t *testing.T, c *Client) map[string]store.Discovered {
	t.Helper()
	items, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	out := map[string]store.Discovered{}
	for _, item := range items {
		out[item.Key] = item
	}
	return out
}

func TestDiscoverBuildsClusterAddresses(t *testing.T) {
	body := ingressJSON(rule("shop", "orders", "orders.example.com", "/", "orders-svc", 8080, nil))
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	found := discover(t, c)
	item, ok := found["orders-svc.shop:8080"]
	if !ok {
		t.Fatalf("expected the backend service as the key, got %v", keysOf(found))
	}
	// Going to the Ingress hostname from inside the cluster often does not
	// resolve, or leaves for the load balancer and comes back.
	if item.BaseURL != "http://orders-svc.shop:8080" {
		t.Errorf("base URL = %q, want the in-cluster address", item.BaseURL)
	}
	if item.Host != "orders.example.com" || item.Namespace != "shop" || item.IngressName != "orders" {
		t.Errorf("provenance lost: %+v", item)
	}
	// The public host is kept as the way in for when the in-cluster address
	// stops answering — a NetworkPolicy, a renamed Service, a port only the
	// ingress controller may reach.
	if item.FallbackURL != "http://orders.example.com" {
		t.Errorf("fallback = %q, want the public host", item.FallbackURL)
	}
}

// Несколько Ingress в разных namespace на один публичный хост — это один API
// снаружи, а в реестре получалось несколько одинаковых записей.
func TestSameHostInSeveralNamespacesCollapsesToOneEntry(t *testing.T) {
	body := ingressJSON(
		rule("shop", "api", "api.example.com", "/", "api-svc", 80, nil),
		rule("shop-new", "api", "api.example.com", "/", "api-svc", 80, nil),
		rule("legacy", "api", "api.example.com", "/", "old-api", 80, nil),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	// Дискавери не выбирает победителя — оно только сообщает, что все три
	// записи стоят на одном публичном адресе. Кто из них настоящий, видно по
	// истории и по тому, кто отвечает, а этих данных здесь нет.
	found := discover(t, c)
	if len(found) != 3 {
		t.Fatalf("записей %d, ожидалось три: %v", len(found), keysOf(found))
	}
	for key, item := range found {
		if item.PublicKey != "api.example.com" {
			t.Errorf("%s: публичный адрес = %q", key, item.PublicKey)
		}
	}
}

// Один хост может обслуживать два разных API под разными префиксами — это не
// дубль, и схлопывать их нельзя.
func TestSameHostDifferentPathsStayApart(t *testing.T) {
	body := ingressJSON(
		rule("shop", "orders", "api.example.com", "/orders", "orders-svc", 80, nil),
		rule("shop", "billing", "api.example.com", "/billing", "billing-svc", 80, nil),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	seen := map[string]bool{}
	for _, item := range discover(t, c) {
		seen[item.PublicKey] = true
	}
	if !seen["api.example.com/orders"] || !seen["api.example.com/billing"] {
		t.Errorf("префиксы пути потерялись, публичные адреса: %v", seen)
	}
}

// Имя, которое команда объявила аннотацией, выигрывает слияние: оно осмысленнее
// имени Service и не зависит от того, какой Ingress прочли первым.
func TestWrittenNameWinsTheMerge(t *testing.T) {
	body := ingressJSON(
		rule("a", "api", "api.example.com", "/", "aaa-svc", 80, nil),
		rule("b", "api", "api.example.com", "/", "zzz-svc", 80,
			map[string]string{annotationName: "Заказы"}),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	item, ok := discover(t, c)["zzz-svc.b:80"]
	if !ok {
		t.Fatal("запись с аннотацией не найдена")
	}
	if item.DisplayName != "Заказы" || !item.NameIsExplicit {
		t.Errorf("имя из аннотации потерялось: %q, явное=%v", item.DisplayName, item.NameIsExplicit)
	}
}

// Whichever address is primary, the other one is the fallback: the pair is the
// same, only the order changes.
func TestFallbackIsTheOtherAddress(t *testing.T) {
	body := ingressJSON(map[string]any{
		"metadata": map[string]any{"name": "orders", "namespace": "shop"},
		"spec": map[string]any{
			"tls": []any{map[string]any{"hosts": []any{"orders.example.com"}}},
			"rules": []any{map[string]any{
				"host": "orders.example.com",
				"http": map[string]any{"paths": []any{map[string]any{
					"path": "/",
					"backend": map[string]any{"service": map[string]any{
						"name": "orders-svc", "port": map[string]any{"number": 80},
					}},
				}}},
			}},
		},
	})
	c := newClient(t, fakeAPIServer(t, body).URL, func(k *config.Kubernetes) {
		k.Target = config.TargetIngress
	})

	item := discover(t, c)["orders-svc.shop:80"]
	if item.BaseURL != "https://orders.example.com" {
		t.Errorf("base URL = %q", item.BaseURL)
	}
	if item.FallbackURL != "http://orders-svc.shop:80" {
		t.Errorf("fallback = %q, want the in-cluster address", item.FallbackURL)
	}
}

func TestDiscoverCanTargetTheIngressHost(t *testing.T) {
	body := ingressJSON(map[string]any{
		"metadata": map[string]any{"name": "orders", "namespace": "shop"},
		"spec": map[string]any{
			"tls": []any{map[string]any{"hosts": []any{"orders.example.com"}}},
			"rules": []any{map[string]any{
				"host": "orders.example.com",
				"http": map[string]any{"paths": []any{map[string]any{
					"path": "/",
					"backend": map[string]any{"service": map[string]any{
						"name": "orders-svc", "port": map[string]any{"number": 80},
					}},
				}}},
			}},
		},
	})
	c := newClient(t, fakeAPIServer(t, body).URL, func(k *config.Kubernetes) {
		k.Target = config.TargetIngress
	})

	found := discover(t, c)
	item := found["orders-svc.shop:80"]
	// The scheme has to come from the TLS block, not be assumed.
	if item.BaseURL != "https://orders.example.com" {
		t.Errorf("base URL = %q, want https from the TLS section", item.BaseURL)
	}
}

func TestDiscoverSkipsWhatCannotOrShouldNotBeProbed(t *testing.T) {
	body := ingressJSON(
		rule("shop", "orders", "orders.example.com", "/", "orders-svc", 80, nil),
		// cert-manager creates these for the duration of a challenge.
		rule("shop", "cm-acme-http-solver-abc", "orders.example.com",
			"/.well-known/acme-challenge/tok", "solver", 8089, nil),
		// The owner asked to be left alone.
		rule("shop", "private", "private.example.com", "/", "private-svc", 80,
			map[string]string{annotationSkip: "true"}),
		// A wildcard host is not an address anything can call.
		rule("shop", "wild", "*.example.com", "/", "wild-svc", 80, nil),
		// Excluded namespace.
		rule("kube-system", "dashboard", "dash.example.com", "/", "kube-dns", 53, nil),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	found := discover(t, c)
	if len(found) != 1 {
		t.Fatalf("expected only the real service, got %v", keysOf(found))
	}
	if _, ok := found["orders-svc.shop:80"]; !ok {
		t.Errorf("the one service that should be discovered is missing: %v", keysOf(found))
	}
}

// Two ingresses for one backend are one API. Which of them is read first is
// random, so the name must not depend on it.
func TestDuplicateBackendsCollapseAndPreferTheWrittenName(t *testing.T) {
	body := ingressJSON(
		rule("shop", "orders-public", "orders.example.com", "/", "orders-svc", 80, nil),
		rule("shop", "orders-internal", "orders.internal", "/", "orders-svc", 80,
			map[string]string{annotationName: "Orders API"}),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	for attempt := 0; attempt < 20; attempt++ {
		found := discover(t, c)
		if len(found) != 1 {
			t.Fatalf("two ingresses for one backend produced %d services", len(found))
		}
		if got := found["orders-svc.shop:80"].DisplayName; got != "Orders API" {
			t.Fatalf("display name = %q on attempt %d; the annotated name must always win",
				got, attempt)
		}
	}
}

func TestNamespaceFilter(t *testing.T) {
	body := ingressJSON(
		rule("shop", "orders", "orders.example.com", "/", "orders-svc", 80, nil),
		rule("other", "thing", "thing.example.com", "/", "thing-svc", 80, nil),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, func(k *config.Kubernetes) {
		k.Namespaces = []string{"shop", "extra"} // two, so the URL stays cluster-wide
	})

	found := discover(t, c)
	if len(found) != 1 || found["orders-svc.shop:80"].Namespace != "shop" {
		t.Errorf("namespace filter ignored: %v", keysOf(found))
	}
}

func TestIngressClassFilter(t *testing.T) {
	withClass := map[string]any{
		"metadata": map[string]any{"name": "a", "namespace": "shop"},
		"spec": map[string]any{
			"ingressClassName": "internal",
			"rules": []any{map[string]any{
				"host": "a.example.com",
				"http": map[string]any{"paths": []any{map[string]any{
					"path": "/",
					"backend": map[string]any{"service": map[string]any{
						"name": "a-svc", "port": map[string]any{"number": 80},
					}},
				}}},
			}},
		},
	}
	body := ingressJSON(withClass,
		rule("shop", "b", "b.example.com", "/", "b-svc", 80, nil))

	c := newClient(t, fakeAPIServer(t, body).URL, func(k *config.Kubernetes) {
		k.IngressClasses = []string{"internal"}
	})
	found := discover(t, c)
	if len(found) != 1 {
		t.Errorf("ingress class filter ignored: %v", keysOf(found))
	}
}

func TestPathPrefixBecomesPartOfTheAddress(t *testing.T) {
	body := ingressJSON(
		rule("shop", "orders", "api.example.com", "/orders", "orders-svc", 80, nil),
		// Rewrite captures used by nginx carry no meaning for us.
		rule("shop", "billing", "api.example.com", "/billing(/|$)(.*)", "billing-svc", 80, nil),
	)
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	found := discover(t, c)
	if got := found["orders-svc.shop:80/orders"].BaseURL; got != "http://orders-svc.shop:80/orders" {
		t.Errorf("path prefix lost: %q", got)
	}
	// The capture groups belong to the rewrite rule, not to the address: the
	// service really is mounted at /billing.
	if got := found["billing-svc.shop:80/billing"].BaseURL; got != "http://billing-svc.shop:80/billing" {
		t.Errorf("rewrite captures not stripped, got %q", got)
	}
}

func TestPathThatIsOnlyARegexBecomesNoPrefix(t *testing.T) {
	body := ingressJSON(rule("shop", "api", "api.example.com", "/(.*)", "api-svc", 80, nil))
	c := newClient(t, fakeAPIServer(t, body).URL, nil)

	found := discover(t, c)
	if got := found["api-svc.shop:80"].BaseURL; got != "http://api-svc.shop:80" {
		t.Errorf("a path made only of a capture should leave no prefix, got %q", got)
	}
}

// A missing RBAC rule is the most likely failure in a real cluster, so the
// message has to name the fix rather than print a bare status code.
func TestForbiddenExplainsTheMissingPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"ingresses is forbidden"}`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, nil)
	_, err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ingresses.networking.k8s.io") {
		t.Errorf("error does not point at the RBAC rule to add: %v", err)
	}
}

func keysOf(m map[string]store.Discovered) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
