package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Statuses of the point-in-time recovery of an app (ARCHITECTURE 10.9).
const (
	PITRPreparing = "preparing"
	PITRReady     = "ready"
	PITRFailed    = "failed"
)

// Reasons a period cannot be recovered to.
const (
	PITRGapMountMissing   = "mount_missing"
	PITRGapDropped        = "dropped"
	PITRGapVersionChanged = "version_changed"
)

// PITRState is where the point-in-time recovery of an app stands.
type PITRState struct {
	AppID            int64
	Service          string
	Status           string
	Error            string
	DataDirectory    string
	ServerVersionNum int
	ArchiverFailed   int64
	Warning          string
	UpdatedAt        time.Time
}

// GetPITRState returns the state of an app, or ErrNotFound when it was
// never prepared.
func (s *Store) GetPITRState(ctx context.Context, appID int64) (PITRState, error) {
	var (
		st      PITRState
		updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT app_id, service, status, error, data_directory, server_version_num, archiver_failed, warning, updated_at
		 FROM pitr_states WHERE app_id = ?`, appID).
		Scan(&st.AppID, &st.Service, &st.Status, &st.Error, &st.DataDirectory, &st.ServerVersionNum,
			&st.ArchiverFailed, &st.Warning, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return PITRState{}, ErrNotFound
	}
	if err != nil {
		return PITRState{}, fmt.Errorf("read point-in-time recovery state: %w", err)
	}
	st.UpdatedAt = fromMillis(updated)
	return st, nil
}

// PutPITRState writes the state of an app.
func (s *Store) PutPITRState(ctx context.Context, st PITRState) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pitr_states (app_id, service, status, error, data_directory, server_version_num, archiver_failed, warning, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id) DO UPDATE SET service = excluded.service, status = excluded.status,
		   error = excluded.error, data_directory = excluded.data_directory,
		   server_version_num = excluded.server_version_num, archiver_failed = excluded.archiver_failed,
		   warning = excluded.warning, updated_at = excluded.updated_at`,
		st.AppID, st.Service, st.Status, st.Error, st.DataDirectory, st.ServerVersionNum,
		st.ArchiverFailed, st.Warning, toMillis(st.UpdatedAt))
	if err != nil {
		return fmt.Errorf("write point-in-time recovery state: %w", err)
	}
	return nil
}

// DeletePITRState forgets the state of an app whose recovery was turned
// off. Its base backups, batches and gaps stay until the app is deleted:
// the snapshots they describe stay in the repository too.
func (s *Store) DeletePITRState(ctx context.Context, appID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pitr_states WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("delete point-in-time recovery state: %w", err)
	}
	return nil
}

// PITRBase is one base backup.
type PITRBase struct {
	ID               int64
	AppID            int64
	Service          string
	SnapshotID       string
	StartedAt        time.Time
	FinishedAt       time.Time
	StartSegment     string
	Timeline         int
	ServerVersionNum int
}

// AddPITRBase records a base backup.
func (s *Store) AddPITRBase(ctx context.Context, b PITRBase) (PITRBase, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pitr_bases (app_id, service, snapshot_id, started_at, finished_at, start_segment, timeline, server_version_num)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.AppID, b.Service, b.SnapshotID, toMillis(b.StartedAt), toMillis(b.FinishedAt), b.StartSegment, b.Timeline, b.ServerVersionNum)
	if err != nil {
		return PITRBase{}, fmt.Errorf("record base backup: %w", err)
	}
	b.ID, err = res.LastInsertId()
	return b, err
}

