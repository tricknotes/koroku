package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrLinkTarget is returned when a link would point at an app outside the
// consumer's group or at a service the target does not expose.
var ErrLinkTarget = errors.New("the target is not an exposed service of another app in the group")

// LinkNetworkPrefix starts the name of every link network. Slugs cannot
// contain "_", so no app's own network (koroku-<slug>_*) starts with it.
const LinkNetworkPrefix = "koroku_link_"

// Link says that Name in the consumer's (AppID) Compose file means the
// TargetService of TargetApp (ARCHITECTURE 6.1).
type Link struct {
	ID            int64
	AppID         int64
	AppSlug       string
	Name          string
	TargetAppID   int64
	TargetAppSlug string
	TargetService string
	// Services are the consumer's services that use the name.
	Services  []string
	CreatedAt time.Time
}

// Network is the Docker network of the link in the given installation.
func (l Link) Network(installation string) string {
	return LinkNetworkPrefix + installation + "_" + strconv.FormatInt(l.ID, 10)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const linkSelect = `SELECT l.id, l.app_id, a.slug, l.name, l.target_app_id, t.slug, l.target_service, l.services, l.created_at
	FROM app_links l JOIN apps a ON a.id = l.app_id JOIN apps t ON t.id = l.target_app_id `

func queryLinks(ctx context.Context, q querier, where string, args ...any) ([]Link, error) {
	rows, err := q.QueryContext(ctx, linkSelect+where+` ORDER BY a.slug, l.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var links []Link
	for rows.Next() {
		var (
			l        Link
			services string
			created  int64
		)
		if err := rows.Scan(&l.ID, &l.AppID, &l.AppSlug, &l.Name, &l.TargetAppID, &l.TargetAppSlug, &l.TargetService, &services, &created); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		l.Services = []string{}
		if err := json.Unmarshal([]byte(services), &l.Services); err != nil {
			return nil, fmt.Errorf("decode services of link %d: %w", l.ID, err)
		}
		l.CreatedAt = fromMillis(created)
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	return links, nil
}

// ListLinks returns every link.
func (s *Store) ListLinks(ctx context.Context) ([]Link, error) {
	return queryLinks(ctx, s.db, ``)
}

// ListAppLinks returns the links the app is the consumer of.
func (s *Store) ListAppLinks(ctx context.Context, appID int64) ([]Link, error) {
	return queryLinks(ctx, s.db, `WHERE l.app_id = ?`, appID)
}

// ListLinksOf returns the links the app takes part in, on either end.
func (s *Store) ListLinksOf(ctx context.Context, appID int64) ([]Link, error) {
	return queryLinks(ctx, s.db, `WHERE l.app_id = ? OR l.target_app_id = ?`, appID, appID)
}

// GetLink returns one link of a consumer app.
func (s *Store) GetLink(ctx context.Context, appID, id int64) (Link, error) {
	links, err := queryLinks(ctx, s.db, `WHERE l.app_id = ? AND l.id = ?`, appID, id)
	if err != nil {
		return Link{}, err
	}
	if len(links) == 0 {
		return Link{}, ErrNotFound
	}
	return links[0], nil
}

// CreateLink records a link. The consumer and the target must be different
// apps in the same group and the target must expose the service
// (ErrLinkTarget); a name the consumer already uses is ErrConflict.
func (s *Store) CreateLink(ctx context.Context, appID int64, name string, targetAppID int64, targetService string, services []string, now time.Time) (Link, error) {
	encoded, err := encodeStrings(services)
	if err != nil {
		return Link{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		appGroup, targetGroup sql.NullInt64
		exposedJSON           string
	)
	if err := tx.QueryRowContext(ctx, `SELECT group_id FROM apps WHERE id = ?`, appID).Scan(&appGroup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Link{}, fmt.Errorf("create link: app: %w", ErrNotFound)
		}
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	err = tx.QueryRowContext(ctx, `SELECT group_id, group_services FROM apps WHERE id = ?`, targetAppID).Scan(&targetGroup, &exposedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrLinkTarget
	}
	if err != nil {
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	var exposed []string
	if err := json.Unmarshal([]byte(exposedJSON), &exposed); err != nil {
		return Link{}, fmt.Errorf("create link: decode group_services: %w", err)
	}
	if appID == targetAppID || !appGroup.Valid || !targetGroup.Valid || appGroup.Int64 != targetGroup.Int64 || !containsString(exposed, targetService) {
		return Link{}, ErrLinkTarget
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO app_links (app_id, name, target_app_id, target_service, services, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, appID, name, targetAppID, targetService, encoded, toMillis(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Link{}, fmt.Errorf("link %q: %w", name, ErrConflict)
		}
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	links, err := queryLinks(ctx, tx, `WHERE l.id = ?`, id)
	if err != nil || len(links) != 1 {
		return Link{}, fmt.Errorf("create link: read back: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return Link{}, fmt.Errorf("create link: %w", err)
	}
	return links[0], nil
}

// DeleteLink removes one link of a consumer app.
func (s *Store) DeleteLink(ctx context.Context, appID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM app_links WHERE app_id = ? AND id = ?`, appID, id)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	return requireOneRow(res, "delete link")
}

// InstallationID returns the random ID of this installation, creating it
// on first use. Docker objects koroku shares between apps carry it, so that
// another koroku on the same daemon never touches them.
func (s *Store) InstallationID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'installation_id'`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read installation id: %w", err)
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate installation id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES ('installation_id', ?)`, hex.EncodeToString(b)); err != nil {
		return "", fmt.Errorf("store installation id: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'installation_id'`).Scan(&id); err != nil {
		return "", fmt.Errorf("read installation id: %w", err)
	}
	return id, nil
}
