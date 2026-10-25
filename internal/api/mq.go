package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"spectra/internal/rabbitmq"
	"spectra/internal/store"
)

// mqTopology returns the last snapshot of every configured broker. A broker
// that has not been reached yet is listed anyway, without a snapshot, so the
// page can say "ещё не опрашивался" instead of pretending it does not exist.
func (s *Server) mqTopology(w http.ResponseWriter, r *http.Request) {
	states, err := s.store.ListMQStates(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	byCluster := map[string]store.MQState{}
	for _, st := range states {
		byCluster[st.Cluster] = st
	}
	admin := s.isAdmin(r)

	brokers := make([]map[string]any, 0, len(s.cfg.RabbitMQ))
	for _, b := range s.cfg.RabbitMQ {
		entry := map[string]any{"cluster": b.Name}
		if st, ok := byCluster[b.Name]; ok {
			entry["capturedAt"] = st.CapturedAt
			entry["lastCheckedAt"] = st.LastCheckedAt
			entry["lastSuccessAt"] = st.LastSuccessAt
			// The error text quotes the broker's address and the account the
			// portal uses; a visitor only needs to know the data is stale.
			entry["failing"] = st.LastError != ""
			if admin {
				entry["lastError"] = st.LastError
			}
			if st.Snapshot != "" {
				var snap rabbitmq.Snapshot
				if err := json.Unmarshal([]byte(st.Snapshot), &snap); err == nil {
					if !admin {
						redactSnapshot(&snap)
					}
					entry["snapshot"] = snap
				}
			}
		}
		brokers = append(brokers, entry)
	}
	declared, err := s.store.ListMQDeclared(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": len(s.cfg.RabbitMQ) > 0, "brokers": brokers, "declared": declared,
	})
}

var declaredModes = map[string]bool{"": true, "subscribe": true, "schedule": true, "get": true, "manual": true}

// mqDeclare records a link a person knows about and the broker cannot show:
// "очередь читает такой-то сервис раз в сутки". Only manual links are written
// here; code-derived ones will come from the CI scanner.
func (s *Server) mqDeclare(w http.ResponseWriter, r *http.Request) {
	var d store.MQDeclared
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	d.Service, d.Note = strings.TrimSpace(d.Service), strings.TrimSpace(d.Note)
	known := false
	for _, b := range s.cfg.RabbitMQ {
		known = known || b.Name == d.Cluster
	}
	switch {
	case !known:
		s.fail(w, http.StatusBadRequest, errors.New("такого брокера нет в SPECTRA_RABBITMQ_BROKERS"))
		return
	case d.Vhost == "" || d.Object == "":
		s.fail(w, http.StatusBadRequest, errors.New("нужно указать vhost и очередь или exchange"))
		return
	case !(d.ObjectKind == "queue" && d.Role == "consume") && !(d.ObjectKind == "exchange" && d.Role == "publish"):
		s.fail(w, http.StatusBadRequest, errors.New("очередь читают, в exchange публикуют: objectKind и role не сочетаются"))
		return
	case d.Service == "" || len(d.Service) > 200:
		s.fail(w, http.StatusBadRequest, errors.New("нужно указать сервис"))
		return
	case !declaredModes[d.Mode]:
		s.fail(w, http.StatusBadRequest, errors.New("mode: subscribe, schedule, get или manual"))
		return
	case len(d.Note) > 500:
		s.fail(w, http.StatusBadRequest, errors.New("комментарий длиннее 500 символов"))
		return
	}
	d.Source = "manual"
	id, err := s.store.SaveMQDeclared(r.Context(), d)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) mqUndeclare(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	deleted, err := s.store.DeleteMQDeclared(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !deleted {
		s.fail(w, http.StatusNotFound, errors.New("такой ручной отметки нет; отметки из кода снимает сканер"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// redactSnapshot removes what a visitor has no use for: the broker account
// each connection authenticated as. Addresses stay, exactly as they do for
// services — see redact.go for why.
func redactSnapshot(snap *rabbitmq.Snapshot) {
	for i := range snap.Connections {
		snap.Connections[i].User = ""
	}
}

func (s *Server) mqChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.MQChangeFilter{
		Cluster: q.Get("cluster"),
		Vhost:   q.Get("vhost"),
		Object:  q.Get("object"),
		Limit:   intParam(r, "limit", 60, 500),
	}
	for _, kind := range strings.Split(q.Get("kinds"), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			filter.Kinds = append(filter.Kinds, kind)
		}
	}
	changes, err := s.store.ListMQChanges(r.Context(), filter)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

func (s *Server) mqRefresh(w http.ResponseWriter, r *http.Request) {
	s.collector.RefreshMQ()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}
