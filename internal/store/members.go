package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Per-app roles of members (ARCHITECTURE 10.7), from the least to the most
// powerful. The owner has no role row: it may do everything.
const (
	RoleView   = "view"
	RoleDeploy = "deploy"
	RoleManage = "manage"
)

// ValidRole reports whether role is one a member can hold on an app.
func ValidRole(role string) bool {
	return role == RoleView || role == RoleDeploy || role == RoleManage
}

// Member is a GitHub user the owner let in. The ID is what counts; the login
// and avatar are for display.
type Member struct {
	GitHubUserID int64
	Login        string
	AvatarURL    string
	AddedAt      time.Time
	// Roles maps app IDs to the member's role on them.
	Roles map[int64]string
}

// Actor is who started an operation, recorded with it.
type Actor struct {
	UserID int64
	Login  string
}

type actorKey struct{}

// WithActor attaches the user acting in a request to ctx, so that the
// records an operation creates (deployments, task runs, database copies)
// name who started it without every layer passing it along.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor attached by WithActor.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// actorColumns returns the values of the actor_user_id and actor_login
// columns for a record created under ctx.
func actorColumns(ctx context.Context) (sql.NullInt64, string) {
	a, ok := ActorFrom(ctx)
	if !ok {
		return sql.NullInt64{}, ""
	}
	return sql.NullInt64{Int64: a.UserID, Valid: true}, a.Login
}

func actorFromColumns(id sql.NullInt64, login string) *Actor {
	if !id.Valid {
		return nil
	}
	return &Actor{UserID: id.Int64, Login: login}
}

// AddMember stores a new member without roles. It returns ErrConflict when
// the user is a member already.
func (s *Store) AddMember(ctx context.Context, m Member) error {
	if m.GitHubUserID <= 0 {
		return errors.New("add member: invalid user id")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO members (github_user_id, login, avatar_url, added_at) VALUES (?, ?, ?, ?)`,
		m.GitHubUserID, m.Login, m.AvatarURL, toMillis(m.AddedAt))
	if isUniqueViolation(err) {
		return fmt.Errorf("member %d: %w", m.GitHubUserID, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

// UpdateMemberProfile refreshes the login and avatar of a member, which
// may have changed on GitHub. Unknown users are ignored.
func (s *Store) UpdateMemberProfile(ctx context.Context, userID int64, login, avatarURL string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE members SET login = ?, avatar_url = ? WHERE github_user_id = ?`, login, avatarURL, userID); err != nil {
		return fmt.Errorf("update member: %w", err)
	}
	return nil
}

// GetMember returns one member with its roles.
func (s *Store) GetMember(ctx context.Context, userID int64) (Member, error) {
	var (
		m     Member
		added int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT github_user_id, login, avatar_url, added_at FROM members WHERE github_user_id = ?`, userID).
		Scan(&m.GitHubUserID, &m.Login, &m.AvatarURL, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrNotFound
	}
	if err != nil {
		return Member{}, fmt.Errorf("get member: %w", err)
	}
	m.AddedAt = fromMillis(added)
	roles, err := s.RolesOf(ctx, userID)
	if err != nil {
		return Member{}, err
	}
	m.Roles = roles
	return m, nil
}

// ListMembers returns every member with its roles, oldest first.
func (s *Store) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT github_user_id, login, avatar_url, added_at FROM members ORDER BY added_at, github_user_id`)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	var out []Member
	index := map[int64]int{}
	for rows.Next() {
		var (
			m     Member
			added int64
		)
		if err := rows.Scan(&m.GitHubUserID, &m.Login, &m.AvatarURL, &added); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list members: %w", err)
		}
		m.AddedAt = fromMillis(added)
		m.Roles = map[int64]string{}
		index[m.GitHubUserID] = len(out)
		out = append(out, m)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	roleRows, err := s.db.QueryContext(ctx, `SELECT github_user_id, app_id, role FROM app_roles`)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer func() { _ = roleRows.Close() }()
	for roleRows.Next() {
		var (
			userID, appID int64
			role          string
		)
		if err := roleRows.Scan(&userID, &appID, &role); err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		if i, ok := index[userID]; ok {
			out[i].Roles[appID] = role
		}
	}
	if err := roleRows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return out, nil
}

