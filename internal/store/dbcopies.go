package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Sources and statuses of a database copy (ARCHITECTURE 10.6).
const (
	CopySourceLive     = "live"
	CopySourceSnapshot = "snapshot"

	// Kinds of a record: a copy from another app, or a restore of the
	// app's own dump (ARCHITECTURE 10.8).
	CopyKindCopy    = "copy"
	CopyKindRestore = "restore"

	CopyRunning   = "running"
	CopySucceeded = "succeeded"
	CopyFailed    = "failed"
)

// CopyInterruptedError is recorded on copies that were running when koroku
// stopped.
const CopyInterruptedError = "interrupted: koroku restarted while the copy was running"

// DBCopy is one copy of an app's database into a staging app.
type DBCopy struct {
	ID          int64
	TargetAppID int64
	// SourceAppID is nil once the source app has been deleted.
	SourceAppID *int64
	SourceSlug  string
	SourceKind  string
	Status      string
	Error       string
	Bytes       int64
	// BackupSnapshotID is the dump of the target taken before loading;
	// empty when none was taken.
	BackupSnapshotID string
	// Kind is CopyKindCopy or CopyKindRestore.
	Kind string
	// SourceSnapshotID is the dump a restore loaded; empty for a copy.
	SourceSnapshotID string
	StartedAt        time.Time
	FinishedAt       *time.Time
	// Actor is who started the copy; nil for older records.
	Actor *Actor
}

const dbCopyColumns = `id, target_app_id, source_app_id, source_slug, source_kind, status, error,
	bytes, backup_snapshot_id, started_at, finished_at, actor_user_id, actor_login, kind, source_snapshot_id`

func scanDBCopy(row rowScanner) (DBCopy, error) {
	var (
		c        DBCopy
		sourceID sql.NullInt64
		started  int64
		finished sql.NullInt64
		actorID  sql.NullInt64
		actor    string
	)
	err := row.Scan(&c.ID, &c.TargetAppID, &sourceID, &c.SourceSlug, &c.SourceKind, &c.Status, &c.Error,
		&c.Bytes, &c.BackupSnapshotID, &started, &finished, &actorID, &actor, &c.Kind, &c.SourceSnapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return DBCopy{}, ErrNotFound
	}
	if err != nil {
		return DBCopy{}, fmt.Errorf("scan database copy: %w", err)
	}
	if sourceID.Valid {
		c.SourceAppID = &sourceID.Int64
	}
	c.StartedAt = fromMillis(started)
	c.FinishedAt = fromNullMillis(finished)
	c.Actor = actorFromColumns(actorID, actor)
	return c, nil
}

// StartDBCopy records the beginning of a copy.
func (s *Store) StartDBCopy(ctx context.Context, target, source App, kind string, now time.Time) (DBCopy, error) {
	actorID, actorLogin := actorColumns(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO db_copies (target_app_id, source_app_id, source_slug, source_kind, status, started_at,
			actor_user_id, actor_login)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		target.ID, source.ID, source.Slug, kind, CopyRunning, toMillis(now), actorID, actorLogin)
	if err != nil {
		return DBCopy{}, fmt.Errorf("start database copy: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DBCopy{}, fmt.Errorf("start database copy: %w", err)
	}
	return s.GetDBCopy(ctx, id)
}

// StartDBRestore records the beginning of a restore of the app's own dump
// snapshot (ARCHITECTURE 10.8): a copy whose source and target are the app.
func (s *Store) StartDBRestore(ctx context.Context, app App, snapshotID string, now time.Time) (DBCopy, error) {
	actorID, actorLogin := actorColumns(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO db_copies (target_app_id, source_app_id, source_slug, source_kind, status, started_at,
			actor_user_id, actor_login, kind, source_snapshot_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ID, app.ID, app.Slug, CopySourceSnapshot, CopyRunning, toMillis(now), actorID, actorLogin,
		CopyKindRestore, snapshotID)
	if err != nil {
		return DBCopy{}, fmt.Errorf("start database restore: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DBCopy{}, fmt.Errorf("start database restore: %w", err)
	}
	return s.GetDBCopy(ctx, id)
}

// SetDBCopyBackup records the snapshot taken of the target before loading.
func (s *Store) SetDBCopyBackup(ctx context.Context, id int64, snapshotID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE db_copies SET backup_snapshot_id = ? WHERE id = ?`, snapshotID, id)
	if err != nil {
		return fmt.Errorf("record the backup of a database copy: %w", err)
	}
	return requireOneRow(res, "record the backup of a database copy")
}

// FinishDBCopy records the outcome of a running copy.
func (s *Store) FinishDBCopy(ctx context.Context, id int64, succeeded bool, errMsg string, bytes int64, now time.Time) error {
	status := CopyFailed
	if succeeded {
		status = CopySucceeded
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE db_copies SET status = ?, error = ?, bytes = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, errMsg, bytes, toMillis(now), id, CopyRunning)
	if err != nil {
		return fmt.Errorf("finish database copy: %w", err)
	}
	return requireOneRow(res, "finish database copy")
}

// GetDBCopy returns one copy.
func (s *Store) GetDBCopy(ctx context.Context, id int64) (DBCopy, error) {
	return scanDBCopy(s.db.QueryRowContext(ctx, `SELECT `+dbCopyColumns+` FROM db_copies WHERE id = ?`, id))
}

// ListDBCopies returns the newest copies into the target app.
func (s *Store) ListDBCopies(ctx context.Context, targetAppID int64, limit int) ([]DBCopy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+dbCopyColumns+` FROM db_copies WHERE target_app_id = ? ORDER BY id DESC LIMIT ?`,
		targetAppID, limit)
	if err != nil {
		return nil, fmt.Errorf("list database copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DBCopy
	for rows.Next() {
		c, err := scanDBCopy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list database copies: %w", err)
	}
	return out, nil
}

// FailInterruptedDBCopies marks copies left running by a previous process
// as failed and returns them. It is called once at startup.
func (s *Store) FailInterruptedDBCopies(ctx context.Context, now time.Time) ([]DBCopy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dbCopyColumns+` FROM db_copies WHERE status = ? ORDER BY id`, CopyRunning)
	if err != nil {
		return nil, fmt.Errorf("fail interrupted database copies: %w", err)
	}
	var copies []DBCopy
	for rows.Next() {
		c, err := scanDBCopy(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		copies = append(copies, c)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("fail interrupted database copies: %w", err)
	}
	for _, c := range copies {
		if err := s.FinishDBCopy(ctx, c.ID, false, CopyInterruptedError, c.Bytes, now); err != nil {
			return nil, err
		}
	}
	return copies, nil
}
