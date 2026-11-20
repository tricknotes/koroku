package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTooManyTokens is returned when issuing a token would exceed the limit
// of live API tokens.
var ErrTooManyTokens = errors.New("too many api tokens")

// APIToken is a read-only API token (ARCHITECTURE 10.5). Only a hash of its
// value is stored, so a leaked database does not yield usable tokens.
type APIToken struct {
	// ID names the token in the console; unlike the hash it may be shown.
	ID         string
	TokenHash  string
	Name       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	LastUsedIP string
}

const apiTokenColumns = `id, token_hash, name, created_at, expires_at, last_used_at, last_used_ip`

func scanAPIToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var (
		tok              APIToken
		created, expires int64
		lastUsed         sql.NullInt64
	)
	if err := row.Scan(&tok.ID, &tok.TokenHash, &tok.Name, &created, &expires, &lastUsed, &tok.LastUsedIP); err != nil {
		return APIToken{}, err
	}
	tok.CreatedAt, tok.ExpiresAt, tok.LastUsedAt = fromMillis(created), fromMillis(expires), fromNullMillis(lastUsed)
	return tok, nil
}

// CreateAPIToken stores a new token after removing expired ones. It returns
// ErrTooManyTokens when max unexpired tokens exist already.
func (s *Store) CreateAPIToken(ctx context.Context, tok APIToken, max int) error {
	if tok.ID == "" || tok.TokenHash == "" {
		return errors.New("create api token: empty id or hash")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at <= ?`, toMillis(tok.CreatedAt)); err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens`).Scan(&live); err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	if live >= max {
		return ErrTooManyTokens
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO api_tokens (id, token_hash, name, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tok.ID, tok.TokenHash, tok.Name, toMillis(tok.CreatedAt), toMillis(tok.ExpiresAt)); err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	return nil
}

// GetAPIToken returns the unexpired token with the given hash.
func (s *Store) GetAPIToken(ctx context.Context, tokenHash string, now time.Time) (APIToken, error) {
	tok, err := scanAPIToken(s.db.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, toMillis(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("get api token: %w", err)
	}
	return tok, nil
}

// ListAPITokens returns the unexpired tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, now time.Time) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE expires_at > ? ORDER BY created_at DESC, id`,
		toMillis(now))
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list api tokens: %w", err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	return out, nil
}

// TouchAPIToken records that a token was used, and from where.
func (s *Store) TouchAPIToken(ctx context.Context, id string, at time.Time, ip string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, toMillis(at), ip, id); err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	return nil
}

// DeleteAPIToken revokes a token. It returns ErrNotFound when there is none.
func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete api token: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
