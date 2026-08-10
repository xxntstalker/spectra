package store

import (
	"context"
	"time"
)

// Discovered is one candidate the cluster reported.
type Discovered struct {
	Key         string // stable identity across sweeps
	DisplayName string
	BaseURL     string
	Host        string
	Namespace   string
	IngressName string
	Backend     string
	// SpecURL and Group come from annotations on the Ingress: the team that
	// owns a service declares these itself, next to its own manifests.
	SpecURL string
	Group   string
	// Owner declared on the Ingress by the team that owns the service.
	Owner OwnerFields
}

// SyncResult reports what a sweep changed.
//
// The names matter as much as the counts: "найдено 9, новых 0" does not answer
// the question an operator actually has, which is what appeared and what went
// away.
type SyncResult struct {
	Found      int      `json:"found"`
	Added      int      `json:"added"`
	Gone       int      `json:"gone"`
	AddedNames []string `json:"addedNames"`
	GoneNames  []string `json:"goneNames"`
}

// SyncDiscovered reconciles the registry with what the cluster reports.
//
// Nothing is deleted. A service that disappears is flagged absent so its
// history, its group and its manually entered spec URL survive a redeploy that
// briefly removes an Ingress.
//
// Fields a human owns — display name, group, tags, notes, hidden flag, manual
// spec URL — are never overwritten by a sweep.
func (s *Store) SyncDiscovered(ctx context.Context, source string, items []Discovered) (SyncResult, error) {
	started := time.Now()
	res := SyncResult{Found: len(items), AddedNames: []string{}, GoneNames: []string{}}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	now := time.Now()
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)

		// A group named in an annotation is created on the fly.
		var groupID *int64
		if item.Group != "" {
			var id int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO groups (name) VALUES ($1)
				ON CONFLICT (name) DO UPDATE SET name = excluded.name
				RETURNING id`, item.Group).Scan(&id); err != nil {
				return res, err
			}
			groupID = &id
		}

		var inserted bool
		if err := tx.QueryRow(ctx, `
			INSERT INTO services
				(key, source, display_name, base_url, host, namespace, ingress_name, backend,
				 first_seen_at, last_seen_at, present, group_id, spec_url, spec_url_source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,TRUE,$10,NULLIF($11,''),
			        CASE WHEN $11 = '' THEN NULL ELSE $12 END)
			ON CONFLICT (key) DO UPDATE SET
				-- Address details follow the cluster; everything a person
				-- edited is left alone.
				base_url     = excluded.base_url,
				host         = excluded.host,
				namespace    = excluded.namespace,
				ingress_name = excluded.ingress_name,
				backend      = excluded.backend,
				last_seen_at = excluded.last_seen_at,
				present      = TRUE,
				-- The annotation only fills a gap: whatever a person set in
				-- the UI stays theirs.
				group_id = COALESCE(services.group_id, excluded.group_id),
				spec_url = CASE
					WHEN services.manual_spec_url IS NOT NULL AND services.manual_spec_url <> ''
						THEN services.spec_url
					WHEN excluded.spec_url IS NOT NULL THEN excluded.spec_url
					ELSE services.spec_url END,
				spec_url_source = CASE
					WHEN services.manual_spec_url IS NOT NULL AND services.manual_spec_url <> ''
						THEN services.spec_url_source
					WHEN excluded.spec_url IS NOT NULL THEN excluded.spec_url_source
					ELSE services.spec_url_source END,
				-- Новый адрес из аннотации — это повод попробовать снова.
				-- Без этого команда добавила бы аннотацию, а сервис остался бы
				-- висеть выключенным до ручного опроса.
				auto_probe = CASE
					WHEN excluded.spec_url IS NOT NULL
					 AND excluded.spec_url IS DISTINCT FROM services.spec_url
						THEN TRUE
					ELSE services.auto_probe END,
				next_probe_at = CASE
					WHEN excluded.spec_url IS NOT NULL
					 AND excluded.spec_url IS DISTINCT FROM services.spec_url
						THEN NULL
					ELSE services.next_probe_at END,
				updated_at   = now()
			RETURNING (xmax = 0)`,
			item.Key, source, item.DisplayName, item.BaseURL, item.Host,
			item.Namespace, item.IngressName, item.Backend, now,
			groupID, item.SpecURL, SourceAnnotation).
			Scan(&inserted); err != nil {
			return res, err
		}
		if inserted {
			res.Added++
			res.AddedNames = append(res.AddedNames, item.DisplayName)
		}
		// Annotations are a discovered source: they fill the spec_* columns and
		// leave anything a person entered alone.
		if item.Owner.Team != "" || item.Owner.Contact != "" ||
			item.Owner.Repo != "" || len(item.Owner.Owners) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE services SET
					spec_team = COALESCE(NULLIF($2,''), spec_team),
					spec_contact = COALESCE(NULLIF($3,''), spec_contact),
					spec_repo = COALESCE(NULLIF($4,''), spec_repo),
					spec_owners = CASE WHEN $5::text[] <> '{}' THEN $5::text[] ELSE spec_owners END
				WHERE key = $1`,
				item.Key, item.Owner.Team, item.Owner.Contact,
				item.Owner.Repo, item.Owner.Owners); err != nil {
				return res, err
			}
		}
	}

	rows, err := tx.Query(ctx, `
		UPDATE services SET present = FALSE, updated_at = now()
		WHERE source = $1 AND present AND NOT (key = ANY($2))
		RETURNING display_name`, source, keys)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return res, err
		}
		res.GoneNames = append(res.GoneNames, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	res.Gone = len(res.GoneNames)

	if _, err := tx.Exec(ctx, `
		INSERT INTO discovery_runs (source, found, added, gone, added_names, gone_names, duration_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		source, res.Found, res.Added, res.Gone,
		res.AddedNames, res.GoneNames, time.Since(started).Milliseconds()); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

func (s *Store) RecordSyncFailure(ctx context.Context, source string, cause error) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO discovery_runs (source, error) VALUES ($1,$2)`, source, cause.Error())
	return err
}

