package rabbitmq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spectra/internal/config"
)

// fakeBroker answers like the management plugin: JSON for the paths it knows,
// 406 to a JSON request for a path it does not — which is how a disabled
// plugin looks on 3.13.
func fakeBroker(t *testing.T, routes map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if user, pass, ok := r.BasicAuth(); !ok || user != "spectra" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Escaped, as the broker sees it: the default vhost "/" travels as %2F.
		body, ok := routes[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func baseRoutes() map[string]string {
	return map[string]string{
		"/api/overview":    `{"rabbitmq_version":"3.13.7","cluster_name":"rabbit@a"}`,
		"/api/vhosts":      `[{"name":"/"}]`,
		"/api/exchanges":   `[{"name":"","vhost":"/","type":"direct"},{"name":"a","vhost":"/","type":"topic"},{"name":"b","vhost":"/","type":"topic"}]`,
		"/api/queues":      `[{"name":"q","vhost":"/","type":"classic","durable":true}]`,
		"/api/bindings":    `[{"source":"a","vhost":"/","destination":"q","destination_type":"queue","routing_key":"k"}]`,
		"/api/consumers":   `[]`,
		"/api/connections": `[]`,
		"/api/policies":    `[]`,
	}
}

func client(srv *httptest.Server, password string) *Client {
	return NewClient(config.Broker{Name: "prod", URL: srv.URL, User: "spectra", Password: password},
		http.DefaultTransport, 5*time.Second)
}

func TestMissingPluginsAreNotErrors(t *testing.T) {
	routes := baseRoutes()
	routes["/api/exchanges/%2F/a"] = `{"incoming":[],"message_stats":{}}`
	routes["/api/exchanges/%2F/b"] = `{"incoming":[]}`
	srv, _ := fakeBroker(t, routes)
	raw, err := client(srv, "secret").Fetch(context.Background())
	if err != nil {
		t.Fatalf("отсутствие shovel и federation — не ошибка: %v", err)
	}
	if raw.Shovels != nil || raw.FederationLinks != nil || raw.OperatorPolicies != nil {
		t.Error("выключенный плагин должен давать nil, а не пустой список")
	}
}

func TestDefaultRatesModeMeansPublishersUnknown(t *testing.T) {
	routes := baseRoutes()
	// What 3.13 answers without rates_mode = detailed: messages arrived,
	// nobody is listed as having sent them.
	routes["/api/exchanges/%2F/a"] = `{"incoming":[],"message_stats":{"publish_in":6}}`
	routes["/api/exchanges/%2F/b"] = `{"incoming":[],"message_stats":{"publish_in":1}}`
	srv, requests := fakeBroker(t, routes)
	raw, err := client(srv, "secret").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw.Incoming != nil {
		t.Fatalf("публикующие должны быть неизвестны, получили %v", raw.Incoming)
	}
	// 11 lists plus one exchange; the second exchange is not asked.
	if n := requests.Load(); n != 12 {
		t.Errorf("в режиме без детальной статистики спрашиваем один exchange, запросов было %d", n)
	}
}

func TestDetailedRatesModeNamesPublishers(t *testing.T) {
	routes := baseRoutes()
	routes["/api/exchanges/%2F/a"] = `{"incoming":[{"channel_details":{"connection_name":"c1"},"stats":{"publish":6}}],"message_stats":{"publish_in":6}}`
	routes["/api/exchanges/%2F/b"] = `{"incoming":[],"message_stats":{}}`
	srv, _ := fakeBroker(t, routes)
	raw, err := client(srv, "secret").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Incoming["/\x00a"]; len(got) != 1 || got[0].ChannelDetails.ConnectionName != "c1" {
		t.Fatalf("публикующие не прочитаны: %v", raw.Incoming)
	}
	if _, ok := raw.Incoming["/\x00b"]; !ok {
		t.Error("exchange без публикаций должен быть известен как «никто», а не пропущен")
	}
}

// Spectra — каталог, а не мониторинг: сколько сообщений лежит в очередях, она
// у брокера не спрашивает вовсе.
func TestQueueDepthIsNotAsked(t *testing.T) {
	var query string
	routes := baseRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/queues" {
			query = r.URL.RawQuery
		}
		body, ok := routes[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	if _, err := client(srv, "secret").Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "messages") || strings.Contains(query, "enable_queue_totals") {
		t.Errorf("запрос очередей просит счётчики сообщений: %s", query)
	}
}

func TestWrongCredentialsNameTheFix(t *testing.T) {
	srv, _ := fakeBroker(t, baseRoutes())
	_, err := client(srv, "wrong").Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "monitoring") {
		t.Fatalf("ошибка входа должна подсказывать про тег monitoring: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Error("пароль попал в текст ошибки")
	}
}

func TestAMQPPortIsCalledOut(t *testing.T) {
	srv, _ := fakeBroker(t, map[string]string{})
	_, err := client(srv, "secret").Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "15672") {
		t.Fatalf("адрес без management API должен давать понятную ошибку: %v", err)
	}
}
