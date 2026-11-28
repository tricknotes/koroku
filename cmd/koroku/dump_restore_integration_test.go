//go:build integration

package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// dumpRestoreApp is a PostgreSQL database behind a small web service.
const dumpRestoreApp = `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
  db:
    image: postgres:17.10-alpine
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U app -d app"]
      interval: 1s
      timeout: 5s
      retries: 60
volumes:
  pgdata:
`

// TestServeRestoresADump restores the dump of an app into its own
// database with the real koroku serve, PostgreSQL, restic and an
// S3-compatible server (ARCHITECTURE 10.8): a deleted row comes back, the
// data before the restore is kept as a dump, and a dump that cannot be
// loaded leaves the data as it was and the app running.
func TestServeRestoresADump(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed (it creates the test bucket)")
	}
	endpoint, accessKey, secretKey := startObjectStorage(t)
	bucket := "koroku-it-" + randomHex(t, 4)
	if out, err := exec.Command("curl", "--fail", "--silent", "--show-error", "--request", "PUT",
		"--aws-sigv4", "aws:amz:us-east-1:s3", "--user", accessKey+":"+secretKey,
		endpoint+"/"+bucket).CombinedOutput(); err != nil {
		t.Fatalf("create the bucket: %v: %s", err, out)
	}
	env := startServe(t, endpoint, accessKey, secretKey, bucket)

	repo := t.TempDir()
	if err := os.WriteFile(repo+"/compose.yaml", []byte(dumpRestoreApp), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch", "main"},
		{"config", "uploadpack.allowAnySHA1InWant", "true"},
		{"add", "--all"},
		{"commit", "--quiet", "-m", "PostgreSQL app"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	slug := "it-restore-" + randomHex(t, 4)
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", "koroku-"+slug, "down", "--volumes", "--remove-orphans", "--timeout", "1").Run()
	})
	const dump = `pg_dump -Fc -Z0 -U "$POSTGRES_USER" "$POSTGRES_DB"`
	// The data volume is excluded because volume directories are only
	// readable by root, which the test is not.
	if status := env.call(http.MethodPost, "/api/apps", map[string]any{
		"slug": slug, "repo_url": "file://" + repo, "web_port": 8080,
		"dump_hook_service": "db", "dump_hook_command": dump,
		"load_hook_service": "db", "load_hook_command": `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"`,
		"dump_interval_hours": 1, "backup_exclude_volumes": []string{"pgdata"},
	}, nil); status != http.StatusCreated {
		t.Fatalf("create app: %d", status)
	}
	vars := []map[string]string{
		{"key": "POSTGRES_USER", "value": "app"}, {"key": "POSTGRES_DB", "value": "app"},
		{"key": "POSTGRES_PASSWORD", "value": "restore-test-password-1"},
	}
	if status := env.call(http.MethodPut, "/api/apps/"+slug+"/env", map[string]any{"vars": vars}, nil); status != http.StatusOK {
		t.Fatalf("set env: %d", status)
	}
	var deployment struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if status := env.call(http.MethodPost, "/api/apps/"+slug+"/deployments", map[string]string{"ref": "main"}, &deployment); status != http.StatusAccepted {
		t.Fatalf("deploy: %d", status)
	}
	waitFor(t, 5*time.Minute, "the deployment", func() bool {
		env.call(http.MethodGet, fmt.Sprintf("/api/deployments/%d", deployment.ID), nil, &deployment)
		return deployment.Status == "succeeded" || deployment.Status == "failed"
	})
	if deployment.Status != "succeeded" {
		t.Fatalf("deployment %s: %s", deployment.Status, deployment.Error)
	}
	psql := func(query string) string {
		t.Helper()
		out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "exec", "-T", "db",
			"psql", "-U", "app", "-d", "app", "-tA", "-c", query).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	backup := func() {
		t.Helper()
		if status := env.call(http.MethodPost, "/api/backups", nil, nil); status != http.StatusAccepted {
			t.Fatalf("start backup: %d", status)
		}
		waitFor(t, 3*time.Minute, "the backup", func() bool {
			var st struct {
				Running bool `json:"running"`
			}
			env.call(http.MethodGet, "/api/backups", nil, &st)
			return !st.Running
		})
	}
	type snapshot struct {
		ID, Kind, Target string
		Time             time.Time
	}
	dumps := func() []snapshot {
		t.Helper()
		var out struct {
			Snapshots []snapshot `json:"snapshots"`
		}
		if status := env.call(http.MethodGet, "/api/apps/"+slug+"/backups", nil, &out); status != http.StatusOK {
			t.Fatalf("snapshots: %d", status)
		}
		var list []snapshot
		for _, s := range out.Snapshots {
			if s.Kind == "dump" {
				list = append(list, s)
			}
		}
		return list
	}
	type restore struct {
		ID               int64  `json:"id"`
		Kind             string `json:"kind"`
		Status           string `json:"status"`
		Error            string `json:"error"`
		BackupSnapshotID string `json:"backup_snapshot_id"`
	}
	restoreDump := func(id string) restore {
		t.Helper()
		var r restore
		if status := env.call(http.MethodPost, "/api/apps/"+slug+"/db-restores",
			map[string]any{"snapshot_id": id, "confirm": slug}, &r); status != http.StatusAccepted {
			t.Fatalf("start restore: %d", status)
		}
		waitFor(t, 3*time.Minute, "the restore", func() bool {
			env.call(http.MethodGet, fmt.Sprintf("/api/db-copies/%d", r.ID), nil, &r)
			return r.Status != "running"
		})
		return r
	}

	psql("CREATE TABLE posts (id int PRIMARY KEY, title text); INSERT INTO posts VALUES (1, 'first'), (2, 'second')")
	backup()
	before := dumps()
	if len(before) != 1 {
		t.Fatalf("dumps = %+v", before)
	}

	psql("DELETE FROM posts WHERE id = 2")
	r := restoreDump(before[0].ID)
	if r.Status != "succeeded" || r.Kind != "restore" || r.BackupSnapshotID == "" {
		t.Fatalf("restore = %+v", r)
	}
	if got := psql("SELECT string_agg(title, ',' ORDER BY id) FROM posts"); got != "first,second" {
		t.Errorf("rows after the restore = %q", got)
	}
	after := dumps()
	if len(after) != 2 || !strings.HasPrefix(r.BackupSnapshotID, after[0].ID) {
		t.Errorf("the dump taken before the restore is not listed: %+v, %s", after, r.BackupSnapshotID)
	}

	// A dump the load hook cannot read: the data before goes back in and
	// the app runs again.
	if status := env.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"dump_hook_command": "echo not-a-dump"}, nil); status != http.StatusOK {
		t.Fatalf("patch: %d", status)
	}
	backup()
	broken := dumps()[0]
	if status := env.call(http.MethodPatch, "/api/apps/"+slug, map[string]any{"dump_hook_command": dump}, nil); status != http.StatusOK {
		t.Fatalf("patch: %d", status)
	}
	psql("INSERT INTO posts VALUES (3, 'third')")
	failed := restoreDump(broken.ID)
	if failed.Status != "failed" {
		t.Fatalf("restore of a broken dump = %+v", failed)
	}
	if got := psql("SELECT string_agg(title, ',' ORDER BY id) FROM posts"); got != "first,second,third" {
		t.Errorf("rows after the failed restore = %q", got)
	}
	out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "ps", "--status", "running", "--format", "{{.Service}}").Output()
	if err != nil || !strings.Contains(string(out), "web") {
		t.Errorf("web does not run after the failed restore: %q, %v", out, err)
	}
}
