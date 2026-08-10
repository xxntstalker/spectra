// Package store persists the registry, the settings, every spec revision and
// the changes between them, in Postgres.
//
// Postgres replaced SQLite when the registry stopped being a file in git: the
// service list is now discovered from the cluster and edited in the UI, so it
// is mutable state, and the process itself has to stay stateless enough to run
// as several replicas.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Advisory lock keys. Postgres advisory locks are the cheapest coordination
// available to a stateless service that already has a database.
const (
	// lockMigrations keeps several replicas starting at once from applying the
	// same migration twice.
	lockMigrations int64 = 8412001
	// LockPoller elects the single replica that polls services. Without it,
	// scaling the deployment would multiply the request load on every service
	// the portal watches.
	LockPoller int64 = 8412002
)

// Review states: what a human decided about a service. There is deliberately
// no "new" state — it was never clear when it should clear, and the address
// list shows everything anyway.
const (
	ReviewReviewed = "reviewed" // обычный сервис
	ReviewIgnored  = "ignored"  // «у этого хоста нет API» — не опрашивать вовсе
)

// Probe statuses: what the last poll found.
const (
	ProbeUnknown     = "unknown"
	ProbeOK          = "ok"
	ProbeNoSpec      = "no_spec"
	ProbeUnreachable = "unreachable"
	ProbeInvalidSpec = "invalid_spec"
)

// Spec URL sources.
const (
	SourceProbe       = "probe"
	SourceSwaggerHTML = "swagger-html"
	SourceManual      = "manual"
	// SourceAnnotation is an address declared on the Ingress by the team that
	// owns the service.
	SourceAnnotation = "annotation"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the connection pool for the advisory-lock helpers.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockMigrations); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockMigrations)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", name)
		}
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).
			Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			return err
		}
	}
	return nil
}

// TryLock takes a session-scoped advisory lock without blocking. The returned
// release function must be called to give the lock up.
func (s *Store) TryLock(ctx context.Context, key int64) (bool, func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, nil, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		conn.Release()
		return false, nil, err
	}
	if !acquired {
		conn.Release()
		return false, nil, nil
	}
	release := func() {
		// Best effort: releasing the connection would drop the lock anyway,
		// since it is session-scoped.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}
	return true, release, nil
}

// ---------------------------------------------------------------- settings --

// Settings are the operational knobs an operator changes at runtime. They live
// in the database rather than the environment so the settings page can write
// them and every replica picks the change up.
type Settings struct {
	PollInterval   time.Duration `json:"pollInterval"`
	RequestTimeout time.Duration `json:"requestTimeout"`
	ProbeBudget    time.Duration `json:"probeBudget"`
	// ProbeDelay paces requests against one service during discovery.
	ProbeDelay     time.Duration `json:"probeDelay"`
	MaxBackoff     time.Duration `json:"maxBackoff"`
	Concurrency    int           `json:"concurrency"`
	RetainVersions int           `json:"retainVersions"`
	IgnoreServers  bool          `json:"ignoreServers"`
	IgnoreFields   []string      `json:"ignoreFields"`
}

// DefaultSettings are what a fresh install starts with.
func DefaultSettings() Settings {
	return Settings{
		PollInterval:   time.Hour,
		RequestTimeout: 10 * time.Second,
		ProbeBudget:    2 * time.Minute,
		ProbeDelay:     200 * time.Millisecond,
		MaxBackoff:     6 * time.Hour,
		Concurrency:    8,
		RetainVersions: 100,
		IgnoreServers:  true,
		IgnoreFields:   nil,
	}
}

