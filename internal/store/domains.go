package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Domain statuses.
const (
	DomainPending = "pending"
	DomainActive  = "active"
)

// ErrDomainNotActive is returned when a pending domain is made primary.
var ErrDomainNotActive = errors.New("domain is not active")

// Domain is a custom hostname attached to an app.
type Domain struct {
	ID            int64
	AppID         int64
	Hostname      string
	Status        string
	IsPrimary     bool
	ForceActive   bool
	LastCheckedAt *time.Time
	LastError     string
	CreatedAt     time.Time
}

const domainColumns = `id, app_id, hostname, status, is_primary, force_active,
	last_checked_at, last_error, created_at`

func scanDomain(row rowScanner) (Domain, error) {
	var (
		d       Domain
		checked sql.NullInt64
		created int64
	)
	err := row.Scan(&d.ID, &d.AppID, &d.Hostname, &d.Status, &d.IsPrimary, &d.ForceActive,
		&checked, &d.LastError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Domain{}, ErrNotFound
	}
	if err != nil {
		return Domain{}, fmt.Errorf("scan domain: %w", err)
	}
	d.LastCheckedAt = fromNullMillis(checked)
	d.CreatedAt = fromMillis(created)
	return d, nil
}

func (s *Store) queryDomains(ctx context.Context, query string, args ...any) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	return out, nil
}

// AddDomain registers a hostname for an app in the pending state. Hostnames
// are unique across all apps.
func (s *Store) AddDomain(ctx context.Context, appID int64, hostname string, now time.Time) (Domain, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO app_domains (app_id, hostname, created_at) VALUES (?, ?, ?)`,
		appID, hostname, toMillis(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Domain{}, fmt.Errorf("hostname %q: %w", hostname, ErrConflict)
		}
		return Domain{}, fmt.Errorf("add domain: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Domain{}, fmt.Errorf("add domain: %w", err)
	}
	return s.GetDomain(ctx, appID, id)
}

// GetDomain returns a domain of the given app.
func (s *Store) GetDomain(ctx context.Context, appID, id int64) (Domain, error) {
	return scanDomain(s.db.QueryRowContext(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE id = ? AND app_id = ?`, id, appID))
}

// ListDomains returns the domains of an app ordered by hostname.
func (s *Store) ListDomains(ctx context.Context, appID int64) ([]Domain, error) {
	return s.queryDomains(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE app_id = ? ORDER BY hostname`, appID)
}

// ListActiveDomains returns every active domain of every app.
func (s *Store) ListActiveDomains(ctx context.Context) ([]Domain, error) {
	return s.queryDomains(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE status = ? ORDER BY hostname`, DomainActive)
}

// ListPendingDomains returns the domains that still wait for DNS.
func (s *Store) ListPendingDomains(ctx context.Context) ([]Domain, error) {
	return s.queryDomains(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE status = ? ORDER BY id`, DomainPending)
}

// RecordDomainCheck stores the outcome of a DNS verification. A forced
// domain stays active whatever the outcome.
func (s *Store) RecordDomainCheck(ctx context.Context, id int64, verified bool, lastError string, now time.Time) error {
	status := DomainPending
	if verified {
		status = DomainActive
	}
	res, err := s.db.ExecContext(ctx, `UPDATE app_domains SET
			status = CASE WHEN force_active = 1 THEN 'active' ELSE ? END,
			is_primary = CASE WHEN force_active = 1 OR ? = 'active' THEN is_primary ELSE 0 END,
			last_checked_at = ?, last_error = ?
		WHERE id = ?`, status, status, toMillis(now), lastError, id)
	if err != nil {
		return fmt.Errorf("record domain check: %w", err)
	}
	return requireOneRow(res, "record domain check")
}

// SetDomainForceActive turns forced activation on or off. Turning it off
// returns the domain to pending until DNS is verified again.
func (s *Store) SetDomainForceActive(ctx context.Context, appID, id int64, force bool) error {
	var (
		res sql.Result
		err error
	)
	if force {
		res, err = s.db.ExecContext(ctx,
			`UPDATE app_domains SET force_active = 1, status = 'active' WHERE id = ? AND app_id = ?`, id, appID)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE app_domains SET force_active = 0, status = 'pending', is_primary = 0
			 WHERE id = ? AND app_id = ? AND force_active = 1`, id, appID)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				// Not forced before: nothing changes, but the row must exist.
				_, err = s.GetDomain(ctx, appID, id)
				return err
			}
		}
	}
	if err != nil {
		return fmt.Errorf("set domain force_active: %w", err)
	}
	return requireOneRow(res, "set domain force_active")
}

// SetDomainPrimary makes a domain the app's primary hostname (other
// hostnames then redirect to it) or clears that role.
func (s *Store) SetDomainPrimary(ctx context.Context, appID, id int64, primary bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set primary domain: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	d, err := scanDomain(tx.QueryRowContext(ctx,
		`SELECT `+domainColumns+` FROM app_domains WHERE id = ? AND app_id = ?`, id, appID))
	if err != nil {
		return err
	}
	if primary && d.Status != DomainActive {
		return ErrDomainNotActive
	}
	if primary {
		if _, err := tx.ExecContext(ctx,
			`UPDATE app_domains SET is_primary = 0 WHERE app_id = ? AND is_primary = 1`, appID); err != nil {
			return fmt.Errorf("set primary domain: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE app_domains SET is_primary = ? WHERE id = ?`, primary, id); err != nil {
		return fmt.Errorf("set primary domain: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set primary domain: %w", err)
	}
	return nil
}

// DeleteDomain removes a domain from an app.
func (s *Store) DeleteDomain(ctx context.Context, appID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM app_domains WHERE id = ? AND app_id = ?`, id, appID)
	if err != nil {
		return fmt.Errorf("delete domain: %w", err)
	}
	return requireOneRow(res, "delete domain")
}
