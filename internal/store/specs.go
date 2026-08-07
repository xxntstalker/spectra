package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Version is one stored spec revision.
type Version struct {
	ID int64 `json:"id"`
	// Revision is the number a person sees: 1, 2, 3 within one service. ID is
	// the primary key and stays global, because changes reference it.
	Revision      int       `json:"revision"`
	ServiceID     int64     `json:"serviceId"`
	Hash          string    `json:"hash"`
	CapturedAt    time.Time `json:"capturedAt"`
	Title         string    `json:"title,omitempty"`
	APIVersion    string    `json:"apiVersion,omitempty"`
	SpecVersion   string    `json:"specVersion,omitempty"`
	EndpointCount int       `json:"endpointCount"`
	Spec          []byte    `json:"-"`
	// RollbackOf is the revision this one returned to, when the contract is
	// byte-for-byte a state seen before.
	RollbackOf *int `json:"rollbackOf,omitempty"`
}

func (s *Store) LatestVersion(ctx context.Context, serviceID int64) (*Version, error) {
	return s.scanVersion(s.pool.QueryRow(ctx, `
		SELECT id, revision, service_id, hash, captured_at, title, api_version, spec_version, endpoint_count, rollback_of, spec
		FROM spec_versions WHERE service_id = $1 ORDER BY id DESC LIMIT 1`, serviceID))
}

// GetVersion looks a revision up by the number shown to the user, not by the
// primary key: links people share should carry the number they can see.
func (s *Store) GetVersion(ctx context.Context, serviceID int64, revision int) (*Version, error) {
	return s.scanVersion(s.pool.QueryRow(ctx, `
		SELECT id, revision, service_id, hash, captured_at, title, api_version, spec_version, endpoint_count, rollback_of, spec
		FROM spec_versions WHERE service_id = $1 AND revision = $2`, serviceID, revision))
}

func (s *Store) scanVersion(row pgx.Row) (*Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.Revision, &v.ServiceID, &v.Hash, &v.CapturedAt, &v.Title,
		&v.APIVersion, &v.SpecVersion, &v.EndpointCount, &v.RollbackOf, &v.Spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) ListVersions(ctx context.Context, serviceID int64, limit int) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, revision, service_id, hash, captured_at, title, api_version, spec_version, endpoint_count, rollback_of
		FROM spec_versions WHERE service_id = $1 ORDER BY id DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Revision, &v.ServiceID, &v.Hash, &v.CapturedAt, &v.Title,
			&v.APIVersion, &v.SpecVersion, &v.EndpointCount, &v.RollbackOf); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PreviousRevisionWithHash reports the earliest revision of this service whose
// contract hash matches, or nil. A match means the service returned to a state
// it had before — almost always a rolled-back deploy.
func (s *Store) PreviousRevisionWithHash(ctx context.Context, serviceID int64, hash string) (*int, error) {
	var revision *int
	err := s.pool.QueryRow(ctx, `
		SELECT min(revision) FROM spec_versions WHERE service_id = $1 AND hash = $2`,
		serviceID, hash).Scan(&revision)
	return revision, err
}

// InsertVersion assigns the next per-service revision number.
//
// Two polls of the same service overlapping is unlikely — sweeps are
// serialised — but a manual probe can land next to a scheduled one. The unique
// index catches that collision, and the retry picks the next free number
// instead of dropping the revision.
func (s *Store) InsertVersion(ctx context.Context, v *Version) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var id int64
		var revision int
		err := s.pool.QueryRow(ctx, `
			INSERT INTO spec_versions
				(service_id, revision, hash, captured_at, spec, title, api_version, spec_version,
				 endpoint_count, rollback_of)
			VALUES (
				$1,
				(SELECT COALESCE(max(revision), 0) + 1 FROM spec_versions WHERE service_id = $1),
				$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING id, revision`,
			v.ServiceID, v.Hash, v.CapturedAt, v.Spec, v.Title, v.APIVersion, v.SpecVersion,
			v.EndpointCount, v.RollbackOf).
			Scan(&id, &revision)
		if err == nil {
			v.Revision = revision
			return id, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "idx_versions_revision") {
			return 0, err
		}
	}
	return 0, lastErr
}

// PruneVersions keeps the newest revisions and drops the changes that pointed
// at the removed ones. Every revision holds a full spec, so history has to be
// bounded by something.
func (s *Store) PruneVersions(ctx context.Context, serviceID int64, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	const survivors = `SELECT id FROM spec_versions WHERE service_id = $1 ORDER BY id DESC LIMIT $2`
	if _, err := tx.Exec(ctx,
		`DELETE FROM changes WHERE service_id = $1 AND to_version_id NOT IN (`+survivors+`)`,
		serviceID, keep); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM spec_versions WHERE service_id = $1 AND id NOT IN (`+survivors+`)`,
		serviceID, keep)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// Change is a recorded transition between two revisions.
type Change struct {
	ID            int64  `json:"id"`
	ServiceID     int64  `json:"serviceId"`
	ServiceName   string `json:"serviceName,omitempty"`
	FromVersionID *int64 `json:"fromVersionId,omitempty"`
	ToVersionID   int64  `json:"toVersionId"`
	// Human-facing numbers, scoped to the service.
	FromRevision *int            `json:"fromRevision,omitempty"`
	ToRevision   int             `json:"toRevision"`
	DetectedAt   time.Time       `json:"detectedAt"`
	Breaking     bool            `json:"breaking"`
	Added        int             `json:"added"`
	Removed      int             `json:"removed"`
	Modified     int             `json:"modified"`
	Diff         json.RawMessage `json:"diff,omitempty"`
}

func (s *Store) InsertChange(ctx context.Context, c *Change) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO changes
			(service_id, from_version_id, to_version_id, detected_at, breaking, added, removed, modified, diff)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		c.ServiceID, c.FromVersionID, c.ToVersionID, c.DetectedAt,
		c.Breaking, c.Added, c.Removed, c.Modified, []byte(c.Diff))
	return err
}

func (s *Store) ListChanges(ctx context.Context, serviceID int64, limit int) ([]Change, error) {
	query := `
		SELECT c.id, c.service_id, s.display_name, c.from_version_id, c.to_version_id,
		       fv.revision, COALESCE(tv.revision, 0),
		       c.detected_at, c.breaking, c.added, c.removed, c.modified, c.diff
		FROM changes c
		JOIN services s ON s.id = c.service_id
		LEFT JOIN spec_versions tv ON tv.id = c.to_version_id
		LEFT JOIN spec_versions fv ON fv.id = c.from_version_id`
	args := []any{limit}
	if serviceID != 0 {
		query += ` WHERE c.service_id = $2`
		args = append(args, serviceID)
	}
	query += ` ORDER BY c.id DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Change{}
	for rows.Next() {
		var c Change
		var diff []byte
		if err := rows.Scan(&c.ID, &c.ServiceID, &c.ServiceName, &c.FromVersionID,
			&c.ToVersionID, &c.FromRevision, &c.ToRevision,
			&c.DetectedAt, &c.Breaking, &c.Added, &c.Removed,
			&c.Modified, &diff); err != nil {
			return nil, err
		}
		c.Diff = diff
		out = append(out, c)
	}
	return out, rows.Err()
}
