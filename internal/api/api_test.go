package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"spectra/internal/collector"
	"spectra/internal/config"
	"spectra/internal/store"
)

const spec = `{"openapi":"3.0.3","info":{"title":"Orders","version":"1.0.0"},
  "servers":[{"url":"http://orders.internal"}],
  "paths":{"/orders":{"get":{"operationId":"list","responses":{"200":{"description":"OK"}}}}}}`

const swagger2 = `{"swagger":"2.0","info":{"title":"L","version":"1"},
  "host":"legacy.internal","basePath":"/api","schemes":["https"],
  "paths":{"/pay":{"get":{"responses":{"200":{"description":"OK"}}}}}}`

type env struct {
	handler http.Handler
	server  *Server
	store   *store.Store
	ids     map[string]int64
}

func newEnv(t *testing.T, tune func(*config.Config)) *env {
	t.Helper()
	dsn := os.Getenv("SPECTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SPECTRA_TEST_DATABASE_URL не задан — пропускаю тесты, которым нужен Postgres")
	}
	schema := schemaName("a_", t.Name())
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	ctx := context.Background()
	bootstrap, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("подключение к тестовой базе: %v", err)
	}
	bootstrap.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	if _, err := bootstrap.Pool().Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	bootstrap.Close()

	st, err := store.Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if drop, err := store.Open(context.Background(), dsn); err == nil {
			drop.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			drop.Close()
		}
		st.Close()
	})

	cfg := &config.Config{
		Admin:      config.Admin{User: "admin", Password: "secret"},
		HTTP:       config.HTTP{ProxyEnabled: true, UserAgent: "spectra-test"},
		Kubernetes: config.Kubernetes{SyncInterval: time.Hour},
	}
	if tune != nil {
		tune(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	col := collector.New(cfg, st, nil, log)

	srv := New(cfg, st, col, log)
	e := &env{handler: srv.Handler(), server: srv, store: st, ids: map[string]int64{}}
	return e
}

func (e *env) seed(t *testing.T, items ...store.Discovered) {
	t.Helper()
	if _, err := e.store.SyncDiscovered(context.Background(), "test", items); err != nil {
		t.Fatal(err)
	}
	services, err := e.store.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		e.ids[s.DisplayName] = s.ID
	}
}

func (e *env) do(t *testing.T, method, target, body string, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if admin {
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte("admin:secret")))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("разбор ответа %s: %v", rec.Body.String(), err)
	}
	return v
}

func standardService() store.Discovered {
	return store.Discovered{
		Key: "orders.shop:80", DisplayName: "orders", BaseURL: "http://orders.shop:80",
		Host: "orders.example.com", Namespace: "shop", Backend: "orders.shop:80",
		IngressName: "orders",
	}
}

func TestHealthDescribesThePortalNotTheServices(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())

	if rec := e.do(t, "GET", "/healthz", "", false); rec.Code != http.StatusOK {
		t.Fatalf("статус = %d", rec.Code)
	}
	// A service being down is normal operation and must never take the portal
	// out of rotation.
	e.store.RecordProbe(context.Background(), store.ProbeResult{
		ServiceID: e.ids["orders"], Status: store.ProbeUnreachable, CheckedAt: time.Now(),
	})
	if rec := e.do(t, "GET", "/healthz", "", false); rec.Code != http.StatusOK {
		t.Errorf("недоступный сервис сделал портал нездоровым: %d", rec.Code)
	}
}

