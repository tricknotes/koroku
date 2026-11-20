package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GateGrant lets one browser open one protected app hostname while the
// console session it was issued from lasts (ARCHITECTURE 8.3). Only a hash
// of the cookie value is stored.
type GateGrant struct {
	TokenHash        string
	Hostname         string
	SessionTokenHash string
	CreatedAt        time.Time
	ExpiresAt        time.Time
}

// CreateGateCode stores a one-time code that can be redeemed once, on
// hostname, before expiresAt. Expired codes are removed on the way.
func (s *Store) CreateGateCode(ctx context.Context, codeHash, hostname, sessionTokenHash string, expiresAt, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create gate code: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM gate_codes WHERE expires_at <= ?`, toMillis(now)); err != nil {
		return fmt.Errorf("create gate code: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gate_codes (code_hash, hostname, session_token_hash, expires_at) VALUES (?, ?, ?, ?)`,
		codeHash, hostname, sessionTokenHash, toMillis(expiresAt)); err != nil {
		return fmt.Errorf("create gate code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create gate code: %w", err)
	}
	return nil
}

// RedeemGateCode consumes a code issued for hostname and returns the hash of
// the session it was issued from. A code is gone after the first attempt to
// redeem it on its hostname. It returns ErrNotFound for unknown, expired,
// used and foreign-host codes.
func (s *Store) RedeemGateCode(ctx context.Context, codeHash, hostname string, now time.Time) (string, error) {
	var sessionHash string
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM gate_codes WHERE code_hash = ? AND hostname = ? AND expires_at > ?
		 RETURNING session_token_hash`,
		codeHash, hostname, toMillis(now)).Scan(&sessionHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redeem gate code: %w", err)
	}
	return sessionHash, nil
}

// CreateGateGrant stores a grant. Expired grants are removed on the way.
func (s *Store) CreateGateGrant(ctx context.Context, g GateGrant, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create gate grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM gate_grants WHERE expires_at <= ?`, toMillis(now)); err != nil {
		return fmt.Errorf("create gate grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gate_grants (token_hash, hostname, session_token_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		g.TokenHash, g.Hostname, g.SessionTokenHash, toMillis(g.CreatedAt), toMillis(g.ExpiresAt)); err != nil {
		return fmt.Errorf("create gate grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create gate grant: %w", err)
	}
	return nil
}

// GateGrantSession returns the console session behind an unexpired grant for
// hostname, provided that the session is unexpired too. Deleting a session
// deletes its grants. It returns ErrNotFound otherwise.
func (s *Store) GateGrantSession(ctx context.Context, tokenHash, hostname string, now time.Time) (Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.id, s.github_user_id, s.login, s.avatar_url, s.created_at, s.expires_at,
		        s.last_seen_at, s.ip, s.user_agent
		 FROM gate_grants g JOIN sessions s ON s.token_hash = g.session_token_hash
		 WHERE g.token_hash = ? AND g.hostname = ? AND g.expires_at > ? AND s.expires_at > ?`,
		tokenHash, hostname, toMillis(now), toMillis(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get gate grant: %w", err)
	}
	return sess, nil
}

// DeleteGateGrant removes a grant (logout on the app host). Deleting an
// unknown grant is not an error.
func (s *Store) DeleteGateGrant(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gate_grants WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete gate grant: %w", err)
	}
	return nil
}

// GetActiveDomainByHostname returns the active custom domain with the given
// hostname. It returns ErrNotFound for unknown and pending hostnames.
func (s *Store) GetActiveDomainByHostname(ctx context.Context, hostname string) (Domain, error) {
	d, err := scanDomain(s.db.QueryRowContext(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE hostname = ? AND status = ?`, hostname, DomainActive))
	if errors.Is(err, ErrNotFound) {
		return Domain{}, ErrNotFound
	}
	if err != nil {
		return Domain{}, fmt.Errorf("get domain: %w", err)
	}
	return d, nil
}
