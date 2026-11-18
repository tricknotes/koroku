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