// Reading is public, but "everyone" must not be handed a map of the cluster.
func TestAnonymousSeesNoClusterInternals(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	e.store.RecordProbe(context.Background(), store.ProbeResult{
		ServiceID: e.ids["orders"], Status: store.ProbeNoSpec,
		Error: "не найдено по http://orders.shop:80/v3/api-docs", CheckedAt: time.Now(),
	})

	body := decode[struct {
		Services []store.Service `json:"services"`
	}](t, e.do(t, "GET", "/api/services", "", false))
	svc := body.Services[0]

	// Скрытым остаётся то, что адресом не является.
	for name, value := range map[string]string{
		"ingressName": svc.IngressName, "specUrl": svc.SpecURL,
		"manualSpecUrl": svc.ManualSpecURL, "lastError": svc.LastError,
	} {
		if value != "" {
			t.Errorf("аноним видит %s = %q", name, value)
		}
	}
	// The public hostname is the address a consumer actually needs.
	if svc.Host != "orders.example.com" {
		t.Errorf("публичный хост скрыт от анонима: %q", svc.Host)
	}
	// Адреса внутри кластера показываются намеренно: когда на одном публичном
	// адресе несколько записей, различить их иначе нельзя. Проверяются все
	// формы сразу — раньше открыли backend, а адрес опроса остался скрыт, и на
	// странице сервиса зияла пустота.
	for name, value := range map[string]string{
		"backend": svc.Backend, "namespace": svc.Namespace,
		"baseUrl": svc.BaseURL, "key": svc.Key,
	} {
		if value == "" {
			t.Errorf("аноним не видит %s", name)
		}
	}
	// Freshness is how a reader judges whether to trust the specification.
	if svc.LastCheckedAt == nil {
		t.Error("аноним не видит, когда сервис проверяли")
	}

	admin := decode[struct {
		Services []store.Service `json:"services"`
	}](t, e.do(t, "GET", "/api/services", "", true))
	if admin.Services[0].BaseURL == "" || admin.Services[0].LastError == "" {
		t.Error("администратор должен видеть внутренние адреса и текст ошибки")
	}
}

func TestOwnerIsPublic(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	e.store.UpdateService(context.Background(), e.ids["orders"], store.ServiceUpdate{
		Owner: &store.OwnerFields{Team: "Команда Профиля", Contact: "team@company.ru"},
	})

	body := decode[struct {
		Services []store.Service `json:"services"`
	}](t, e.do(t, "GET", "/api/services", "", false))
	// "Кого спросить про этот API" is the question a visitor comes with.
	if body.Services[0].Owner.Team != "Команда Профиля" {
		t.Errorf("владелец скрыт от анонима: %+v", body.Services[0].Owner)
	}
}

func TestWritesAndProxyNeedTheLogin(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	id := itoa(e.ids["orders"])

	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/api/services/" + id, `{"displayName":"x"}`},
		{"POST", "/api/services/" + id + "/probe", ""},
		{"GET", "/api/settings", ""},
		{"PUT", "/api/settings", `{}`},
		{"POST", "/api/sync", ""},
		{"POST", "/api/groups", `{"name":"g"}`},
		{"GET", "/proxy/" + id + "/anything", ""},
	} {
		if rec := e.do(t, c.method, c.path, c.body, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s без логина вернул %d, ожидался 401", c.method, c.path, rec.Code)
		}
	}

	// Reading stays open.
	for _, path := range []string{"/api/services", "/api/changes", "/api/groups", "/api/overview"} {
		if rec := e.do(t, "GET", path, "", false); rec.Code != http.StatusOK {
			t.Errorf("GET %s без логина вернул %d — чтение должно быть открыто", path, rec.Code)
		}
	}
}

// The login used to live in the page's sessionStorage, which is per-tab: a
// second tab started logged out. It is exchanged for a cookie now, so the
// browser shares it, and the password never has to be kept anywhere.
func TestLoginIssuesASessionUsableWithoutThePassword(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())

	rec := e.do(t, "POST", "/api/login", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("вход вернул %d: %s", rec.Code, rec.Body.String())
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "spectra_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("вход не поставил куку — вкладки по-прежнему не поделят сессию")
	}
	if !session.HttpOnly {
		t.Error("кука читается скриптом: смысл ухода от sessionStorage теряется")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Error("SameSite не Strict — это единственная защита от постороннего POST")
	}
	// The value must not carry the credentials it was exchanged for.
	if strings.Contains(session.Value, "secret") || strings.Contains(session.Value, "admin") {
		t.Errorf("в куке видны учётные данные: %q", session.Value)
	}

	// A write with the cookie and no password — this is the new tab's case.
	req := httptest.NewRequest("POST", "/api/services/"+itoa(e.ids["orders"])+"/probe", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("запись по сессии вернула %d, ожидался 202", rec.Code)
	}

	// And the overview must report the session, or the UI shows a login button
	// to someone who is already logged in.
	req = httptest.NewRequest("GET", "/api/overview", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if admin, _ := decode[map[string]any](t, rec)["admin"].(bool); !admin {
		t.Error("overview не признал сессию администратором")
	}
}

