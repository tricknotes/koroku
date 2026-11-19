package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	// ErrGroupNotEmpty is returned when a group that still has members is
	// deleted.
	ErrGroupNotEmpty = errors.New("the group has members")
	// ErrGroupEnvironment is returned when an app and its group would have
	// different environments (ARCHITECTURE 6.1: staging must not reach
	// production through a group).
	ErrGroupEnvironment = errors.New("the app's environment differs from the group's")
)

// GroupNetworkPrefix starts the name of every group network. Slugs cannot
// contain "_", so no app's own network (koroku-<slug>_*) starts with it.
const GroupNetworkPrefix = "koroku_group_"

// Group is a set of apps whose chosen services share a network.
type Group struct {
	ID          int64
	Name        string
	Environment string
	CreatedAt   time.Time
}

// Network is the name of the group's Docker network.
func (g Group) Network() string { return GroupNetworkPrefix + strconv.FormatInt(g.ID, 10) }

const groupColumns = `id, name, environment, created_at`

func scanGroup(row rowScanner) (Group, error) {
	var (
		g       Group
		created int64
	)
	err := row.Scan(&g.ID, &g.Name, &g.Environment, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("scan group: %w", err)
	}
	g.CreatedAt = fromMillis(created)
	return g, nil
}

// CreateGroup inserts a group. A taken name is ErrConflict.
func (s *Store) CreateGroup(ctx context.Context, name, environment string, now time.Time) (Group, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO app_groups (name, environment, created_at) VALUES (?, ?, ?)`,
		name, environment, toMillis(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Group{}, fmt.Errorf("group %q: %w", name, ErrConflict)
		}
		return Group{}, fmt.Errorf("create group: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Group{}, fmt.Errorf("create group: %w", err)
	}
	return s.GetGroup(ctx, id)
}

// GetGroup returns the group with the given ID.
func (s *Store) GetGroup(ctx context.Context, id int64) (Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM app_groups WHERE id = ?`, id))
}

// ListGroups returns every group ordered by name.
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+groupColumns+` FROM app_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var groups []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	return groups, nil
}

// RenameGroup changes the name of a group. A taken name is ErrConflict.
func (s *Store) RenameGroup(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE app_groups SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("group %q: %w", name, ErrConflict)
		}
		return fmt.Errorf("rename group: %w", err)
	}
	return requireOneRow(res, "rename group")
}

// DeleteGroup removes a group without members; otherwise ErrGroupNotEmpty.
func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var members int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE group_id = ?`, id).Scan(&members); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if members > 0 {
		return ErrGroupNotEmpty
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM app_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if err := requireOneRow(res, "delete group"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	return nil
}

// ListGroupMembers returns the apps in a group ordered by slug.
func (s *Store) ListGroupMembers(ctx context.Context, groupID int64) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appColumns+` FROM apps WHERE group_id = ? ORDER BY slug`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var apps []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	return apps, nil
}

// SetAppGroup puts an app in a group (or takes it out with a nil groupID)
// and sets the services that join the group network. The group must exist
// and have the app's environment.
func (s *Store) SetAppGroup(ctx context.Context, appID int64, groupID *int64, services []string, now time.Time) error {
	if groupID == nil && len(services) > 0 {
		return errors.New("set app group: services given without a group")
	}
	encoded, err := encodeStrings(services)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set app group: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if groupID != nil {
		var appEnv, groupEnv string
		if err := tx.QueryRowContext(ctx, `SELECT environment FROM apps WHERE id = ?`, appID).Scan(&appEnv); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("set app group: app: %w", ErrNotFound)
			}
			return fmt.Errorf("set app group: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT environment FROM app_groups WHERE id = ?`, *groupID).Scan(&groupEnv); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("set app group: group: %w", ErrNotFound)
			}
			return fmt.Errorf("set app group: %w", err)
		}
		if appEnv != groupEnv {
			return ErrGroupEnvironment
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE apps SET group_id = ?, group_services = ?, updated_at = ? WHERE id = ?`,
		nullInt(groupID), encoded, toMillis(now), appID)
	if err != nil {
		return fmt.Errorf("set app group: %w", err)
	}
	if err := requireOneRow(res, "set app group"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set app group: %w", err)
	}
	return nil
}
