package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metrics are written by hand rather than through a client library.
//
// The exposition format is a few lines of text, and the alternative costs a
// dependency tree larger than the rest of this program. What is exposed is
// chosen the same way: only what somebody would actually alert on.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var out strings.Builder
	out.Grow(2048)

	metric := func(name, help, kind string) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	value := func(name, labels string, v any) {
		if labels != "" {
			labels = "{" + labels + "}"
		}
		fmt.Fprintf(&out, "%s%s %v\n", name, labels, v)
	}

	metric("spectra_up", "Портал отвечает и база доступна.", "gauge")
	dbUp := 1
	if err := s.store.Ping(ctx); err != nil {
		dbUp = 0
	}
	value("spectra_up", "", dbUp)

	if dbUp == 0 {
		// Everything below reads the database; without it there is nothing
		// honest to report, and zeroes would look like real measurements.
		writeMetrics(w, out.String())
		return
	}

	counts, err := s.store.Counts(ctx)
	if err == nil {
		metric("spectra_services", "Сервисы в реестре по состоянию последнего опроса.", "gauge")
		value("spectra_services", `state="ok"`, counts.WithSpec)
		value("spectra_services", `state="no_spec"`, counts.NoSpec)
		value("spectra_services", `state="unreachable"`, counts.Unreachable)
		value("spectra_services", `state="hidden"`, counts.Hidden)
		value("spectra_services", `state="gone"`, counts.Gone)

		metric("spectra_services_total", "Всего записей в реестре.", "gauge")
		value("spectra_services_total", "", counts.Total)

		// Мимо автоматического расписания: спека не нашлась и нужен человек.
		metric("spectra_services_manual_only",
			"Сервисы вне автоматического опроса: спека не найдена, нужен ручной ввод адреса.", "gauge")
		value("spectra_services_manual_only", "", counts.ManualOnly)

		metric("spectra_services_without_owner",
			"Сервисы без указанной команды или контакта.", "gauge")
		value("spectra_services_without_owner", "", counts.NoOwner)
	}

	// The single most useful alert: discovery has been failing since Tuesday
	// and nobody noticed, because the portal keeps serving its cached list.
	runs, err := s.store.ListDiscoveryRuns(ctx, 20)
	if err == nil {
		metric("spectra_discovery_last_success_timestamp_seconds",
			"Время последнего успешного сканирования источника сервисов.", "gauge")
		metric("spectra_discovery_last_failure_timestamp_seconds",
			"Время последней неудачи сканирования.", "gauge")

		lastOK := map[string]int64{}
		lastFail := map[string]int64{}
		for _, run := range runs {
			target := lastOK
			if run.Error != "" {
				target = lastFail
			}
			if ts := run.StartedAt.Unix(); ts > target[run.Source] {
				target[run.Source] = ts
			}
		}
		for _, source := range sortedKeys(lastOK) {
			value("spectra_discovery_last_success_timestamp_seconds",
				`source=`+strconv.Quote(source), lastOK[source])
		}
		for _, source := range sortedKeys(lastFail) {
			value("spectra_discovery_last_failure_timestamp_seconds",
				`source=`+strconv.Quote(source), lastFail[source])
		}
	}

	// Freshness of the data itself: the oldest successful check across the
	// estate answers "how stale can what I am reading be".
	if oldest, err := s.store.OldestSuccessfulCheck(ctx); err == nil && oldest != nil {
		metric("spectra_oldest_successful_check_timestamp_seconds",
			"Самая старая успешная проверка среди опрашиваемых сервисов.", "gauge")
		value("spectra_oldest_successful_check_timestamp_seconds", "", oldest.Unix())
	}

	if revisions, changes, breaking, err := s.store.HistoryTotals(ctx); err == nil {
		metric("spectra_spec_revisions_total", "Сохранённых ревизий спецификаций.", "gauge")
		value("spectra_spec_revisions_total", "", revisions)
		metric("spectra_changes_total", "Зафиксированных изменений контрактов.", "gauge")
		value("spectra_changes_total", "", changes)
		metric("spectra_breaking_changes_total", "Из них помеченных ломающими.", "gauge")
		value("spectra_breaking_changes_total", "", breaking)
	}

	metric("spectra_poller_active", "Эта реплика держит блокировку опроса.", "gauge")
	value("spectra_poller_active", "", boolValue(s.collector.IsPolling()))

	writeMetrics(w, out.String())
}

func writeMetrics(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func boolValue(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
