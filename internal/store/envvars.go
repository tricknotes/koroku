package store

import (
	"context"
	"fmt"
	"sort"
)

// EnvVar is one encrypted environment variable of an app.
type EnvVar struct {
	Key            string
	ValueEncrypted []byte
}

// ListEnvVars returns the env vars of an app ordered by key.
func (s *Store) ListEnvVars(ctx context.Context, appID int64) ([]EnvVar, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value_encrypted FROM env_vars WHERE app_id = ? ORDER BY key`, appID)
	if err != nil {
		return nil, fmt.Errorf("list env vars: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var vars []EnvVar
	for rows.Next() {
		var v EnvVar
		if err := rows.Scan(&v.Key, &v.ValueEncrypted); err != nil {
			return nil, fmt.Errorf("list env vars: %w", err)
		}
		vars = append(vars, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list env vars: %w", err)
	}
	return vars, nil
}

// ReplaceEnvVars atomically replaces every env var of an app.
func (s *Store) ReplaceEnvVars(ctx context.Context, appID int64, vars []EnvVar) error {
	sorted := append([]EnvVar(nil), vars...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace env vars: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM env_vars WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("replace env vars: %w", err)
	}
	for _, v := range sorted {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO env_vars (app_id, key, value_encrypted) VALUES (?, ?, ?)`,
			appID, v.Key, v.ValueEncrypted); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("env var %q: %w", v.Key, ErrConflict)
			}
			return fmt.Errorf("replace env vars: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace env vars: %w", err)
	}
	return nil
}

// PutEnvVar adds an env var or replaces the value of an existing one.
func (s *Store) PutEnvVar(ctx context.Context, appID int64, v EnvVar) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO env_vars (app_id, key, value_encrypted) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted`,
		appID, v.Key, v.ValueEncrypted); err != nil {
		return fmt.Errorf("put env var: %w", err)
	}
	return nil
}

// RenameEnvVar replaces the env var from with v in one transaction, so that
// no reader sees the variable missing or twice. It returns ErrNotFound when
// from does not exist and ErrConflict when v.Key already does.
func (s *Store) RenameEnvVar(ctx context.Context, appID int64, from string, v EnvVar) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename env var: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, from)
	if err != nil {
		return fmt.Errorf("rename env var: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("rename env var: %w", err)
	} else if n == 0 {
		return fmt.Errorf("env var %q: %w", from, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO env_vars (app_id, key, value_encrypted) VALUES (?, ?, ?)`,
		appID, v.Key, v.ValueEncrypted); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("env var %q: %w", v.Key, ErrConflict)
		}
		return fmt.Errorf("rename env var: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rename env var: %w", err)
	}
	return nil
}

// DeleteEnvVar removes one env var. It returns ErrNotFound when there is none.
func (s *Store) DeleteEnvVar(ctx context.Context, appID int64, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, key)
	if err != nil {
		return fmt.Errorf("delete env var: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete env var: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("env var %q: %w", key, ErrNotFound)
	}
	return nil
}