func TestForgedAndExpiredSessionsAreRejected(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	path := "/api/services/" + itoa(e.ids["orders"]) + "/probe"

	for name, value := range map[string]string{
		"выдуманная":         "9999999999.bm9wZQ",
		"без подписи":        "9999999999",
		"просроченная":       e.server.issueSession(time.Now().Add(-time.Minute)),
		"чужая подпись":      "9999999999." + strings.Repeat("A", 43),
		"пустая":             "",
		"подпись от прошлой": strings.Replace(e.server.issueSession(time.Now().Add(time.Hour)), ".", ".x", 1),
	} {
		req := httptest.NewRequest("POST", path, nil)
		req.AddCookie(&http.Cookie{Name: "spectra_session", Value: value})
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s кука вернула %d, ожидался 401", name, rec.Code)
		}
	}

	// A valid one still works, so the checks above are not rejecting everything.
	req := httptest.NewRequest("POST", path, nil)
	req.AddCookie(&http.Cookie{Name: "spectra_session",
		Value: e.server.issueSession(time.Now().Add(time.Hour))})
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("действующая сессия вернула %d", rec.Code)
	}
}

// Logging out has to end the session for every tab, not just the one that
// clicked.
func TestLogoutClearsTheCookie(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "POST", "/api/logout", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("выход вернул %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "spectra_session" && c.MaxAge >= 0 {
			t.Errorf("кука не погашена: MaxAge = %d", c.MaxAge)
		}
	}
}

func TestSpecIsServedByTheRevisionNumberPeopleSee(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService(), store.Discovered{
		Key: "billing.shop:80", DisplayName: "billing", BaseURL: "http://billing.shop:80",
	})
	ctx := context.Background()

	// Give billing a head start so primary keys and revision numbers diverge.
	for i := 0; i < 3; i++ {
		e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["billing"],
			Hash: string(rune('a' + i)), CapturedAt: time.Now(), Spec: []byte(`{"who":"billing"}`)})
	}
	e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["orders"],
		Hash: "o1", CapturedAt: time.Now(), Spec: []byte(spec)})

	rec := e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec?version=1", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("ревизия 1 не отдана: %d %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if info := got["info"].(map[string]any); info["title"] != "Orders" {
		t.Errorf("вернулась чужая ревизия: %v", info)
	}

	if rec := e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec?version=99", "", false); rec.Code != http.StatusNotFound {
		t.Errorf("несуществующая ревизия вернула %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec?version=abc", "", false); rec.Code != http.StatusBadRequest {
		t.Errorf("нечисловая ревизия вернула %d", rec.Code)
	}
}

func TestSpecServersArePointedAtTheProxy(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService(), store.Discovered{
		Key: "legacy.shop:80", DisplayName: "legacy", BaseURL: "http://legacy.shop:80",
	})
	ctx := context.Background()
	e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["orders"],
		Hash: "h", CapturedAt: time.Now(), Spec: []byte(spec)})
	e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["legacy"],
		Hash: "h", CapturedAt: time.Now(), Spec: []byte(swagger2)})

	got := decode[map[string]any](t, e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec", "", false))
	servers := got["servers"].([]any)
	if url := servers[0].(map[string]any)["url"]; url != "/proxy/"+itoa(e.ids["orders"]) {
		t.Errorf("servers = %v, «Try it out» пошёл бы напрямую в сервис", url)
	}

	// Swagger 2.0 has no servers array; the equivalent is host/basePath.
	legacy := decode[map[string]any](t, e.do(t, "GET", "/api/services/"+itoa(e.ids["legacy"])+"/spec", "", false))
	if legacy["basePath"] != "/proxy/"+itoa(e.ids["legacy"]) {
		t.Errorf("basePath = %v", legacy["basePath"])
	}
	if _, ok := legacy["host"]; ok {
		t.Error("host пережил переписывание — вьюер целился бы в сервис напрямую")
	}
}

