package collector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spectra/internal/config"
	"spectra/internal/store"
)

// The collector is mostly about what it does *not* do — not re-poll, not
// double-poll, not record a revision — so the tests count requests and rows
// against a real Postgres and real HTTP servers rather than mocks.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("SPECTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SPECTRA_TEST_DATABASE_URL не задан — пропускаю тесты, которым нужен Postgres")
	}
	schema := strings.ReplaceAll("c_"+t.Name(), "/", "_")
	if len(schema) > 40 {
		schema = schema[:40]
	}
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
		t.Fatalf("создание схемы: %v", err)
	}
	bootstrap.Close()

	s, err := store.Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("открытие store: %v", err)
	}
	t.Cleanup(func() {
		if drop, err := store.Open(context.Background(), dsn); err == nil {
			drop.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			drop.Close()
		}
		s.Close()
	})
	return s
}

type harness struct {
	collector *Collector
	store     *store.Store
	settings  store.Settings
}

func newHarness(t *testing.T, tune func(*store.Settings)) *harness {
	t.Helper()
	st := testStore(t)

	settings := store.DefaultSettings()
	settings.ProbeDelay = 0 // pacing is tested on its own; here it only slows things down
	settings.RequestTimeout = 3 * time.Second
	settings.ProbeBudget = 20 * time.Second
	if tune != nil {
		tune(&settings)
	}
	if err := st.SaveSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		PollerEnabled: true,
		HTTP:          config.HTTP{UserAgent: "spectra-test"},
		Kubernetes:    config.Kubernetes{SyncInterval: time.Hour},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &harness{collector: New(cfg, st, nil, log), store: st, settings: settings}
}

// register puts one service into the registry and returns its id.
func (h *harness) register(t *testing.T, name, baseURL string) int64 {
	t.Helper()
	if _, err := h.store.SyncDiscovered(context.Background(), "test", []store.Discovered{{
		Key: name, DisplayName: name, BaseURL: baseURL, Host: name + ".example.com",
	}}); err != nil {
		t.Fatal(err)
	}
	services, err := h.store.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		if s.DisplayName == name {
			return s.ID
		}
	}
	t.Fatalf("сервис %s не зарегистрирован", name)
	return 0
}

func (h *harness) poll(t *testing.T) {
	t.Helper()
	h.collector.PollDue(context.Background())
}

func (h *harness) service(t *testing.T, id int64) store.Service {
	t.Helper()
	svc, err := h.store.GetService(context.Background(), id)
	if err != nil || svc == nil {
		t.Fatalf("сервис %d не найден: %v", id, err)
	}
	return *svc
}

const spec = `{"openapi":"3.0.3","info":{"title":"T","version":"1.0.0"},
  "paths":{"/ping":{"get":{"operationId":"ping","responses":{"200":{"description":"OK"}}}}}}`

const specPlus = `{"openapi":"3.0.3","info":{"title":"T","version":"1.1.0"},
  "paths":{"/ping":{"get":{"operationId":"ping","responses":{"200":{"description":"OK"}}}},
           "/pong":{"get":{"operationId":"pong","responses":{"200":{"description":"OK"}}}}}}`

// specServer answers on /openapi.json with whatever body points at.
func specServer(t *testing.T, body *string, etag func() string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var full atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.json" {
			http.NotFound(w, r)
			return
		}
		if etag != nil {
			tag := etag()
			w.Header().Set("ETag", tag)
			if r.Header.Get("If-None-Match") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		full.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, *body)
	}))
	t.Cleanup(srv.Close)
	return srv, &full
}

func TestRevisionOnlyWhenTheContractMoves(t *testing.T) {
	body := spec
	h := newHarness(t, nil)
	srv, _ := specServer(t, &body, nil)
	id := h.register(t, "orders", srv.URL)

	h.poll(t)
	h.poll(t)
	h.poll(t)

	versions, err := h.store.ListVersions(context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("опрос без изменений создал %d ревизий — «обновлено» стало бы "+
			"означать «мы недавно спрашивали»", len(versions))
	}
	if versions[0].EndpointCount != 1 {
		t.Errorf("эндпоинтов = %d", versions[0].EndpointCount)
	}

	body = specPlus
	h.poll(t)
	versions, _ = h.store.ListVersions(context.Background(), id, 10)
	if len(versions) != 2 || versions[0].Revision != 2 {
		t.Fatalf("настоящее изменение не записано: %+v", versions)
	}

	changes, _ := h.store.ListChanges(context.Background(), id, 10)
	if len(changes) != 2 || changes[0].Added != 1 {
		t.Errorf("лента изменений: %+v", changes)
	}
}