// CountMembers returns how many members there are.
func (s *Store) CountMembers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM members`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count members: %w", err)
	}
	return n, nil
}

// RolesOf returns the roles of a user, keyed by app ID. It is empty for
// users who are not members.
func (s *Store) RolesOf(ctx context.Context, userID int64) (map[int64]string, error) {
	// A review app has no roles of its own: its parent's apply
	// (ARCHITECTURE 9.9).
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, r.role FROM apps a
		 LEFT JOIN review_apps ra ON ra.app_id = a.id
		 JOIN app_roles r ON r.app_id = COALESCE(ra.parent_app_id, a.id)
		 WHERE r.github_user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var (
			appID int64
			role  string
		)
		if err := rows.Scan(&appID, &role); err != nil {
			return nil, fmt.Errorf("list roles: %w", err)
		}
		out[appID] = role
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	return out, nil
}

// AppRole returns the role of a user on an app, or "" when it has none.
// On a review app it is the role on the parent (ARCHITECTURE 9.9).
func (s *Store) AppRole(ctx context.Context, appID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT r.role FROM app_roles r
		 WHERE r.github_user_id = ? AND r.app_id = COALESCE(
		   (SELECT parent_app_id FROM review_apps WHERE app_id = ?), ?)`, userID, appID, appID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get role: %w", err)
	}
	return role, nil
}

// HasAnyRole reports whether a user holds a role on at least one app, which
// is what a member needs to log in.
func (s *Store) HasAnyRole(ctx context.Context, userID int64) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM app_roles WHERE github_user_id = ?`, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("check roles: %w", err)
	}
	return n > 0, nil
}

// SetAppRole gives a member a role on an app, replacing the one it had. An
// empty role takes the member's role away. It returns ErrNotFound when the
// user is not a member.
func (s *Store) SetAppRole(ctx context.Context, appID, userID int64, role string) error {
	if role != "" && !ValidRole(role) {
		return fmt.Errorf("set role: invalid role %q", role)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM members WHERE github_user_id = ?`, userID).Scan(&exists); err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if review, err := isReviewApp(ctx, s.db, appID); err != nil {
		return fmt.Errorf("set role: %w", err)
	} else if review && role != "" {
		return ErrRoleFollowsParent
	}
	var err error
	if role == "" {
		_, err = s.db.ExecContext(ctx,
			`DELETE FROM app_roles WHERE app_id = ? AND github_user_id = ?`, appID, userID)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO app_roles (app_id, github_user_id, role) VALUES (?, ?, ?)
			 ON CONFLICT (app_id, github_user_id) DO UPDATE SET role = excluded.role`,
			appID, userID, role)
	}
	if err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	return nil
}

// DeleteMember removes a member, its roles, sessions and API tokens. It
// returns ErrNotFound when the user is not a member.
func (s *Store) DeleteMember(ctx context.Context, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete member: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM members WHERE github_user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete member: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete member: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{
		`DELETE FROM sessions WHERE github_user_id = ?`,
		`DELETE FROM api_tokens WHERE github_user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return fmt.Errorf("delete member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete member: %w", err)
	}
	return nil
}

// RevokeCredentialsWithoutAccess ends the sessions and deletes the API
// tokens of every user who may no longer log in: anyone but the owner who
// holds no role. It runs after roles are taken away (directly, or by
// deleting an app) and reports how many sessions and tokens it removed.
func (s *Store) RevokeCredentialsWithoutAccess(ctx context.Context, ownerID int64) (int, error) {
	total := 0
	for _, table := range []string{"sessions", "api_tokens"} {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE github_user_id <> ?
			 AND github_user_id NOT IN (SELECT github_user_id FROM app_roles)`, ownerID)
		if err != nil {
			return total, fmt.Errorf("revoke credentials: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("revoke credentials: %w", err)
		}
		total += int(n)
	}
	return total, nil
}
