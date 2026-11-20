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
	TokenHash string
	// ID names the session in the console; unlike the token hash it may be
	// shown and used to revoke the session.
	ID           string
	GitHubUserID int64
	Login        string
	AvatarURL    string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastSeenAt   time.Time
	IP           string
	UserAgent    string
}

const sessionColumns = `token_hash, id, github_user_id, login, avatar_url, created_at, expires_at, last_seen_at, ip, user_agent`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		sess                       Session
		created, expires, lastSeen int64
	)
	err := row.Scan(&sess.TokenHash, &sess.ID, &sess.GitHubUserID, &sess.Login, &sess.AvatarURL,
		&created, &expires, &lastSeen, &sess.IP, &sess.UserAgent)
	if err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt = fromMillis(created), fromMillis(expires), fromMillis(lastSeen)
	return sess, nil
}

// CreateSession stores a new session. LastSeenAt defaults to CreatedAt.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if sess.ID == "" {
		return errors.New("create session: empty id")
	}
	if sess.LastSeenAt.IsZero() {
		sess.LastSeenAt = sess.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.ID, sess.GitHubUserID, sess.Login, sess.AvatarURL,
		toMillis(sess.CreatedAt), toMillis(sess.ExpiresAt), toMillis(sess.LastSeenAt),
		sess.IP, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns the unexpired session with the given token hash.
func (s *Store) GetSession(ctx context.Context, tokenHash string, now time.Time) (Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, toMillis(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// ListSessions returns the unexpired sessions of a user, newest first.
func (s *Store) ListSessions(ctx context.Context, githubUserID int64, now time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE github_user_id = ? AND expires_at > ? ORDER BY created_at DESC, id`,
		githubUserID, toMillis(now))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// TouchSession records that a session was used, along with where from.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, at time.Time, ip, userAgent string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, ip = ?, user_agent = ? WHERE token_hash = ?`,
		toMillis(at), ip, userAgent, tokenHash)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession removes a session (logout). Deleting an unknown session is
// not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteSessionByID removes a session of a user by its public ID. It returns
// ErrNotFound when the user has no such session.
func (s *Store) DeleteSessionByID(ctx context.Context, githubUserID int64, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ? AND github_user_id = ?`, id, githubUserID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete session: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteOtherSessions removes every session of a user except the one with
// keepTokenHash, and reports how many unexpired sessions it ended.
func (s *Store) DeleteOtherSessions(ctx context.Context, githubUserID int64, keepTokenHash string, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE github_user_id = ? AND token_hash <> ? AND expires_at > ?`,
		githubUserID, keepTokenHash, toMillis(now)).Scan(&live); err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE github_user_id = ? AND token_hash <> ?`,
		githubUserID, keepTokenHash); err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete other sessions: %w", err)
	}
	return live, nil
}

// DeleteExpiredSessions removes sessions that can no longer be used.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, toMillis(now)); err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}

// Login outcomes recorded in the login history. Histories written by
// older versions may also hold "two_factor_required" and
// "two_factor_unknown"; they are kept and shown as they are.
const (
	LoginSucceeded  = "succeeded"
	LoginNotAllowed = "not_allowed"
)

// LoginEvent is one login attempt by a known GitHub user.
type LoginEvent struct {
	At           time.Time
	Outcome      string
	GitHubUserID int64
	Login        string
	IP           string
	UserAgent    string
}

// loginEventsKept bounds the login history. Anyone with a GitHub account can
// add a rejected attempt, so the table must not grow without limit.
const loginEventsKept = 200

// AddLoginEvent records a login attempt and drops the oldest beyond the limit.
func (s *Store) AddLoginEvent(ctx context.Context, ev LoginEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add login event: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO login_events (at, outcome, github_user_id, login, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		toMillis(ev.At), ev.Outcome, ev.GitHubUserID, ev.Login, ev.IP, ev.UserAgent); err != nil {
		return fmt.Errorf("add login event: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM login_events WHERE id NOT IN
		 (SELECT id FROM login_events ORDER BY id DESC LIMIT ?)`, loginEventsKept); err != nil {
		return fmt.Errorf("add login event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("add login event: %w", err)
	}
	return nil
}

// ListLoginEvents returns the newest login attempts first.
func (s *Store) ListLoginEvents(ctx context.Context, limit int) ([]LoginEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, outcome, github_user_id, login, ip, user_agent
		 FROM login_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list login events: %w", err)
	}
	defer rows.Close()
	var out []LoginEvent
	for rows.Next() {
		var (
			ev LoginEvent
			at int64
		)
		if err := rows.Scan(&at, &ev.Outcome, &ev.GitHubUserID, &ev.Login, &ev.IP, &ev.UserAgent); err != nil {
			return nil, fmt.Errorf("list login events: %w", err)
		}
		ev.At = fromMillis(at)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list login events: %w", err)
	}
	return out, nil
}
