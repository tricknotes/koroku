package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if want := migrations[len(migrations)-1].version; version != want {
		t.Errorf("SchemaVersion = %d, want %d", version, want)
	}

	for _, table := range []string{"apps", "env_vars", "deployments", "sessions"} {
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestOpenIsIdempotentOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "paas.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.db.ExecContext(ctx,
		`INSERT INTO apps (slug, web_port, host_port, created_at, updated_at) VALUES ('demo', 3000, 20000, 0, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()
	var count int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("apps rows = %d, want 1 (data must survive reopen)", count)
	}
	var applied int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	migrations, _ := loadMigrations()
	if applied != len(migrations) {
		t.Errorf("schema_migrations rows = %d, want %d", applied, len(migrations))
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := openTest(t)
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO deployments (app_id, commit_sha, queued_at) VALUES (999, 'abc', 0)`)
	if err == nil {
		t.Fatal("expected a foreign key violation")
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	insert := `INSERT INTO apps (slug, web_port, host_port, created_at, updated_at) VALUES (?, 3000, ?, 0, 0)`
	if _, err := s.db.ExecContext(ctx, insert, "a", 20000); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, insert, "a", 20001); err == nil {
		t.Error("duplicate slug must be rejected")
	}
	if _, err := s.db.ExecContext(ctx, insert, "b", 20000); err == nil {
		t.Error("duplicate host_port must be rejected")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE apps SET desired_state = 'paused' WHERE slug = 'a'`); err == nil {
		t.Error("invalid desired_state must be rejected")
	}
}
