// Package api exposes the registry over HTTP and serves the UI.
//
// Unlike the previous version this API is read-write: the registry lives in
// the database now, so grouping, tagging, ignoring a host and supplying a spec
// URL all happen here. That makes authentication in front of the portal a
// requirement rather than a recommendation — see the README.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"spectra/internal/collector"
	"spectra/internal/config"
	"spectra/internal/discovery"
	"spectra/internal/openapi"
	"spectra/internal/store"
	"spectra/internal/web"
)

type Server struct {
	cfg       *config.Config
	store     *store.Store
	collector *collector.Collector
	log       *slog.Logger
}

func New(cfg *config.Config, st *store.Store, col *collector.Collector, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, collector: col, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	// Metrics describe the portal's own work and carry no service addresses,
	// so they need no login — but they do belong behind your ingress.
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/overview", s.overview)

	// Public: looking things up. Anyone who reaches the portal may read.
	mux.HandleFunc("GET /api/services", s.listServices)
	mux.HandleFunc("GET /api/services/{id}", s.getService)
	mux.HandleFunc("GET /api/services/{id}/spec", s.getSpec)
	mux.HandleFunc("GET /api/changes", s.listChanges)
	mux.HandleFunc("GET /api/groups", s.listGroups)
	mux.HandleFunc("GET /api/tags", s.listTags)

	// Behind a login: anything that changes state or acts on a service.
	// Вход обменивает логин с паролем на сессионную куку: браузер делит её
	// между вкладками сам, и странице больше не нужно хранить пароль.
	mux.HandleFunc("POST /api/login", s.requireAdmin(s.login))
	mux.HandleFunc("POST /api/logout", s.logout)

	mux.HandleFunc("PATCH /api/services/{id}", s.requireAdmin(s.patchService))
	mux.HandleFunc("POST /api/services/{id}/probe", s.requireAdmin(s.probeService))
	mux.HandleFunc("POST /api/groups", s.requireAdmin(s.createGroup))
	mux.HandleFunc("DELETE /api/groups/{id}", s.requireAdmin(s.deleteGroup))
	mux.HandleFunc("GET /api/settings", s.requireAdmin(s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.requireAdmin(s.putSettings))
	mux.HandleFunc("POST /api/sync", s.requireAdmin(s.syncNow))

	// The proxy sends real requests to internal services from inside the
	// cluster. Reading documentation is public; calling an API through the
	// portal is not.
	mux.Handle("/proxy/{id}/", s.requireAdminHandler(s.proxyHandler()))
	mux.Handle("/", staticCache(http.FileServer(http.FS(web.FS()))))

	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// overview backs the header: how many services, how many still unreviewed, and
// whether cluster discovery is actually working.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := s.store.Counts(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	admin := s.isAdmin(r)

	// Sweep records carry failure messages that quote internal addresses, and
	// they are only rendered on the settings page anyway.
	runs := []store.DiscoveryRun{}
	if admin {
		runs, err = s.store.ListDiscoveryRuns(ctx, 10)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	settings, _ := s.store.LoadSettings(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"counts":        counts,
		"discoveryRuns": runs,
		"pollInterval":  settings.PollInterval.String(),
		"kubernetes":    s.cfg.Kubernetes.Enabled,
		"proxyEnabled":  s.cfg.HTTP.ProxyEnabled,
		"admin":         admin,
		"adminEnabled":  s.cfg.Admin.User != "" && s.cfg.Admin.Password != "",
	})
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.store.ListServices(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !s.isAdmin(r) {
		services = redactServices(services)
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc, err := s.service(w, r)
	if svc == nil {
		return
	}
	if !s.isAdmin(r) {
		redacted := redactService(*svc)
		svc = &redacted
	}
	versions, err := s.store.ListVersions(ctx, svc.ID, 50)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	changes, err := s.store.ListChanges(ctx, svc.ID, 50)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": svc, "versions": versions, "changes": changes,
	})
}

type servicePatch struct {
	DisplayName   *string    `json:"displayName"`
	GroupID       *int64     `json:"groupId"`
	ClearGroup    bool       `json:"clearGroup"`
	Tags          *[]string  `json:"tags"`
	Notes         *string    `json:"notes"`
	ReviewState   *string    `json:"reviewState"`
	ManualSpecURL *string    `json:"manualSpecUrl"`
	Owner         *ownerBody `json:"owner"`
}

type ownerBody struct {
	Team    string   `json:"team"`
	Contact string   `json:"contact"`
	Repo    string   `json:"repo"`
	Owners  []string `json:"owners"`
}

func (s *Server) patchService(w http.ResponseWriter, r *http.Request) {
	svc := s.mustService(w, r)
	if svc == nil {
		return
	}
	var body servicePatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	update := store.ServiceUpdate{
		DisplayName: body.DisplayName,
		Tags:        body.Tags,
		Notes:       body.Notes,
	}
	if body.ClearGroup {
		var none *int64
		update.GroupID = &none
	} else if body.GroupID != nil {
		id := body.GroupID
		update.GroupID = &id
	}
	if body.ReviewState != nil {
		switch *body.ReviewState {
		case store.ReviewReviewed, store.ReviewIgnored:
			update.ReviewState = body.ReviewState
		default:
			s.fail(w, http.StatusBadRequest, errors.New("неизвестное состояние: "+*body.ReviewState))
			return
		}
	}
	if body.Owner != nil {
		owners := body.Owner.Owners
		if owners == nil {
			owners = []string{}
		}
		update.Owner = &store.OwnerFields{
			Team:    strings.TrimSpace(body.Owner.Team),
			Contact: strings.TrimSpace(body.Owner.Contact),
			Repo:    strings.TrimSpace(body.Owner.Repo),
			Owners:  owners,
		}
	}
	if body.ManualSpecURL != nil {
		// The address is confined to the service's own host. Anything else
		// would turn this endpoint into a request forger running inside the
		// cluster; see discovery.ResolveManualSpecURL.
		resolved, err := discovery.ResolveManualSpecURL(svc.BaseURL, svc.Host, *body.ManualSpecURL)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		update.ManualSpecURL = &resolved
	}

	if err := s.store.UpdateService(r.Context(), svc.ID, update); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	updated, _ := s.store.GetService(r.Context(), svc.ID)
	writeJSON(w, http.StatusOK, map[string]any{"service": updated})
}

// probeService runs a poll for one service right now, ignoring the backoff.
func (s *Server) probeService(w http.ResponseWriter, r *http.Request) {
	svc := s.mustService(w, r)
	if svc == nil {
		return
	}
	s.collector.Refresh(r.Context(), svc.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

func (s *Server) getSpec(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc := s.mustService(w, r)
	if svc == nil {
		return
	}

	var (
		version *store.Version
		err     error
	)
	if raw := r.URL.Query().Get("version"); raw != "" {
		// The number in the link is the one shown on the page: revisions are
		// numbered within a service.
		revision, convErr := strconv.Atoi(raw)
		if convErr != nil || revision <= 0 {
			s.fail(w, http.StatusBadRequest, errors.New("номер ревизии должен быть положительным числом"))
			return
		}
		version, err = s.store.GetVersion(ctx, svc.ID, revision)
	} else {
		version, err = s.store.LatestVersion(ctx, svc.ID)
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if version == nil {
		s.fail(w, http.StatusNotFound, errors.New("спецификация для этого сервиса ещё не собрана"))
		return
	}

	// Адрес в спеке всегда переписывается под то, что портал готов показать.
	//
	// С включённым прокси это сам портал — тогда «Try it out» идёт через него.
	// С выключенным — публичный адрес сервиса: отправлять запросы порталу
	// нельзя, но документация обязана честно называть, где живёт этот API.
	// Внутренний адрес из спеки не годится ни в том, ни в другом случае: он
	// недостижим снаружи и не показывается анонимному читателю.
	//
	// Оставить спеку вовсе без адресов тоже нельзя: вьюер подставляет вместо
	// них адрес текущей страницы, и в примерах curl оказывается сам портал.
	body := version.Spec
	if doc, parseErr := openapi.Parse(version.Spec); parseErr == nil {
		switch {
		case s.cfg.HTTP.ProxyEnabled:
			doc.SetProxyServers("/proxy/"+strconv.FormatInt(svc.ID, 10), "Через портал")
		case publicAddress(*svc) != "":
			doc.SetProxyServers(publicAddress(*svc), "Адрес сервиса")
		default:
			doc.StripServers()
		}
		if rewritten, marshalErr := doc.JSON(); marshalErr == nil {
			body = rewritten
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", `"`+version.Hash+`"`)
	w.Write(body)
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	var serviceID int64
	if raw := r.URL.Query().Get("service"); raw != "" {
		serviceID, _ = strconv.ParseInt(raw, 10, 64)
	}
	changes, err := s.store.ListChanges(r.Context(), serviceID, intParam(r, "limit", 60, 500))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Position int    `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		s.fail(w, http.StatusBadRequest, errors.New("нужно указать название группы"))
		return
	}
	id, err := s.store.CreateGroup(r.Context(), strings.TrimSpace(body.Name), body.Position)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteGroup(r.Context(), id); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.AllTags(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.LoadSettings(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsJSON(settings))
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PollInterval   string   `json:"pollInterval"`
		RequestTimeout string   `json:"requestTimeout"`
		ProbeBudget    string   `json:"probeBudget"`
		ProbeDelay     string   `json:"probeDelay"`
		MaxBackoff     string   `json:"maxBackoff"`
		Concurrency    int      `json:"concurrency"`
		RetainVersions int      `json:"retainVersions"`
		IgnoreServers  bool     `json:"ignoreServers"`
		IgnoreFields   []string `json:"ignoreFields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	settings := store.DefaultSettings()
	// allowZero is true only for the probe delay, where zero means "do not
	// pace" — everywhere else zero would mean "never" or "immediately".
	parse := func(name, raw string, target *time.Duration, allowZero bool) error {
		if raw == "" {
			return nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 || (d == 0 && !allowZero) {
			return errors.New(name + ": ожидается длительность, например 1h или 30s")
		}
		*target = d
		return nil
	}
	for _, item := range []struct {
		name, raw string
		target    *time.Duration
		allowZero bool
	}{
		{"pollInterval", body.PollInterval, &settings.PollInterval, false},
		{"requestTimeout", body.RequestTimeout, &settings.RequestTimeout, false},
		{"probeBudget", body.ProbeBudget, &settings.ProbeBudget, false},
		{"probeDelay", body.ProbeDelay, &settings.ProbeDelay, true},
		{"maxBackoff", body.MaxBackoff, &settings.MaxBackoff, false},
	} {
		if err := parse(item.name, item.raw, item.target, item.allowZero); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
	}
	// A budget below the per-request timeout makes discovery impossible; a
	// backoff below the interval means a failing service is retried as often
	// as a healthy one.
	if settings.ProbeBudget < settings.RequestTimeout {
		s.fail(w, http.StatusBadRequest, errors.New("бюджет на опрос должен быть не меньше таймаута запроса"))
		return
	}
	if settings.MaxBackoff < settings.PollInterval {
		s.fail(w, http.StatusBadRequest, errors.New("потолок паузы должен быть не меньше интервала опроса"))
		return
	}
	if body.Concurrency > 0 {
		settings.Concurrency = body.Concurrency
	}
	if body.RetainVersions > 0 {
		settings.RetainVersions = body.RetainVersions
	}
	settings.IgnoreServers = body.IgnoreServers
	settings.IgnoreFields = body.IgnoreFields

	if err := s.store.SaveSettings(r.Context(), settings); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsJSON(settings))
}

func settingsJSON(v store.Settings) map[string]any {
	fields := v.IgnoreFields
	if fields == nil {
		fields = []string{}
	}
	return map[string]any{
		"pollInterval":   v.PollInterval.String(),
		"requestTimeout": v.RequestTimeout.String(),
		"probeBudget":    v.ProbeBudget.String(),
		"probeDelay":     v.ProbeDelay.String(),
		"maxBackoff":     v.MaxBackoff.String(),
		"concurrency":    v.Concurrency,
		"retainVersions": v.RetainVersions,
		"ignoreServers":  v.IgnoreServers,
		"ignoreFields":   fields,
	}
}

func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	s.collector.SyncNow()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// service resolves the {id} path value, writing the error response itself.
func (s *Server) service(w http.ResponseWriter, r *http.Request) (*store.Service, error) {
	return s.mustService(w, r), nil
}

func (s *Server) mustService(w http.ResponseWriter, r *http.Request) *store.Service {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("идентификатор сервиса должен быть числом"))
		return nil
	}
	svc, err := s.store.GetService(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return nil
	}
	if svc == nil {
		s.fail(w, http.StatusNotFound, errors.New("сервис не найден"))
		return nil
	}
	return svc
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func intParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		next.ServeHTTP(w, r)
	})
}
