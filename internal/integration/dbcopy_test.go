//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// postgresApp is a Compose app with a PostgreSQL database, a worker that is
// stopped while data is loaded, and a web service. The worker uses the
// PostgreSQL image so that it can run psql as the post-load command.
const postgresApp = `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
  worker:
    image: postgres:17.10-alpine
    stop_signal: SIGKILL
    command: ["sleep", "1000000"]
  db:
    image: postgres:17.10-alpine
    volumes:
      - pgdata:/var/lib/postgresql/data
    # Over TCP: on first start the image initialises the database with a
    # temporary server that listens on the socket only, then restarts it.
    # A socket check passes during that window and psql then fails.
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U app -d app"]
      interval: 1s
      timeout: 5s
      retries: 60
volumes:
  pgdata:
`

// psql runs a query in the db service of an app and returns its output.
func psql(t *testing.T, slug, query string) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "exec", "-T", "db",
		"psql", "-U", "app", "-d", "app", "-tA", "-c", query).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q in %s: %v\n%s", query, slug, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runningServices lists the services of an app with a running container.
func runningServices(t *testing.T, slug string) []string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "ps",
		"--status", "running", "--format", "{{.Service}}").Output()
	if err != nil {
		t.Fatalf("docker compose ps: %v", err)
	}
	services := strings.Fields(string(out))
	slices.Sort(services)
	return services
}

type dbCopy struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Bytes  int64  `json:"bytes"`
}

// TestCopyDatabaseIntoStaging copies the PostgreSQL database of a
// production app into a staging app of the same repository: the rows
// arrive, the post-load command masks them before the app is served again,
// and the staging app's other services run again afterwards.
func TestCopyDatabaseIntoStaging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "")
	source.write("compose.yaml", postgresApp)
	sha := source.commit("PostgreSQL app")

	prod, staging := e.slug("dbprod"), e.slug("dbstage")
	vars := map[string]string{
		"POSTGRES_USER": "app", "POSTGRES_DB": "app",
		"POSTGRES_PASSWORD": "copy-test-password-1", "PGPASSWORD": "copy-test-password-1",
	}
	for _, slug := range []string{prod, staging} {
		e.createApp(slug, source.url())
		e.setEnv(slug, vars)
		if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
			t.Fatalf("deploy %s: %s %s\n%s", slug, d.Status, d.Error, e.logText(d.ID))
		}
	}

	psql(t, prod, "CREATE TABLE users (id int PRIMARY KEY, email text); "+
		"INSERT INTO users VALUES (1, 'alice@example.com'), (2, 'bob@example.com')")
	psql(t, staging, "CREATE TABLE users (id int PRIMARY KEY, email text); "+
		"CREATE TABLE leftovers (id int); INSERT INTO users VALUES (9, 'old@example.com')")

	patch := func(slug string, body map[string]any) {
		t.Helper()
		if status := e.call(http.MethodPatch, "/api/apps/"+slug, body, nil); status != http.StatusOK {
			t.Fatalf("patch %s: %d", slug, status)
		}
	}
	patch(prod, map[string]any{
		"environment":       "production",
		"dump_hook_service": "db", "dump_hook_command": `pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"`,
	})
	patch(staging, map[string]any{
		"load_hook_service": "db", "load_hook_command": `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"`,
		"post_load_service": "worker", "post_load_command": `psql -h db -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "UPDATE users SET email = 'user' || id || '@example.invalid'"`,
	})

	// Copying into the production app is refused.
	if status := e.call(http.MethodPost, "/api/apps/"+prod+"/db-copies",
		map[string]any{"source": staging, "confirm": prod}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("copy into production: %d", status)
	}

	var c dbCopy
	if status := e.call(http.MethodPost, "/api/apps/"+staging+"/db-copies",
		map[string]any{"source": prod, "confirm": staging}, &c); status != http.StatusAccepted {
		t.Fatalf("start copy: %d", status)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for c.Status == "running" && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		e.call(http.MethodGet, fmt.Sprintf("/api/db-copies/%d", c.ID), nil, &c)
	}
	log := e.copyLog(c.ID)
	if c.Status != "succeeded" || c.Bytes == 0 {
		t.Fatalf("copy = %+v\n%s", c, log)
	}

	if got := psql(t, staging, "SELECT id || ' ' || email FROM users ORDER BY id"); got != "1 user1@example.invalid\n2 user2@example.invalid" {
		t.Errorf("staging rows = %q\n%s", got, log)
	}
	// The production data is untouched.
	if got := psql(t, prod, "SELECT count(*) FROM users WHERE email LIKE '%@example.com'"); got != "2" {
		t.Errorf("production rows = %q", got)
	}
	if got := runningServices(t, staging); !slices.Equal(got, []string{"db", "web", "worker"}) {
		t.Errorf("running services after the copy = %v\n%s", got, log)
	}
	for _, line := range []string{"Stopped: web, worker", "Started: web, worker", "Backups are not configured"} {
		if !strings.Contains(log, line) {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
	if strings.Contains(log, "copy-test-password-1") {
		t.Errorf("the log leaks a variable:\n%s", log)
	}
}

func (e *env) copyLog(id int64) string {
	e.t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/db-copies/%d/logs?format=text", e.server.URL, id))
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
