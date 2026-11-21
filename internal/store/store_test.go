package store

import (
	"context"
	"os"
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
	path := filepath.Join(t.TempDir(), "nested", "koroku.db")

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

func TestVacuumInto(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "koroku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO apps (slug, web_port, host_port, created_at, updated_at) VALUES ('demo', 3000, 20000, 0, 0)`); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(dir, "copy.db")
	if err := s.VacuumInto(ctx, copyPath); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}
	// The copy is a complete database of its own.
	restored, err := Open(ctx, copyPath)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() { _ = restored.Close() }()
	var slug string
	if err := restored.db.QueryRowContext(ctx, `SELECT slug FROM apps`).Scan(&slug); err != nil || slug != "demo" {
		t.Errorf("copy content: %q, %v", slug, err)
	}
	version, _ := restored.SchemaVersion(ctx)
	want, _ := s.SchemaVersion(ctx)
	if version != want {
		t.Errorf("schema version of the copy = %d, want %d", version, want)
	}
	// An existing destination is never overwritten.
	if err := s.VacuumInto(ctx, copyPath); err == nil {
		t.Error("VacuumInto must refuse an existing file")
	}
}

func TestDatabaseFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "koroku.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "koroku.db")
	rw, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := rw.db.ExecContext(ctx,
		`INSERT INTO apps (slug, web_port, host_port, created_at, updated_at) VALUES ('demo', 3000, 20000, 0, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The writer stays open, as `koroku serve` does next to `koroku run`.
	defer func() { _ = rw.Close() }()

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()
	app, err := ro.GetApp(ctx, "demo")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Slug != "demo" {
		t.Errorf("slug = %q, want demo", app.Slug)
	}
	if _, err := ro.db.ExecContext(ctx, `DELETE FROM apps`); err == nil {
		t.Error("a write through the read-only store succeeded")
	}
}

func TestOpenReadOnlyRefusesOtherSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "koroku.db")
	rw, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := rw.db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = rw.Close()
	if _, err := OpenReadOnly(ctx, path); err == nil {
		t.Fatal("OpenReadOnly accepted a database of another schema version")
	}
}

func TestOpenReadOnlyMissingFile(t *testing.T) {
	if _, err := OpenReadOnly(context.Background(), filepath.Join(t.TempDir(), "none.db")); err == nil {
		t.Fatal("OpenReadOnly created or accepted a missing database")
	}
}