func (s *Store) LoadSettings(ctx context.Context) (Settings, error) {
	out := DefaultSettings()
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	stored := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		stored[k] = v
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Unparseable values fall back to the default rather than failing the
	// whole load: one bad row must not take the portal down.
	dur := func(key string, target *time.Duration) {
		if raw, ok := stored[key]; ok {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				*target = d
			}
		}
	}
	num := func(key string, target *int) {
		if raw, ok := stored[key]; ok {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				*target = n
			}
		}
	}
	dur("poll_interval", &out.PollInterval)
	dur("request_timeout", &out.RequestTimeout)
	dur("probe_budget", &out.ProbeBudget)
	dur("probe_delay", &out.ProbeDelay)
	dur("max_backoff", &out.MaxBackoff)
	num("concurrency", &out.Concurrency)
	num("retain_versions", &out.RetainVersions)
	if raw, ok := stored["ignore_servers"]; ok {
		out.IgnoreServers = raw == "true"
	}
	if raw, ok := stored["ignore_fields"]; ok {
		out.IgnoreFields = nil
		for _, f := range strings.Split(raw, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out.IgnoreFields = append(out.IgnoreFields, f)
			}
		}
	}
	return out, nil
}

func (s *Store) SaveSettings(ctx context.Context, v Settings) error {
	pairs := [][2]string{
		{"poll_interval", v.PollInterval.String()},
		{"request_timeout", v.RequestTimeout.String()},
		{"probe_budget", v.ProbeBudget.String()},
		{"probe_delay", v.ProbeDelay.String()},
		{"max_backoff", v.MaxBackoff.String()},
		{"concurrency", strconv.Itoa(v.Concurrency)},
		{"retain_versions", strconv.Itoa(v.RetainVersions)},
		{"ignore_servers", strconv.FormatBool(v.IgnoreServers)},
		{"ignore_fields", strings.Join(v.IgnoreFields, ",")},
	}
	batch := &pgx.Batch{}
	for _, p := range pairs {
		batch.Queue(`
			INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now()`,
			p[0], p[1])
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// ---------------------------------------------------------------- services --

// Service is a registry entry joined with the state of its newest revision.
type Service struct {
	ID          int64    `json:"id"`
	Key         string   `json:"key"`
	Source      string   `json:"source"`
	DisplayName string   `json:"displayName"`
	BaseURL     string   `json:"baseUrl"`
	ReviewState string   `json:"reviewState"`
	ProbeStatus string   `json:"probeStatus"`
	Present     bool     `json:"present"`
	Host        string   `json:"host,omitempty"`
	Namespace   string   `json:"namespace,omitempty"`
	IngressName string   `json:"ingressName,omitempty"`
	Backend     string   `json:"backend,omitempty"`
	GroupID     *int64   `json:"groupId,omitempty"`
	GroupName   string   `json:"groupName,omitempty"`
	Tags        []string `json:"tags"`
	Notes       string   `json:"notes,omitempty"`

	SpecURL       string `json:"specUrl,omitempty"`
	SpecURLSource string `json:"specUrlSource,omitempty"`
	ManualSpecURL string `json:"manualSpecUrl,omitempty"`
	LastError     string `json:"lastError,omitempty"`

	// Conditional-request validators. Not exposed to the UI: they are plumbing
	// between polls, and an ETag on a page tells a reader nothing.
	ETag         string `json:"-"`
	LastModified string `json:"-"`

	LastCheckedAt  *time.Time `json:"lastCheckedAt,omitempty"`
	LastSuccessAt  *time.Time `json:"lastSuccessAt,omitempty"`
	FirstSeenAt    time.Time  `json:"firstSeenAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	NextProbeAt    *time.Time `json:"nextProbeAt,omitempty"`
	FailCount      int        `json:"failCount"`
	// AutoProbe goes false once a poll finished and found no specification.
	// Re-probing such a host on a schedule is pointless: nothing about it will
	// change on its own. A successful manual probe turns it back on.
	AutoProbe bool `json:"autoProbe"`

	VersionID     int64      `json:"versionId,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
	Title         string     `json:"title,omitempty"`
	APIVersion    string     `json:"apiVersion,omitempty"`
	SpecVersion   string     `json:"specVersion,omitempty"`
	EndpointCount int        `json:"endpointCount"`
	VersionCount  int        `json:"versionCount"`
}

const serviceColumns = `
	s.id, s.key, s.source, s.display_name, s.base_url,
	s.review_state, s.probe_status, s.present,
	COALESCE(s.host,''), COALESCE(s.namespace,''), COALESCE(s.ingress_name,''), COALESCE(s.backend,''),
	s.group_id, COALESCE(g.name,''), s.tags, s.notes,
	COALESCE(s.spec_url,''), COALESCE(s.spec_url_source,''), COALESCE(s.manual_spec_url,''), s.last_error,
	COALESCE(s.etag,''), COALESCE(s.last_modified,''),
	s.last_checked_at, s.last_success_at, s.first_seen_at, s.last_seen_at,
	s.acknowledged_at, s.next_probe_at, s.fail_count, s.auto_probe,
	COALESCE(v.id,0), v.captured_at, COALESCE(v.title,''), COALESCE(v.api_version,''),
	COALESCE(v.spec_version,''), COALESCE(v.endpoint_count,0),
	(SELECT count(*) FROM spec_versions WHERE service_id = s.id)`

const serviceFrom = `
	FROM services s
	LEFT JOIN groups g ON g.id = s.group_id
	LEFT JOIN LATERAL (
		SELECT id, captured_at, title, api_version, spec_version, endpoint_count
		FROM spec_versions WHERE service_id = s.id ORDER BY id DESC LIMIT 1
	) v ON TRUE`

func scanService(row pgx.Row) (Service, error) {
	var s Service
	var groupID *int64
	err := row.Scan(&s.ID, &s.Key, &s.Source, &s.DisplayName, &s.BaseURL,
		&s.ReviewState, &s.ProbeStatus, &s.Present,
		&s.Host, &s.Namespace, &s.IngressName, &s.Backend,
		&groupID, &s.GroupName, &s.Tags, &s.Notes,
		&s.SpecURL, &s.SpecURLSource, &s.ManualSpecURL, &s.LastError,
		&s.ETag, &s.LastModified,
		&s.LastCheckedAt, &s.LastSuccessAt, &s.FirstSeenAt, &s.LastSeenAt,
		&s.AcknowledgedAt, &s.NextProbeAt, &s.FailCount, &s.AutoProbe,
		&s.VersionID, &s.UpdatedAt, &s.Title, &s.APIVersion,
		&s.SpecVersion, &s.EndpointCount, &s.VersionCount)
	s.GroupID = groupID
	if s.Tags == nil {
		s.Tags = []string{}
	}
	return s, err
}

func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceColumns+serviceFrom+`
		ORDER BY g.position NULLS LAST, g.name NULLS LAST, s.display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) GetService(ctx context.Context, id int64) (*Service, error) {
	svc, err := scanService(s.pool.QueryRow(ctx,
		`SELECT `+serviceColumns+serviceFrom+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &svc, nil
}

// PollCandidates returns the services a poll should touch.
//
// Ignored services are excluded outright — a host whose owner said it has no
// API must never be probed again. Services still inside their backoff window
// are excluded too, which is what keeps a hostless sweep from turning into a
// recurring flood of requests against things that will never answer.
func (s *Store) PollCandidates(ctx context.Context, now time.Time) ([]Service, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceColumns+serviceFrom+`
		WHERE s.present
		  AND s.review_state <> $1
		  AND s.auto_probe
		  AND (s.next_probe_at IS NULL OR s.next_probe_at <= $2)
		ORDER BY s.id`, ReviewIgnored, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// ServiceUpdate carries the fields the UI is allowed to change.
type ServiceUpdate struct {
	DisplayName   *string
	GroupID       **int64
	Tags          *[]string
	Notes         *string
	ReviewState   *string
	ManualSpecURL *string
}

func (s *Store) UpdateService(ctx context.Context, id int64, u ServiceUpdate) error {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	add := func(expr string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", expr, len(args)))
	}
	if u.DisplayName != nil {
		add("display_name", *u.DisplayName)
	}
	if u.GroupID != nil {
		add("group_id", *u.GroupID)
	}
	if u.Tags != nil {
		add("tags", *u.Tags)
	}
	if u.Notes != nil {
		add("notes", *u.Notes)
	}
	if u.ReviewState != nil {
		add("review_state", *u.ReviewState)
		// Any explicit decision counts as having looked at the service, so the
		// "new" badge clears without a separate click.
		sets = append(sets, "acknowledged_at = COALESCE(acknowledged_at, now())")
	}
	if u.ManualSpecURL != nil {
		add("manual_spec_url", *u.ManualSpecURL)
		// A human supplying an address is a fresh start: forget the failures
		// and the backoff so the next poll actually tries it.
		sets = append(sets, "fail_count = 0", "next_probe_at = NULL")
		if *u.ManualSpecURL != "" {
			add("spec_url", *u.ManualSpecURL)
			add("spec_url_source", SourceManual)
			sets = append(sets, "etag = NULL", "last_modified = NULL")
		}
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE services SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	return err
}

// ClearBackoff lets a manual probe run immediately.
// ClearBackoff lets a manual probe run immediately, and re-enables automatic
// polling for the attempt: a person asking for a retry must get one, whatever
// the previous outcome was.
func (s *Store) ClearBackoff(ctx context.Context, id int64) error {
	query := `UPDATE services SET next_probe_at = NULL, fail_count = 0, auto_probe = TRUE, updated_at = now()`
	if id == 0 {
		_, err := s.pool.Exec(ctx, query)
		return err
	}
	_, err := s.pool.Exec(ctx, query+` WHERE id = $1`, id)
	return err
}

// ------------------------------------------------------------------ probes --

type ProbeResult struct {
	ServiceID     int64
	Status        string
	Error         string
	SpecURL       string
	SpecURLSource string
	ETag          string
	LastModified  string
	CheckedAt     time.Time
	Succeeded     bool
	NextProbeAt   *time.Time
	// AutoProbe false parks the service until someone asks for it by hand.
	AutoProbe bool
}

func (s *Store) RecordProbe(ctx context.Context, p ProbeResult) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE services SET
			probe_status    = $2,
			last_error      = $3,
			last_checked_at = $4,
			last_success_at = CASE WHEN $5 THEN $4 ELSE last_success_at END,
			spec_url        = COALESCE(NULLIF($6, ''), spec_url),
			spec_url_source = COALESCE(NULLIF($7, ''), spec_url_source),
			etag            = NULLIF($8, ''),
			last_modified   = NULLIF($9, ''),
			fail_count      = CASE WHEN $5 THEN 0 ELSE fail_count + 1 END,
			next_probe_at   = $10,
			auto_probe      = $11,
			updated_at      = now()
		WHERE id = $1`,
		p.ServiceID, p.Status, p.Error, p.CheckedAt, p.Succeeded,
		p.SpecURL, p.SpecURLSource, p.ETag, p.LastModified, p.NextProbeAt, p.AutoProbe)
	return err
}

func (s *Store) ClearSpecURL(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE services
		SET spec_url = manual_spec_url, spec_url_source = NULL, etag = NULL, last_modified = NULL
		WHERE id = $1`, id)
	return err
}

// ------------------------------------------------------------------ groups --

type Group struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	Count    int    `json:"count"`
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.name, g.position, count(s.id)
		FROM groups g LEFT JOIN services s ON s.group_id = g.id
		GROUP BY g.id ORDER BY g.position, g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Position, &g.Count); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) CreateGroup(ctx context.Context, name string, position int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO groups (name, position) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET position = excluded.position
		RETURNING id`, name, position).Scan(&id)
	return id, err
}

func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
	return err
}

// AllTags powers the tag filter in the UI.
func (s *Store) AllTags(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT unnest(tags) AS tag FROM services ORDER BY tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}
