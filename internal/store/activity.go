package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ActivityKeep is how many activity entries an app keeps (ARCHITECTURE
// 10.7). Recording one more drops the oldest.
const ActivityKeep = 500

// Activity is one thing someone did to an app.
type Activity struct {
	ID    int64
	AppID int64
	At    time.Time
	// Actor is who did it; nil for koroku itself.
	Actor  *Actor
	Action string
	// Details is a JSON object whose shape depends on Action. It never
	// holds secret values.
	Details json.RawMessage
}

// RecordActivity stores an entry for app, done by the actor attached to
// ctx (none: koroku itself), and drops the app's entries beyond the newest
// ActivityKeep. details is marshaled to JSON; nil stores {}.
func (s *Store) RecordActivity(ctx context.Context, appID int64, action string, details any, now time.Time) (Activity, error) {
	if action == "" {
		return Activity{}, errors.New("record activity: empty action")
	}
	body := []byte("{}")
	if details != nil {
		var err error
		if body, err = json.Marshal(details); err != nil {
			return Activity{}, fmt.Errorf("record activity: %w", err)
		}
	}
	actorID, actorLogin := actorColumns(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Activity{}, fmt.Errorf("record activity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO app_activity (app_id, created_at, actor_user_id, actor_login, action, details)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		appID, toMillis(now), actorID, actorLogin, action, string(body))
	if err != nil {
		return Activity{}, fmt.Errorf("record activity: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Activity{}, fmt.Errorf("record activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM app_activity WHERE app_id = ? AND id <= (
			SELECT id FROM app_activity WHERE app_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		appID, appID, ActivityKeep); err != nil {
		return Activity{}, fmt.Errorf("record activity: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Activity{}, fmt.Errorf("record activity: %w", err)
	}
	return Activity{
		ID: id, AppID: appID, At: fromMillis(toMillis(now)), Actor: actorFromColumns(actorID, actorLogin),
		Action: action, Details: body,
	}, nil
}

// ListActivity returns up to limit entries of an app, newest first. With
// before > 0 it starts below that entry ID, for the next page.
func (s *Store) ListActivity(ctx context.Context, appID int64, before int64, limit int) ([]Activity, error) {
	query := `SELECT id, app_id, created_at, actor_user_id, actor_login, action, details
		FROM app_activity WHERE app_id = ?`
	args := []any{appID}
	if before > 0 {
		query += ` AND id < ?`
		args = append(args, before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Activity{}
	for rows.Next() {
		var (
			a       Activity
			at      int64
			actorID sql.NullInt64
			login   string
			details string
		)
		if err := rows.Scan(&a.ID, &a.AppID, &at, &actorID, &login, &a.Action, &details); err != nil {
			return nil, fmt.Errorf("list activity: %w", err)
		}
		a.At = fromMillis(at)
		a.Actor = actorFromColumns(actorID, login)
		a.Details = json.RawMessage(details)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	return out, nil
}