// DiscoveryRun is one recorded sweep, shown on the settings page so an operator
// can tell "no new services" from "discovery has been failing since Tuesday".
type DiscoveryRun struct {
	ID         int64     `json:"id"`
	StartedAt  time.Time `json:"startedAt"`
	Source     string    `json:"source"`
	Found      int       `json:"found"`
	Added      int       `json:"added"`
	Gone       int       `json:"gone"`
	AddedNames []string  `json:"addedNames"`
	GoneNames  []string  `json:"goneNames"`
	DurationMS int       `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
}

func (s *Store) ListDiscoveryRuns(ctx context.Context, limit int) ([]DiscoveryRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, started_at, source, found, added, gone, added_names, gone_names, duration_ms, error
		FROM discovery_runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DiscoveryRun{}
	for rows.Next() {
		var r DiscoveryRun
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.Source, &r.Found, &r.Added, &r.Gone,
			&r.AddedNames, &r.GoneNames, &r.DurationMS, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Counts drives the filter chips.
type Counts struct {
	Total       int `json:"total"`
	WithSpec    int `json:"withSpec"`
	NoSpec      int `json:"noSpec"`
	Unreachable int `json:"unreachable"`
	Hidden      int `json:"hidden"`
	NoOwner     int `json:"noOwner"`
	ManualOnly  int `json:"manualOnly"`
	Gone        int `json:"gone"`
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE probe_status = $1),
			count(*) FILTER (WHERE probe_status = $2 AND review_state <> $3),
			count(*) FILTER (WHERE probe_status = $4 AND review_state <> $3),
			count(*) FILTER (WHERE review_state = $3),
			count(*) FILTER (WHERE NOT auto_probe AND review_state <> $3),
			count(*) FILTER (WHERE NOT present),
			count(*) FILTER (WHERE review_state <> $3
				AND COALESCE(NULLIF(manual_team,''), NULLIF(spec_team,''),
				             NULLIF(manual_contact,''), NULLIF(spec_contact,'')) IS NULL)
		FROM services`,
		ProbeOK, ProbeNoSpec, ReviewIgnored, ProbeUnreachable).
		Scan(&c.Total, &c.WithSpec, &c.NoSpec, &c.Unreachable, &c.Hidden, &c.ManualOnly,
			&c.Gone, &c.NoOwner)
	return c, err
}

// OldestSuccessfulCheck answers "как давно в худшем случае проверялись данные,
// которые сейчас показываются". Hidden services and ones parked for manual
// entry are excluded: nobody expects those to be fresh.
func (s *Store) OldestSuccessfulCheck(ctx context.Context) (*time.Time, error) {
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT min(last_success_at) FROM services
		WHERE present AND auto_probe AND review_state <> $1 AND last_success_at IS NOT NULL`,
		ReviewIgnored).Scan(&oldest)
	return oldest, err
}

// HistoryTotals backs the growth metrics: revisions carry a full copy of every
// specification, so the number is what tells an operator when retention needs
// tightening.
func (s *Store) HistoryTotals(ctx context.Context) (revisions, changes, breaking int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM spec_versions),
			(SELECT count(*) FROM changes),
			(SELECT count(*) FROM changes WHERE breaking)`).
		Scan(&revisions, &changes, &breaking)
	return revisions, changes, breaking, err
}