// С выключенным прокси адресов в спеке не остаётся вовсе.
//
// Отправлять запрос всё равно некуда — портал его не пропустит, — а сам адрес
// обычно внутренний. Всё остальное про кластер анониму вырезается, и спека
// была единственным местом, где адрес возвращался обратно.
func TestProxyOffLeavesNoAddressInTheSpecification(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.HTTP.ProxyEnabled = false })
	e.seed(t, standardService(), store.Discovered{
		Key: "legacy.shop:80", DisplayName: "legacy", BaseURL: "http://legacy.shop:80",
	})
	ctx := context.Background()
	e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["orders"],
		Hash: "h", CapturedAt: time.Now(), Spec: []byte(spec)})
	e.store.InsertVersion(ctx, &store.Version{ServiceID: e.ids["legacy"],
		Hash: "h", CapturedAt: time.Now(), Spec: []byte(swagger2)})

	for _, admin := range []bool{false, true} {
		rec := e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec", "", admin)
		if strings.Contains(rec.Body.String(), "orders.internal") {
			t.Errorf("админ=%v: внутренний адрес отдан в спеке: %s", admin, rec.Body.String())
		}
		// Адрес не вырезается совсем, а заменяется публичным: без servers
		// вьюер подставляет адрес текущей страницы, и в примерах curl
		// оказывается сам портал.
		servers, ok := decode[map[string]any](t, rec)["servers"].([]any)
		if !ok || len(servers) != 1 {
			t.Fatalf("админ=%v: servers = %v", admin, decode[map[string]any](t, rec)["servers"])
		}
		if url := servers[0].(map[string]any)["url"]; url != "http://orders.example.com" {
			t.Errorf("админ=%v: адрес в спеке = %v, ожидался публичный хост", admin, url)
		}

		// Свагер 2.0: у него адрес разложен на host/basePath.
		legacy := decode[map[string]any](t, e.do(t, "GET", "/api/services/"+itoa(e.ids["legacy"])+"/spec", "", admin))
		if strings.Contains(fmt.Sprint(legacy), "legacy.internal") {
			t.Errorf("админ=%v: внутренний адрес пережил замену: %v", admin, legacy)
		}
	}

	// Сама спека при этом читается: заменены адреса, а не содержание.
	got := decode[map[string]any](t, e.do(t, "GET", "/api/services/"+itoa(e.ids["orders"])+"/spec", "", false))
	if _, ok := got["paths"].(map[string]any)["/orders"]; !ok {
		t.Error("вместе с адресами потерялись эндпоинты")
	}
}

