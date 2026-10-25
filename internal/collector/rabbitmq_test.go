package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"spectra/internal/config"
	"spectra/internal/rabbitmq"
	"spectra/internal/store"
)

// broker is a management API whose answers a test can change between polls.
type broker struct {
	mu        sync.Mutex
	consumers string
	bindings  string
	server    *httptest.Server
}

func newBroker(t *testing.T) *broker {
	t.Helper()
	b := &broker{
		consumers: `[{"queue":{"name":"documents.convert","vhost":"documents"},"channel_details":{"connection_name":"c1"}}]`,
		bindings:  `[{"source":"documents","vhost":"documents","destination":"documents.convert","destination_type":"queue","routing_key":"convert.*"}]`,
	}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var body string
		switch r.URL.EscapedPath() {
		case "/api/overview":
			body = `{"rabbitmq_version":"3.13.7"}`
		case "/api/vhosts":
			body = `[{"name":"documents"}]`
		case "/api/exchanges":
			body = `[{"name":"","vhost":"documents","type":"direct"},{"name":"documents","vhost":"documents","type":"topic"}]`
		case "/api/queues":
			body = `[{"name":"documents.convert","vhost":"documents","type":"quorum","durable":true}]`
		case "/api/bindings":
			body = b.bindings
		case "/api/consumers":
			body = b.consumers
		case "/api/connections":
			body = `[{"name":"c1","vhost":"documents","peer_host":"10.0.0.1","user_provided_name":"documents-converter"}]`
		case "/api/policies":
			body = `[]`
		case "/api/exchanges/documents/documents":
			body = `{"incoming":[],"message_stats":{}}`
		default:
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (h *harness) withBroker(b *broker) {
	h.collector.cfg.RabbitMQ = []config.Broker{{Name: "prod", URL: b.server.URL, User: "u", Password: "p"}}
}

func (h *harness) mqChanges(t *testing.T) []store.MQChange {
	t.Helper()
	changes, err := h.store.ListMQChanges(context.Background(), store.MQChangeFilter{Cluster: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func (h *harness) mqSnapshot(t *testing.T) rabbitmq.Snapshot {
	t.Helper()
	st, err := h.store.MQState(context.Background(), "prod")
	if err != nil || st == nil {
		t.Fatalf("состояния брокера нет: %v", err)
	}
	var snap rabbitmq.Snapshot
	if err := json.Unmarshal([]byte(st.Snapshot), &snap); err != nil {
		t.Fatalf("снимок не разбирается: %v", err)
	}
	return snap
}

func TestBrokerHistoryIsWrittenOnlyWhenTopologyMoves(t *testing.T) {
	h := newHarness(t, nil)
	b := newBroker(t)
	h.withBroker(b)
	ctx := context.Background()

	h.collector.PollBrokers(ctx)
	h.collector.PollBrokers(ctx)
	if n := len(h.mqChanges(t)); n != 1 {
		t.Fatalf("два одинаковых снимка — одна запись «первый снимок», получили %d", n)
	}

	// Ещё один потребитель того же сервиса — масштабирование, а не смена топологии.
	b.mu.Lock()
	b.consumers = `[{"queue":{"name":"documents.convert","vhost":"documents"},"channel_details":{"connection_name":"c1"}},` +
		`{"queue":{"name":"documents.convert","vhost":"documents"},"channel_details":{"connection_name":"c1"}}]`
	b.mu.Unlock()
	h.collector.PollBrokers(ctx)
	if n := len(h.mqChanges(t)); n != 1 {
		t.Fatalf("второй потребитель того же сервиса записан как изменение: %d записей", n)
	}
	if got := h.mqSnapshot(t).Vhosts[0].Queues[0].Consumers; got != 2 {
		t.Errorf("снимок должен обновляться каждый раз: consumers = %d", got)
	}

	// Подписчика не застали — например, задача читает раз в сутки. Не изменение.
	b.mu.Lock()
	b.consumers = `[]`
	b.mu.Unlock()
	h.collector.PollBrokers(ctx)
	if n := len(h.mqChanges(t)); n != 1 {
		t.Fatalf("подписчик, которого не застал снимок, записан как изменение: %d записей", n)
	}

	// А удалённая привязка — изменение.
	b.mu.Lock()
	b.bindings = `[]`
	b.mu.Unlock()
	h.collector.PollBrokers(ctx)
	changes := h.mqChanges(t)
	if len(changes) != 2 || changes[0].ObjectKind != "binding" || changes[0].Severity != rabbitmq.SeverityBreaking {
		t.Fatalf("удалённая привязка должна попасть в историю как ломающая: %+v", changes)
	}
}

func TestUnreachableBrokerKeepsWhatWasSeen(t *testing.T) {
	h := newHarness(t, nil)
	b := newBroker(t)
	h.withBroker(b)
	ctx := context.Background()

	h.collector.PollBrokers(ctx)
	b.server.Close()
	h.collector.PollBrokers(ctx)

	st, _ := h.store.MQState(ctx, "prod")
	if st.LastError == "" {
		t.Error("неудачный опрос должен оставить ошибку")
	}
	if snap := h.mqSnapshot(t); len(snap.Vhosts) != 1 {
		t.Errorf("недоступный брокер не потерял свои очереди, а снимок пропал: %+v", snap)
	}
	if n := len(h.mqChanges(t)); n != 1 {
		t.Errorf("недоступность не должна выглядеть как удаление всего: %d записей", n)
	}
}

func TestNamedConnectionPointsAtTheRegistry(t *testing.T) {
	h := newHarness(t, nil)
	id := h.register(t, "documents-converter", "http://documents-converter.docs:80")
	h.withBroker(newBroker(t))
	h.collector.PollBrokers(context.Background())

	p := h.mqSnapshot(t).Vhosts[0].Queues[0].ConsumedBy[0]
	if p.Service != "documents-converter" || p.ServiceID != id || p.Via != "name" {
		t.Fatalf("потребитель должен ссылаться на сервис из реестра: %+v (id %d)", p, id)
	}
}