// The regression that made every second poll download the whole document: the
// 304 branch wrote empty validators back and erased the stored ETag.
func TestNotModifiedKeepsTheValidator(t *testing.T) {
	body := spec
	h := newHarness(t, nil)
	srv, full := specServer(t, &body, func() string { return `"v1"` })
	id := h.register(t, "orders", srv.URL)

	h.poll(t)
	h.poll(t)
	h.poll(t)

	if got := full.Load(); got != 1 {
		t.Errorf("тело скачано %d раз из трёх опросов — условные запросы не работают", got)
	}
	if svc := h.service(t, id); svc.ETag != `"v1"` {
		t.Errorf("валидатор потерян между опросами: %q", svc.ETag)
	}
}

// A service that answers but publishes nothing will not grow a specification
// on its own, so it leaves the schedule entirely.
func TestNoSpecLeavesTheSchedule(t *testing.T) {
	h := newHarness(t, nil)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	id := h.register(t, "website", srv.URL)

	h.poll(t)
	afterFirst := requests.Load()
	if afterFirst == 0 {
		t.Fatal("первый опрос не сделал ни одного запроса")
	}

	h.poll(t)
	h.poll(t)
	if got := requests.Load(); got != afterFirst {
		t.Errorf("запросов стало %d вместо %d: сервис без спеки продолжают опрашивать",
			got, afterFirst)
	}

	svc := h.service(t, id)
	if svc.ProbeStatus != store.ProbeNoSpec || svc.AutoProbe {
		t.Errorf("состояние сервиса без спеки: статус=%q автоопрос=%v", svc.ProbeStatus, svc.AutoProbe)
	}
}

// An unreachable service is usually a temporary outage, so it stays on the
// schedule and only backs off.
func TestUnreachableBacksOffButStays(t *testing.T) {
	h := newHarness(t, nil)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	id := h.register(t, "down", url)

	h.poll(t)
	svc := h.service(t, id)
	if svc.ProbeStatus != store.ProbeUnreachable {
		t.Errorf("статус = %q, ожидался «недоступен»", svc.ProbeStatus)
	}
	if !svc.AutoProbe {
		t.Error("недоступный сервис исключён из расписания — но это обычно временно")
	}
	if svc.NextProbeAt == nil || !svc.NextProbeAt.After(time.Now()) {
		t.Error("пауза до следующей попытки не назначена")
	}
}

// A human asking for a retry must get one whatever happened before.
func TestManualProbeReturnsAServiceToTheSchedule(t *testing.T) {
	body := spec
	h := newHarness(t, nil)

	var serve atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serve.Load() || r.URL.Path != "/openapi.json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	id := h.register(t, "orders", srv.URL)

	h.poll(t)
	if svc := h.service(t, id); svc.AutoProbe {
		t.Fatal("сервис без спеки остался в расписании")
	}

	// Someone fixes the service and presses "probe now".
	serve.Store(true)
	h.collector.Refresh(context.Background(), id)
	h.poll(t)

	svc := h.service(t, id)
	if svc.ProbeStatus != store.ProbeOK {
		t.Fatalf("после ручного опроса статус = %q", svc.ProbeStatus)
	}
	if !svc.AutoProbe {
		t.Error("успешный опрос не вернул сервис в автоматическое расписание")
	}
}

// Returning to a contract seen before is a rolled-back deploy, and the feed
// must say so instead of re-listing endpoints as if they had just moved.
func TestRollbackIsLabelled(t *testing.T) {
	body := spec
	h := newHarness(t, nil)
	srv, _ := specServer(t, &body, nil)
	id := h.register(t, "orders", srv.URL)

	h.poll(t)
	body = specPlus
	h.poll(t)
	body = spec // выкатку откатили
	h.poll(t)

	versions, _ := h.store.ListVersions(context.Background(), id, 10)
	if len(versions) != 3 {
		t.Fatalf("ревизий %d, ожидалось 3", len(versions))
	}
	if versions[0].RollbackOf == nil || *versions[0].RollbackOf != 1 {
		t.Fatalf("ревизия 3 не помечена откатом к первой: %+v", versions[0].RollbackOf)
	}

	changes, _ := h.store.ListChanges(context.Background(), id, 10)
	if !strings.Contains(string(changes[0].Diff), "spec.rollback") {
		t.Errorf("в ленте нет пометки об откате: %s", changes[0].Diff)
	}
}

