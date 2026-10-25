package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MQState is the last known state of one RabbitMQ cluster.
type MQState struct {
	Cluster string `json:"cluster"`
	// Snapshot is the JSON document the page renders, kept as text: nothing
	// queries inside it, it is only stored and served.
	Snapshot      string     `json:"-"`
	Definitions   string     `json:"-"`
	Hash          string     `json:"-"`
	CapturedAt    *time.Time `json:"capturedAt,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

// MQChange is one recorded change of a cluster's topology.
type MQChange struct {
	ID         int64     `json:"id"`
	Cluster    string    `json:"cluster"`
	Vhost      string    `json:"vhost"`
	ObjectKind string    `json:"objectKind"`
	Object     string    `json:"object"`
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`
	Lines      []string  `json:"lines"`
	DetectedAt time.Time `json:"detectedAt"`
}

// MQDeclared is a link between a service and a queue or exchange that someone
// stated rather than the broker showed: a nightly job, a basic.get reader.
type MQDeclared struct {
	ID         int64     `json:"id"`
	Cluster    string    `json:"cluster"`
	Vhost      string    `json:"vhost"`
	ObjectKind string    `json:"objectKind"` // queue | exchange
	Object     string    `json:"object"`
	Role       string    `json:"role"` // consume | publish
	Service    string    `json:"service"`
	Mode       string    `json:"mode"` // subscribe | schedule | get | manual
	Note       string    `json:"note"`
	Source     string    `json:"source"` // manual | code
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (s *Store) ListMQDeclared(ctx context.Context) ([]MQDeclared, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, cluster, vhost, object_kind, object_name, role, service, mode, note, source, updated_at
		FROM mq_declared ORDER BY cluster, vhost, object_name, service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MQDeclared{}
	for rows.Next() {
		var d MQDeclared
		if err := rows.Scan(&d.ID, &d.Cluster, &d.Vhost, &d.ObjectKind, &d.Object, &d.Role,
			&d.Service, &d.Mode, &d.Note, &d.Source, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveMQDeclared records a link, or updates how and why it is declared if the
// same source already declared it.
func (s *Store) SaveMQDeclared(ctx context.Context, d MQDeclared) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO mq_declared (cluster, vhost, object_kind, object_name, role, service, mode, note, source, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (cluster, vhost, object_kind, object_name, role, service, source) DO UPDATE SET
			mode = excluded.mode, note = excluded.note, updated_at = now()
		RETURNING id`,
		d.Cluster, d.Vhost, d.ObjectKind, d.Object, d.Role, d.Service, d.Mode, d.Note, d.Source).Scan(&id)
	return id, err
}

// DeleteMQDeclared removes one link a person entered. Links from code are not
// removed this way: the scanner owns them and would only put them back.
func (s *Store) DeleteMQDeclared(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mq_declared WHERE id = $1 AND source = 'manual'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MQState returns the stored state of a cluster, or nil before its first poll.
func (s *Store) MQState(ctx context.Context, cluster string) (*MQState, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT cluster, snapshot, definitions, hash, captured_at, last_checked_at, last_success_at, last_error
		FROM mq_state WHERE cluster = $1`, cluster)
	st, err := scanMQState(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

// ListMQStates returns every cluster that has been polled at least once.
func (s *Store) ListMQStates(ctx context.Context) ([]MQState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cluster, snapshot, definitions, hash, captured_at, last_checked_at, last_success_at, last_error
		FROM mq_state ORDER BY cluster`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MQState{}
	for rows.Next() {
		st, err := scanMQState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

func scanMQState(row pgx.Row) (*MQState, error) {
	var st MQState
	err := row.Scan(&st.Cluster, &st.Snapshot, &st.Definitions, &st.Hash,
		&st.CapturedAt, &st.LastCheckedAt, &st.LastSuccessAt, &st.LastError)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SaveMQSnapshot stores a successful poll. The snapshot is replaced every
// time — it carries message counts the page shows — while changes are written
// only when the caller found any, and in the same transaction, so a crash
// between the two cannot record a change against a state that was never saved
// or save a state whose change was lost.
func (s *Store) SaveMQSnapshot(ctx context.Context, cluster string, snapshot, definitions []byte,
	hash string, capturedAt time.Time, changes []MQChange) error {

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO mq_state (cluster, snapshot, definitions, hash, captured_at,
			                      last_checked_at, last_success_at, last_error)
			VALUES ($1, $2, $3, $4, $5, $5, $5, '')
			ON CONFLICT (cluster) DO UPDATE SET
				snapshot = excluded.snapshot, definitions = excluded.definitions,
				hash = excluded.hash, captured_at = excluded.captured_at,
				last_checked_at = excluded.last_checked_at,
				last_success_at = excluded.last_success_at, last_error = ''`,
			cluster, string(snapshot), string(definitions), hash, capturedAt); err != nil {
			return err
		}
		for _, c := range changes {
			lines := c.Lines
			if lines == nil {
				lines = []string{}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO mq_changes (cluster, vhost, object_kind, object_name, kind, severity, lines, detected_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				cluster, c.Vhost, c.ObjectKind, c.Object, c.Kind, c.Severity, lines, capturedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveMQFailure records a poll that did not get through. The previous
// snapshot stays: a broker that is briefly unreachable has not lost its
// queues, and showing an empty page would say that it had.
func (s *Store) SaveMQFailure(ctx context.Context, cluster, message string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mq_state (cluster, last_checked_at, last_error) VALUES ($1, $2, $3)
		ON CONFLICT (cluster) DO UPDATE SET
			last_checked_at = excluded.last_checked_at, last_error = excluded.last_error`,
		cluster, at, message)
	return err
}

// MQChangeFilter narrows the change list. Empty fields match everything.
type MQChangeFilter struct {
	Cluster string
	Vhost   string
	Object  string
	// Kinds limits the object kinds, e.g. queue and binding for a queue's own
	// history: a binding change is recorded against the queue it points to.
	Kinds []string
	Limit int
}

func (s *Store) ListMQChanges(ctx context.Context, f MQChangeFilter) ([]MQChange, error) {
	if f.Limit <= 0 {
		f.Limit = 60
	}
	kinds := f.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, cluster, vhost, object_kind, object_name, kind, severity, lines, detected_at
		FROM mq_changes
		WHERE ($1 = '' OR cluster = $1)
		  AND ($2 = '' OR vhost = $2)
		  AND ($3 = '' OR object_name = $3)
		  AND (cardinality($4::text[]) = 0 OR object_kind = ANY($4))
		ORDER BY detected_at DESC, id DESC
		LIMIT $5`, f.Cluster, f.Vhost, f.Object, kinds, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MQChange{}
	for rows.Next() {
		var c MQChange
		if err := rows.Scan(&c.ID, &c.Cluster, &c.Vhost, &c.ObjectKind, &c.Object,
			&c.Kind, &c.Severity, &c.Lines, &c.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
