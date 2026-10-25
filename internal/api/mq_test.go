package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"spectra/internal/config"
	"spectra/internal/rabbitmq"
	"spectra/internal/store"
)

func withBroker(cfg *config.Config) {
	cfg.RabbitMQ = []config.Broker{{Name: "prod", URL: "http://rabbit:15672", User: "spectra", Password: "secret"}}
}

func (e *env) storeSnapshot(t *testing.T) {
	t.Helper()
	snap := rabbitmq.Snapshot{
		Cluster: "prod", CapturedAt: time.Now(),
		Vhosts: []rabbitmq.Vhost{{Name: "documents", Accessible: true,
			Queues: []rabbitmq.Queue{{Name: "documents.notify", Type: "quorum"}}}},
		Connections: []rabbitmq.Connection{{Name: "c1", Vhost: "documents", User: "app-user", PeerHost: "10.0.0.3"}},
		Problems: []rabbitmq.Problem{
			{Kind: rabbitmq.ProblemUnbound, Vhost: "documents", ObjectKind: "exchange", Object: "legacy.events"},
			{Kind: rabbitmq.ProblemAnonymous, Vhost: "documents", ObjectKind: "connection", Object: "c1"},
		},
	}
	body, _ := json.Marshal(snap)
	if err := e.store.SaveMQSnapshot(context.Background(), "prod", body, []byte(`{}`), "h", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SaveMQFailure(context.Background(), "prod", "http://rabbit:15672: login spectra rejected", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestAnonymousSeesTopologyButNotTheBrokerAccount(t *testing.T) {
	e := newEnv(t, withBroker)
	e.storeSnapshot(t)

	anon := e.do(t, "GET", "/api/mq", "", false).Body.String()
	if strings.Contains(anon, "app-user") || strings.Contains(anon, "login spectra rejected") {
		t.Errorf("аноним видит учётку соединения или текст ошибки брокера:\n%s", anon)
	}
	if !strings.Contains(anon, "documents.notify") || !strings.Contains(anon, `"failing":true`) {
		t.Errorf("аноним должен видеть топологию и то, что данные устарели:\n%s", anon)
	}

	admin := e.do(t, "GET", "/api/mq", "", true).Body.String()
	if !strings.Contains(admin, "app-user") || !strings.Contains(admin, "login spectra rejected") {
		t.Errorf("администратору нужны учётка и ошибка, чтобы чинить:\n%s", admin)
	}
}

func TestBrokerRefreshNeedsTheLogin(t *testing.T) {
	e := newEnv(t, withBroker)
	if rec := e.do(t, "POST", "/api/mq/refresh", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("снимок по кнопке шлёт запросы брокеру — без входа нельзя: %d", rec.Code)
	}
	if rec := e.do(t, "POST", "/api/mq/refresh", "", true); rec.Code != http.StatusAccepted {
		t.Errorf("статус = %d", rec.Code)
	}
}

func TestBrokerMetricsPrintEveryKind(t *testing.T) {
	e := newEnv(t, withBroker)
	e.storeSnapshot(t)
	body := e.do(t, "GET", "/metrics", "", false).Body.String()
	for _, want := range []string{
		`spectra_mq_last_success_timestamp_seconds{cluster="prod"}`,
		`spectra_mq_problems{cluster="prod",kind="unbound"} 1`,
		// Zeroes are printed too: an alert on "> 0" needs the series to exist.
		`spectra_mq_problems{cluster="prod",kind="nodlq"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q", want)
		}
	}
	if strings.Contains(body, "rabbit:15672") {
		t.Error("адрес брокера утёк в метрики")
	}
}

func TestNoBrokerNoBrokerMetrics(t *testing.T) {
	e := newEnv(t, nil)
	if body := e.do(t, "GET", "/metrics", "", false).Body.String(); strings.Contains(body, "spectra_mq_") {
		t.Error("без брокеров метрик RabbitMQ быть не должно")
	}
	var overview struct {
		RabbitMQ bool `json:"rabbitmq"`
	}
	json.Unmarshal(e.do(t, "GET", "/api/overview", "", false).Body.Bytes(), &overview)
	if overview.RabbitMQ {
		t.Error("overview говорит, что брокеры подключены")
	}
}

func TestDeclaredReaderIsAdminOnlyAndShownToEveryone(t *testing.T) {
	e := newEnv(t, withBroker)
	body := `{"cluster":"prod","vhost":"documents","objectKind":"queue","object":"documents.notify",
		"role":"consume","service":"documents-archiver","mode":"schedule","note":"каждую ночь в 03:00",
		"source":"code"}` // источник ставит сервер: через кнопку бывает только «вручную»

	if rec := e.do(t, "PUT", "/api/mq/declared", body, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("заявлять связи без входа нельзя: %d", rec.Code)
	}
	rec := e.do(t, "PUT", "/api/mq/declared", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d: %s", rec.Code, rec.Body.String())
	}
	id := decode[struct {
		ID int64 `json:"id"`
	}](t, rec).ID

	// Повторное заявление той же связи обновляет её, а не плодит дубли.
	e.do(t, "PUT", "/api/mq/declared", strings.Replace(body, "03:00", "04:00", 1), true)

	got := decode[struct {
		Declared []struct {
			ID      int64  `json:"id"`
			Service string `json:"service"`
			Note    string `json:"note"`
			Source  string `json:"source"`
		} `json:"declared"`
	}](t, e.do(t, "GET", "/api/mq", "", false))
	if len(got.Declared) != 1 || got.Declared[0].Note != "каждую ночь в 04:00" || got.Declared[0].Source != "manual" {
		t.Fatalf("аноним должен видеть одну обновлённую ручную отметку: %+v", got.Declared)
	}

	if rec := e.do(t, "DELETE", "/api/mq/declared/"+itoa(id), "", true); rec.Code != http.StatusOK {
		t.Fatalf("удаление: %d", rec.Code)
	}
}

func TestDeclaredLinkMustMakeSense(t *testing.T) {
	e := newEnv(t, withBroker)
	for name, body := range map[string]string{
		"чужой брокер":         `{"cluster":"other","vhost":"/","objectKind":"queue","object":"q","role":"consume","service":"s"}`,
		"очередь не публикуют": `{"cluster":"prod","vhost":"/","objectKind":"queue","object":"q","role":"publish","service":"s"}`,
		"без сервиса":          `{"cluster":"prod","vhost":"/","objectKind":"queue","object":"q","role":"consume","service":" "}`,
		"неизвестный способ":   `{"cluster":"prod","vhost":"/","objectKind":"queue","object":"q","role":"consume","service":"s","mode":"psychic"}`,
	} {
		if rec := e.do(t, "PUT", "/api/mq/declared", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: статус = %d", name, rec.Code)
		}
	}
}

func TestCodeDeclaredLinksAreNotRemovedByHand(t *testing.T) {
	e := newEnv(t, withBroker)
	id, err := e.store.SaveMQDeclared(context.Background(), store.MQDeclared{
		Cluster: "prod", Vhost: "/", ObjectKind: "queue", Object: "q", Role: "consume",
		Service: "batch", Source: "code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "DELETE", "/api/mq/declared/"+itoa(id), "", true); rec.Code != http.StatusNotFound {
		t.Errorf("отметку из кода снимает сканер, а не кнопка: %d", rec.Code)
	}
}