// ListPITRBases returns the base backups of an app finished at or after
// since, oldest first.
func (s *Store) ListPITRBases(ctx context.Context, appID int64, since time.Time) ([]PITRBase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, service, snapshot_id, started_at, finished_at, start_segment, timeline, server_version_num
		 FROM pitr_bases WHERE app_id = ? AND finished_at >= ? ORDER BY finished_at, id`, appID, toMillis(since))
	if err != nil {
		return nil, fmt.Errorf("list base backups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PITRBase
	for rows.Next() {
		var (
			b                 PITRBase
			started, finished int64
		)
		if err := rows.Scan(&b.ID, &b.AppID, &b.Service, &b.SnapshotID, &started, &finished, &b.StartSegment, &b.Timeline, &b.ServerVersionNum); err != nil {
			return nil, fmt.Errorf("list base backups: %w", err)
		}
		b.StartedAt, b.FinishedAt = fromMillis(started), fromMillis(finished)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list base backups: %w", err)
	}
	return out, nil
}

// PITRWALBatch is the WAL shipped in one snapshot.
type PITRWALBatch struct {
	ID             int64
	AppID          int64
	Service        string
	SnapshotID     string
	FirstSegment   string
	LastSegment    string
	FileCount      int
	LastModifiedAt time.Time
	CreatedAt      time.Time
}

// AddPITRWALBatch records a shipped batch.
func (s *Store) AddPITRWALBatch(ctx context.Context, b PITRWALBatch) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pitr_wal_batches (app_id, service, snapshot_id, first_segment, last_segment, file_count, last_modified_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.AppID, b.Service, b.SnapshotID, b.FirstSegment, b.LastSegment, b.FileCount, toMillis(b.LastModifiedAt), toMillis(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("record WAL batch: %w", err)
	}
	return nil
}

// ListPITRWALBatches returns the batches of an app created at or after
// since, oldest first.
func (s *Store) ListPITRWALBatches(ctx context.Context, appID int64, since time.Time) ([]PITRWALBatch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, service, snapshot_id, first_segment, last_segment, file_count, last_modified_at, created_at
		 FROM pitr_wal_batches WHERE app_id = ? AND created_at >= ? ORDER BY id`, appID, toMillis(since))
	if err != nil {
		return nil, fmt.Errorf("list WAL batches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PITRWALBatch
	for rows.Next() {
		var (
			b                 PITRWALBatch
			modified, created int64
		)
		if err := rows.Scan(&b.ID, &b.AppID, &b.Service, &b.SnapshotID, &b.FirstSegment, &b.LastSegment, &b.FileCount, &modified, &created); err != nil {
			return nil, fmt.Errorf("list WAL batches: %w", err)
		}
		b.LastModifiedAt, b.CreatedAt = fromMillis(modified), fromMillis(created)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list WAL batches: %w", err)
	}
	return out, nil
}

// PITRGap is a period the database cannot be recovered to.
type PITRGap struct {
	ID        int64
	AppID     int64
	Service   string
	StartedAt time.Time
	// EndedAt is nil while the gap lasts.
	EndedAt *time.Time
	Reason  string
}

// OpenPITRGap starts a gap of the given reason, unless one of that reason
// is open already.
func (s *Store) OpenPITRGap(ctx context.Context, appID int64, service, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pitr_gaps (app_id, service, started_at, reason)
		 SELECT ?, ?, ?, ? WHERE NOT EXISTS (
		   SELECT 1 FROM pitr_gaps WHERE app_id = ? AND reason = ? AND ended_at IS NULL)`,
		appID, service, toMillis(at), reason, appID, reason)
	if err != nil {
		return fmt.Errorf("open recovery gap: %w", err)
	}
	return nil
}

// ClosePITRGaps ends the open gaps of an app of the given reason, or of
// every reason when reason is empty. It reports whether one was open.
func (s *Store) ClosePITRGaps(ctx context.Context, appID int64, reason string, at time.Time) (bool, error) {
	query := `UPDATE pitr_gaps SET ended_at = ? WHERE app_id = ? AND ended_at IS NULL`
	args := []any{toMillis(at), appID}
	if reason != "" {
		query += ` AND reason = ?`
		args = append(args, reason)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("close recovery gaps: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListPITRGaps returns the gaps of an app that ended at or after since or
// still last, oldest first.
func (s *Store) ListPITRGaps(ctx context.Context, appID int64, since time.Time) ([]PITRGap, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, service, started_at, ended_at, reason FROM pitr_gaps
		 WHERE app_id = ? AND (ended_at IS NULL OR ended_at >= ?) ORDER BY started_at, id`, appID, toMillis(since))
	if err != nil {
		return nil, fmt.Errorf("list recovery gaps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PITRGap
	for rows.Next() {
		var (
			g       PITRGap
			started int64
			ended   sql.NullInt64
		)
		if err := rows.Scan(&g.ID, &g.AppID, &g.Service, &started, &ended, &g.Reason); err != nil {
			return nil, fmt.Errorf("list recovery gaps: %w", err)
		}
		g.StartedAt, g.EndedAt = fromMillis(started), fromNullMillis(ended)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recovery gaps: %w", err)
	}
	return out, nil
}

// ListPITRApps returns the apps with point-in-time recovery turned on.
func (s *Store) ListPITRApps(ctx context.Context) ([]App, error) {
	apps, err := s.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	var out []App
	for _, a := range apps {
		if a.PITRService != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

// PITRRestore is one restore of a database to a point in time.
type PITRRestore struct {
	ID             int64
	AppID          int64
	Service        string
	TargetTime     time.Time
	BaseSnapshotID string
	TrashPath      string
	Status         string // running | succeeded | failed
	Error          string
	Actor          *Actor
	StartedAt      time.Time
	FinishedAt     *time.Time
}

// PITRRestoreInterruptedError is recorded on restores that were running
// when koroku stopped.
const PITRRestoreInterruptedError = "interrupted: koroku restarted while the restore was running"

const pitrRestoreColumns = `id, app_id, service, target_time, base_snapshot_id, trash_path, status, error,
	actor_user_id, actor_login, started_at, finished_at`

func scanPITRRestore(row rowScanner) (PITRRestore, error) {
	var (
		r                PITRRestore
		target, started  int64
		finished, userID sql.NullInt64
		login            string
	)
	err := row.Scan(&r.ID, &r.AppID, &r.Service, &target, &r.BaseSnapshotID, &r.TrashPath, &r.Status, &r.Error,
		&userID, &login, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return PITRRestore{}, ErrNotFound
	}
	if err != nil {
		return PITRRestore{}, fmt.Errorf("scan point-in-time restore: %w", err)
	}
	r.TargetTime, r.StartedAt, r.FinishedAt = fromMillis(target), fromMillis(started), fromNullMillis(finished)
	r.Actor = actorFromColumns(userID, login)
	return r, nil
}

// StartPITRRestore records the beginning of a restore.
func (s *Store) StartPITRRestore(ctx context.Context, app App, target, now time.Time) (PITRRestore, error) {
	userID, login := actorColumns(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pitr_restores (app_id, service, target_time, actor_user_id, actor_login, started_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, app.ID, app.PITRService, toMillis(target), userID, login, toMillis(now))
	if err != nil {
		return PITRRestore{}, fmt.Errorf("start point-in-time restore: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return PITRRestore{}, err
	}
	return s.GetPITRRestore(ctx, id)
}

// SetPITRRestoreDetails records the base backup a restore uses and where
// it moved the data before it.
func (s *Store) SetPITRRestoreDetails(ctx context.Context, id int64, baseSnapshotID, trashPath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pitr_restores SET base_snapshot_id = ?, trash_path = ? WHERE id = ?`,
		baseSnapshotID, trashPath, id)
	if err != nil {
		return fmt.Errorf("record point-in-time restore: %w", err)
	}
	return nil
}

// FinishPITRRestore records the outcome of a running restore.
func (s *Store) FinishPITRRestore(ctx context.Context, id int64, succeeded bool, errMsg string, now time.Time) error {
	status := CopyFailed
	if succeeded {
		status = CopySucceeded
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE pitr_restores SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = 'running'`,
		status, errMsg, toMillis(now), id)
	if err != nil {
		return fmt.Errorf("finish point-in-time restore: %w", err)
	}
	return requireOneRow(res, "finish point-in-time restore")
}

// GetPITRRestore returns one restore.
func (s *Store) GetPITRRestore(ctx context.Context, id int64) (PITRRestore, error) {
	return scanPITRRestore(s.db.QueryRowContext(ctx, `SELECT `+pitrRestoreColumns+` FROM pitr_restores WHERE id = ?`, id))
}

// ListPITRRestores returns the newest restores of an app.
func (s *Store) ListPITRRestores(ctx context.Context, appID int64, limit int) ([]PITRRestore, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pitrRestoreColumns+` FROM pitr_restores WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list point-in-time restores: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PITRRestore
	for rows.Next() {
		r, err := scanPITRRestore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FailInterruptedPITRRestores marks restores left running by a previous
// process as failed and returns them. It is called once at startup.
func (s *Store) FailInterruptedPITRRestores(ctx context.Context, now time.Time) ([]PITRRestore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pitrRestoreColumns+` FROM pitr_restores WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("fail interrupted point-in-time restores: %w", err)
	}
	var list []PITRRestore
	for rows.Next() {
		r, err := scanPITRRestore(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, r := range list {
		if err := s.FinishPITRRestore(ctx, r.ID, false, PITRRestoreInterruptedError, now); err != nil {
			return nil, err
		}
	}
	return list, nil
}