// The address a person types is confined to the service's own host: otherwise
// a page anyone can reach becomes a request forger inside the cluster.
func TestManualSpecURLIsConfinedToTheService(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	id := itoa(e.ids["orders"])

	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://other.internal/openapi.json",
		"file:///etc/passwd",
	} {
		rec := e.do(t, "PATCH", "/api/services/"+id, `{"manualSpecUrl":"`+bad+`"}`, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s принят (%d)", bad, rec.Code)
		}
	}

	// Голый путь достраивается по адресу опроса; полный адрес сохраняется как
	// введён, вместе с хостом. Второе — потому что документация нередко
	// отдаётся только через ingress, и подмена хоста на внутренний приводила к
	// тому, что в ответе стоял адрес, по которому спеки нет.
	for _, c := range []struct{ typed, want string }{
		{"/v3/api-docs", "http://orders.shop:80/v3/api-docs"},
		{"http://orders.example.com/v3/api-docs", "http://orders.example.com/v3/api-docs"},
		{"https://orders.example.com/swagger.json", "https://orders.example.com/swagger.json"},
	} {
		rec := e.do(t, "PATCH", "/api/services/"+id, `{"manualSpecUrl":"`+c.typed+`"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s отклонён: %d %s", c.typed, rec.Code, rec.Body)
		}
		svc := decode[struct {
			Service store.Service `json:"service"`
		}](t, rec).Service
		if svc.SpecURL != c.want {
			t.Errorf("введено %q → сохранено %q, ожидалось %q", c.typed, svc.SpecURL, c.want)
		}
		if svc.ManualSpecURL != c.want {
			t.Errorf("введено %q → в поле ввода вернулось %q", c.typed, svc.ManualSpecURL)
		}
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t, nil)

	// A budget below the per-request timeout makes discovery impossible.
	rec := e.do(t, "PUT", "/api/settings",
		`{"requestTimeout":"30s","probeBudget":"10s","pollInterval":"1h","maxBackoff":"6h"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("бюджет меньше таймаута принят: %d", rec.Code)
	}
	// A backoff below the interval retries a broken service as often as a healthy one.
	rec = e.do(t, "PUT", "/api/settings",
		`{"requestTimeout":"10s","probeBudget":"2m","pollInterval":"1h","maxBackoff":"30m"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("потолок паузы меньше интервала принят: %d", rec.Code)
	}

	// Zero is meaningful for the probe delay and only for it.
	rec = e.do(t, "PUT", "/api/settings",
		`{"requestTimeout":"10s","probeBudget":"2m","pollInterval":"1h","maxBackoff":"6h","probeDelay":"0s"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("нулевая пауза отклонена: %d %s", rec.Code, rec.Body)
	}
	settings, err := e.store.LoadSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.ProbeDelay != 0 {
		t.Errorf("пауза = %v, а интерфейс обещает, что ноль отключает", settings.ProbeDelay)
	}
	rec = e.do(t, "PUT", "/api/settings",
		`{"requestTimeout":"10s","probeBudget":"2m","pollInterval":"0s","maxBackoff":"6h"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("нулевой интервал опроса принят: %d", rec.Code)
	}
}

func TestServiceEditing(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	id := itoa(e.ids["orders"])

	created := decode[struct {
		ID int64 `json:"id"`
	}](t, e.do(t, "POST", "/api/groups", `{"name":"Коммерция"}`, true))

	rec := e.do(t, "PATCH", "/api/services/"+id, `{
		"displayName":"Заказы","tags":["core","public"],"notes":"важный",
		"groupId":`+itoa(created.ID)+`,
		"owner":{"team":"Команда","contact":"t@c.ru","repo":"https://git/x","owners":["Иванов"]}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("правка отклонена: %d %s", rec.Code, rec.Body)
	}
	svc := decode[struct {
		Service store.Service `json:"service"`
	}](t, rec).Service
	if svc.DisplayName != "Заказы" || svc.GroupName != "Коммерция" || len(svc.Tags) != 2 {
		t.Errorf("правка применилась не полностью: %+v", svc)
	}
	if svc.Owner.Team != "Команда" || len(svc.Owner.Owners) != 1 {
		t.Errorf("владелец не сохранён: %+v", svc.Owner)
	}

	if rec := e.do(t, "PATCH", "/api/services/"+id, `{"reviewState":"выдумка"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("неизвестное состояние принято: %d", rec.Code)
	}
	// Hiding a host without an API is a normal decision, not an error.
	if rec := e.do(t, "PATCH", "/api/services/"+id, `{"reviewState":"ignored"}`, true); rec.Code != http.StatusOK {
		t.Errorf("сервис не удалось спрятать: %d", rec.Code)
	}
}

func TestUnknownServiceAndBadIDs(t *testing.T) {
	e := newEnv(t, nil)
	if rec := e.do(t, "GET", "/api/services/999999", "", false); rec.Code != http.StatusNotFound {
		t.Errorf("несуществующий сервис вернул %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/services/абв", "", false); rec.Code != http.StatusBadRequest {
		t.Errorf("нечисловой идентификатор вернул %d", rec.Code)
	}
}

// The proxy address comes from the registry, never from the request: the path
// only selects a service the portal already knows.
func TestProxyOnlyReachesRegisteredServices(t *testing.T) {
	var reached, gotCookie, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.URL.Path
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	e := newEnv(t, nil)
	e.seed(t, store.Discovered{Key: "k", DisplayName: "orders", BaseURL: upstream.URL})

	req := httptest.NewRequest("GET", "/proxy/"+itoa(e.ids["orders"])+"/orders?status=paid", nil)
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte("admin:secret")))
	req.AddCookie(&http.Cookie{Name: "spectra_session", Value: "secret"})
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || reached != "/orders" {
		t.Fatalf("прокси не доехал: код=%d путь=%q", rec.Code, reached)
	}
	if gotCookie != "" {
		t.Errorf("cookie портала утекли наверх: %q", gotCookie)
	}
	// The browser's own credentials are the portal login, not the service's.
	if strings.Contains(gotAuth, "YWRtaW46c2VjcmV0") {
		t.Error("логин портала ушёл в опрашиваемый сервис")
	}

	if rec := e.do(t, "GET", "/proxy/999999/anything", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("прокси к незарегистрированному сервису вернул %d", rec.Code)
	}
}

func TestProxyOffByConfiguration(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.HTTP.ProxyEnabled = false })
	e.seed(t, standardService())
	if rec := e.do(t, "GET", "/proxy/"+itoa(e.ids["orders"])+"/x", "", true); rec.Code != http.StatusForbidden {
		t.Errorf("прокси выключен, но вернул %d", rec.Code)
	}
}

func TestOverviewHidesOperationalDataFromAnonymous(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	e.store.RecordSyncFailure(context.Background(), "kubernetes", context.DeadlineExceeded)

	anon := decode[map[string]any](t, e.do(t, "GET", "/api/overview", "", false))
	if runs := anon["discoveryRuns"].([]any); len(runs) != 0 {
		t.Errorf("журнал сканирований виден анониму: %v", runs)
	}
	if anon["admin"] != false {
		t.Error("аноним помечен администратором")
	}

	admin := decode[map[string]any](t, e.do(t, "GET", "/api/overview", "", true))
	if runs := admin["discoveryRuns"].([]any); len(runs) == 0 {
		t.Error("администратор не видит журнал сканирований")
	}
}

func TestUIIsServed(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/", "/app.js", "/style.css", "/viewer.html"} {
		if rec := e.do(t, "GET", path, "", false); rec.Code != http.StatusOK {
			t.Errorf("%s вернул %d", path, rec.Code)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}

// A token the user supplied for the API itself must survive: stripping every
// Authorization header would break "Try it out" against a protected endpoint.
func TestProxyForwardsTheUsersOwnToken(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	e := newEnv(t, nil)
	e.seed(t, store.Discovered{Key: "k", DisplayName: "orders", BaseURL: upstream.URL})

	// Logged in through the browser dialog, but sending an API token of their
	// own — that is what the viewer does.
	req := httptest.NewRequest("GET", "/proxy/"+itoa(e.ids["orders"])+"/orders", nil)
	req.Header.Set("Authorization", "Bearer user-api-token")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	// Without the portal login this is refused before it reaches upstream.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("прокси без логина портала вернул %d", rec.Code)
	}
	if gotAuth != "" {
		t.Error("запрос доехал до сервиса без входа в портал")
	}
}

func TestMetricsExposeWhatYouWouldAlertOn(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t, standardService())
	ctx := context.Background()
	e.store.RecordProbe(ctx, store.ProbeResult{ServiceID: e.ids["orders"],
		Status: store.ProbeOK, Succeeded: true, AutoProbe: true, CheckedAt: time.Now()})
	e.store.RecordSyncFailure(ctx, "kubernetes", context.DeadlineExceeded)

	rec := e.do(t, "GET", "/metrics", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"spectra_up 1",
		`spectra_services{state="ok"} 1`,
		"spectra_services_without_owner",
		"spectra_services_manual_only",
		// The alert that matters most: discovery quietly failing while the
		// portal keeps serving its cached list.
		`spectra_discovery_last_failure_timestamp_seconds{source="kubernetes"}`,
		"spectra_oldest_successful_check_timestamp_seconds",
		"spectra_spec_revisions_total",
		"spectra_poller_active",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q", want)
		}
	}
	// Every series needs its HELP/TYPE header or scrapers complain.
	if strings.Count(body, "# TYPE") < 8 {
		t.Errorf("метрики без объявлений типов:\n%s", body)
	}
	// Addresses of watched services must not leak through metric labels.
	for _, forbidden := range []string{"orders.shop", "orders.example.com", "shop"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("в метриках утёк адрес сервиса: %q", forbidden)
		}
	}
}

// Публичный адрес выбирается по хосту, а не по порядку полей: какой из двух
// адресов публичный, зависит от SPECTRA_K8S_TARGET.
func TestPublicAddressIsTheOneBuiltOnTheHost(t *testing.T) {
	cases := []struct {
		name string
		svc  store.Service
		want string
	}{
		{"опрос внутрь, публичный — запасной",
			store.Service{Host: "api.example.com", BaseURL: "http://api.shop:8080",
				FallbackURL: "https://api.example.com"}, "https://api.example.com"},
		{"опрос наружу, публичный — основной",
			store.Service{Host: "api.example.com", BaseURL: "https://api.example.com",
				FallbackURL: "http://api.shop:8080"}, "https://api.example.com"},
		{"с префиксом пути",
			store.Service{Host: "api.example.com", BaseURL: "http://api.shop:8080/v2",
				FallbackURL: "https://api.example.com/v2"}, "https://api.example.com/v2"},
		{"запасного ещё нет — схема от известного адреса",
			store.Service{Host: "app.service1", BaseURL: "http://app.service1:8080"},
			"http://app.service1:8080"},
		{"хоста нет вовсе",
			store.Service{BaseURL: "http://app.ns:8080"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := publicAddress(c.svc); got != c.want {
				t.Errorf("публичный адрес = %q, ожидался %q", got, c.want)
			}
		})
	}
}

// Внутренний адрес не должен попасть в спеку ни при каком раскладе полей.
func TestPublicAddressNeverReturnsTheInClusterOne(t *testing.T) {
	svc := store.Service{Host: "api.example.com", BaseURL: "http://api.shop:8080",
		FallbackURL: "https://api.example.com"}
	if got := publicAddress(svc); strings.Contains(got, "api.shop") {
		t.Errorf("в спеку ушёл внутренний адрес: %q", got)
	}
}

// schemaName строит имя схемы Postgres из имени теста.
//
// Имя теста — не идентификатор: в нём слэши от подтестов и, здесь, кириллица.
// Обрезание по байтам разрывало символ пополам, и Postgres отвергал строку
// («invalid byte sequence for encoding UTF8»). Поэтому имя сводится к ASCII, а
// уникальность даёт хэш от исходного.
func schemaName(prefix, testName string) string {
	var b strings.Builder
	for _, r := range testName {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	ascii := b.String()
	if len(ascii) > 24 {
		ascii = ascii[:24]
	}
	sum := fnv.New32a()
	sum.Write([]byte(testName))
	return fmt.Sprintf("%s%s_%08x", prefix, ascii, sum.Sum32())
}

// Страница, скрипт и стили должны перепроверяться браузером при каждом заходе.
//
// Без заголовка браузер кеширует их по своему усмотрению — файлы отдаются с
// Last-Modified и без явных указаний, — и после обновления портала человек
// видит старый интерфейс. Проверено на живом стенде: обновлённый CSS лежал на
// сервере, а в браузере оставался прежний.
func TestStaticIsRevalidatedButVendorIsNot(t *testing.T) {
	e := newEnv(t, nil)

	for _, path := range []string{"/", "/index.html", "/app.js", "/style.css"} {
		rec := e.do(t, "GET", path, "", false)
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, ожидался no-cache", path, got)
		}
	}

	// У вендоренного вьюера содержимое меняется только вместе с версией
	// портала, а весит он мегабайты — его держать в кеше можно и нужно.
	rec := e.do(t, "GET", "/vendor/scalar.standalone.js", "", false)
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("вьюер отдаётся без кеширования: %q", got)
	}
}
