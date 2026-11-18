package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a logged-in browser. Only a hash of the cookie value is stored,
// so a leaked database does not yield usable session tokens.
type Session struct {
	TokenHash    string
	GitHubUserID int64
	Login        string
	AvatarURL    string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, github_user_id, login, avatar_url, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.GitHubUserID, sess.Login, sess.AvatarURL,
		toMillis(sess.CreatedAt), toMillis(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns the unexpired session with the given token hash.
func (s *Store) GetSession(ctx context.Context, tokenHash string, now time.Time) (Session, error) {
	var (
		sess             Session
		created, expires int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, github_user_id, login, avatar_url, created_at, expires_at
		 FROM sessions WHERE token_hash = ? AND expires_at > ?`, tokenHash, toMillis(now)).
		Scan(&sess.TokenHash, &sess.GitHubUserID, &sess.Login, &sess.AvatarURL, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	sess.CreatedAt, sess.ExpiresAt = fromMillis(created), fromMillis(expires)
	return sess, nil
}

// DeleteSession removes a session (logout). Deleting an unknown session is
// not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that can no longer be used.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, toMillis(now)); err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}
