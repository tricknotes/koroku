package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RegistryCredential is the container registry login of one app
// (ARCHITECTURE 6.1). The token is sealed with the master key.
type RegistryCredential struct {
	AppID          int64
	Registry       string
	Username       string
	TokenEncrypted []byte
	UpdatedAt      time.Time
}

// GetRegistryCredential returns the app's credential, or ErrNotFound when it
// has none.
func (s *Store) GetRegistryCredential(ctx context.Context, appID int64) (RegistryCredential, error) {
	c := RegistryCredential{AppID: appID}
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT registry, username, token_encrypted, updated_at FROM registry_credentials WHERE app_id = ?`,
		appID).Scan(&c.Registry, &c.Username, &c.TokenEncrypted, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistryCredential{}, fmt.Errorf("registry credential of app %d: %w", appID, ErrNotFound)
	}
	if err != nil {
		return RegistryCredential{}, fmt.Errorf("get registry credential: %w", err)
	}
	c.UpdatedAt = fromMillis(updated)
	return c, nil
}

// PutRegistryCredential sets the app's credential, replacing any earlier one.
func (s *Store) PutRegistryCredential(ctx context.Context, c RegistryCredential) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO registry_credentials (app_id, registry, username, token_encrypted, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (app_id) DO UPDATE SET
		   registry = excluded.registry,
		   username = excluded.username,
		   token_encrypted = excluded.token_encrypted,
		   updated_at = excluded.updated_at`,
		c.AppID, c.Registry, c.Username, c.TokenEncrypted, toMillis(c.UpdatedAt)); err != nil {
		return fmt.Errorf("put registry credential: %w", err)
	}
	return nil
}

// DeleteRegistryCredential removes the app's credential. It returns
// ErrNotFound when there is none.
func (s *Store) DeleteRegistryCredential(ctx context.Context, appID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM registry_credentials WHERE app_id = ?`, appID)
	if err != nil {
		return fmt.Errorf("delete registry credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete registry credential: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry credential of app %d: %w", appID, ErrNotFound)
	}
	return nil
}