func TestRetentionCapsHistory(t *testing.T) {
	body := spec
	h := newHarness(t, func(s *store.Settings) { s.RetainVersions = 3 })
	srv, _ := specServer(t, &body, nil)
	id := h.register(t, "orders", srv.URL)

	for i := 0; i < 6; i++ {
		body = strings.Replace(spec, `"1.0.0"`, `"1.0.`+string(rune('0'+i))+`"`, 1)
		h.poll(t)
	}

	versions, _ := h.store.ListVersions(context.Background(), id, 100)
	if len(versions) != 3 {
		t.Errorf("хранится %d ревизий при лимите 3 — база росла бы без предела", len(versions))
	}
}

// Frameworks move their documentation across major upgrades; a spec URL that
// stops working must be rediscovered rather than reported as a broken service.
func TestMovedSpecIsRediscovered(t *testing.T) {
	h := newHarness(t, nil)
	var moved atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v3/api-docs"
		if moved.Load() {
			want = "/openapi.json"
		}
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, spec)
	}))
	defer srv.Close()
	id := h.register(t, "orders", srv.URL)

	h.poll(t)
	if svc := h.service(t, id); !strings.HasSuffix(svc.SpecURL, "/v3/api-docs") {
		t.Fatalf("первый адрес = %q", svc.SpecURL)
	}

	moved.Store(true)
	h.poll(t)

	svc := h.service(t, id)
	if !strings.HasSuffix(svc.SpecURL, "/openapi.json") {
		t.Errorf("адрес после переезда = %q", svc.SpecURL)
	}
	if svc.ProbeStatus != store.ProbeOK {
		t.Errorf("статус после переезда = %q", svc.ProbeStatus)
	}
}

// Contacts declared in a specification are picked up on every successful poll:
// a team may add them without touching a single endpoint.
func TestOwnerIsReadFromTheSpecification(t *testing.T) {
	body := `{"openapi":"3.0.3","info":{"title":"T","version":"1",
	  "contact":{"name":"Команда Профиля","email":"profile@company.ru"},
	  "x-repo":"https://gitlab/profile"},
	  "paths":{"/ping":{"get":{"responses":{"200":{"description":"OK"}}}}}}`
	h := newHarness(t, nil)
	srv, _ := specServer(t, &body, nil)
	id := h.register(t, "orders", srv.URL)

	h.poll(t)

	svc := h.service(t, id)
	if svc.Owner.Team != "Команда Профиля" || svc.Owner.Repo != "https://gitlab/profile" {
		t.Errorf("владелец из спеки не сохранён: %+v", svc.Owner)
	}
	if svc.Owner.Source != "spec" {
		t.Errorf("источник владельца = %q", svc.Owner.Source)
	}
}

// Discovery fires dozens of requests; without pacing they land inside one
// second, which is indistinguishable from a scan.
func TestProbesArePaced(t *testing.T) {
	h := newHarness(t, func(s *store.Settings) { s.ProbeDelay = 40 * time.Millisecond })
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	h.register(t, "website", srv.URL)

	started := time.Now()
	h.poll(t)
	elapsed := time.Since(started)

	n := requests.Load()
	if n < 10 {
		t.Fatalf("запросов всего %d — перебор не состоялся", n)
	}
	// Without pacing this whole sweep would finish in milliseconds.
	if elapsed < 200*time.Millisecond {
		t.Errorf("%d запросов уложились в %v — пауза между попытками не работает", n, elapsed)
	}
}

// The budget covers a whole discovery sweep; reusing the per-request timeout
// for it would make a slow but healthy service undiscoverable.
func TestSlowServiceStillGetsDiscovered(t *testing.T) {
	h := newHarness(t, func(s *store.Settings) {
		s.RequestTimeout = 2 * time.Second
		s.ProbeBudget = 30 * time.Second
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		if r.URL.Path != "/docs/openapi.json" { // намеренно в конце списка кандидатов
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, spec)
	}))
	defer srv.Close()
	id := h.register(t, "slow", srv.URL)

	h.poll(t)

	svc := h.service(t, id)
	if svc.ProbeStatus != store.ProbeOK {
		t.Fatalf("медленный, но живой сервис получил статус %q (%s)", svc.ProbeStatus, svc.LastError)
	}
}

// Two sweeps overlapping would double the requests every watched service sees.
func TestSweepsDoNotOverlap(t *testing.T) {
	h := newHarness(t, nil)
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		if r.URL.Path != "/openapi.json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, spec)
	}))
	defer srv.Close()
	h.register(t, "orders", srv.URL)

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			h.collector.PollDue(context.Background())
			done <- struct{}{}
		}()
	}
	<-done
	<-done

	// One service, one sweep at a time: never two requests at once.
	if peak.Load() > 1 {
		t.Errorf("одновременных запросов к сервису: %d — обходы наложились", peak.Load())
	}
}
