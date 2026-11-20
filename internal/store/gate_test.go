package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestAccessProtectionMigration(t *testing.T) {
	ctx := context.Background()
	// Roll the schema back to before migration 8 with two apps on it.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if _, err := fresh.db.ExecContext(ctx, `DROP TABLE gate_grants; DROP TABLE gate_codes;
		ALTER TABLE apps DROP COLUMN access_protection; ALTER TABLE apps DROP COLUMN protection_exempt_paths;
		DELETE FROM schema_migrations WHERE version >= 8`); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.db.ExecContext(ctx, `INSERT INTO apps (slug, web_port, host_port, environment, created_at, updated_at)
		VALUES ('shop', 3000, 20000, 'production', 0, 0), ('shop-staging', 3000, 20001, 'staging', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version >= 8 {
			if err := fresh.applyMigration(ctx, m); err != nil {
				t.Fatalf("apply %s: %v", m.name, err)
			}
		}
	}
	prod, err := fresh.GetApp(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	staging, err := fresh.GetApp(ctx, "shop-staging")
	if err != nil {
		t.Fatal(err)
	}
	if prod.AccessProtection || !staging.AccessProtection {
		t.Errorf("after migration: production %v, staging %v", prod.AccessProtection, staging.AccessProtection)
	}
	if staging.ProtectionExemptPaths == nil || len(staging.ProtectionExemptPaths) != 0 {
		t.Errorf("exempt paths = %#v", staging.ProtectionExemptPaths)
	}
}

func TestAppProtectionRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	app, err := s.CreateApp(ctx, App{Slug: "blog", WebPort: 3000, AccessProtection: true,
		ProtectionExemptPaths: []string{"/webhooks/"}}, 20000, 20999, now)
	if err != nil {
		t.Fatal(err)
	}
	if !app.AccessProtection || !slices.Equal(app.ProtectionExemptPaths, []string{"/webhooks/"}) {
		t.Errorf("created %+v", app)
	}
	app.AccessProtection = false
	app.ProtectionExemptPaths = nil
	if err := s.UpdateApp(ctx, app, now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetApp(ctx, "blog")
	if got.AccessProtection || len(got.ProtectionExemptPaths) != 0 {
		t.Errorf("updated %+v", got)
	}
}

func TestGateCodesAndGrants(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	sess := Session{TokenHash: "sess", ID: "1", GitHubUserID: 42, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateGateCode(ctx, "code", "a.example.com", "sess", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "code", "b.example.com", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeemed on another host: %v", err)
	}
	if got, err := s.RedeemGateCode(ctx, "code", "a.example.com", now); err != nil || got != "sess" {
		t.Errorf("redeem = %q, %v", got, err)
	}
	if _, err := s.RedeemGateCode(ctx, "code", "a.example.com", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeemed twice: %v", err)
	}
	if err := s.CreateGateCode(ctx, "late", "a.example.com", "sess", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemGateCode(ctx, "late", "a.example.com", now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeemed an expired code: %v", err)
	}

	grant := GateGrant{TokenHash: "g", Hostname: "a.example.com", SessionTokenHash: "sess", CreatedAt: now, ExpiresAt: sess.ExpiresAt}
	if err := s.CreateGateGrant(ctx, grant, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GateGrantSession(ctx, "g", "a.example.com", now); err != nil || got.GitHubUserID != 42 {
		t.Errorf("grant = %+v, %v", got, err)
	}
	if _, err := s.GateGrantSession(ctx, "g", "b.example.com", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("grant valid on another host: %v", err)
	}
	if _, err := s.GateGrantSession(ctx, "g", "a.example.com", sess.ExpiresAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("grant valid after expiry: %v", err)
	}

	// Deleting the session takes its codes and grants with it.
	if err := s.CreateGateCode(ctx, "pending", "a.example.com", "sess", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "sess"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM gate_grants) + (SELECT count(*) FROM gate_codes)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d codes and grants outlived their session", left)
	}
}
