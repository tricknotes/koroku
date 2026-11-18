package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/runner"
)

// Values that must never show up in arguments, errors or logs.
const (
	testAccessKey = "AKIDEXAMPLE00000001"
	testSecretKey = "secret-access-key-0000000001"
	testPassword  = "repository-password-0000001"
)

var testNow = time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)

// blog is the app most tests use.
var blog = App{Slug: "blog", Project: "paas-blog"}

type handler func(ctx context.Context, cmd runner.Cmd) (string, error)

// host scripts the answers of docker and restic. Commands are matched by
// their first words ("restic backup", "docker volume ls", "docker compose");
// on overrides the default answer of a command.
type host struct {
	t   *testing.T
	run *runner.Fake

	mu sync.Mutex
	// volumes maps a Compose project to short volume name -> mountpoint.
	volumes map[string]map[string]string
	// records is what `restic snapshots` answers, filtered by --tag.
	records []record
	on      map[string]handler
	nextID  int
}

func newHost(t *testing.T) *host {
	t.Helper()
	h := &host{t: t, run: &runner.Fake{}, volumes: map[string]map[string]string{}, on: map[string]handler{}}
	h.run.Handler = h.handle
	return h
}

func commandKey(cmd runner.Cmd) string {
	words := []string{cmd.Name}
	n := 1
	if cmd.Name == "docker" && len(cmd.Args) > 0 && cmd.Args[0] == "volume" {
		n = 2
	}
	return strings.Join(append(words, cmd.Args[:min(n, len(cmd.Args))]...), " ")
}

func (h *host) handle(ctx context.Context, cmd runner.Cmd) (string, error) {
	key := commandKey(cmd)
	h.mu.Lock()
	custom := h.on[key]
	h.mu.Unlock()
	if custom != nil {
		return custom(ctx, cmd)
	}
	switch key {
	case "restic cat", "restic init", "restic unlock", "restic forget", "restic check", "restic restore", "restic dump":
		return "", nil
	case "restic backup":
		return h.backup(ctx, cmd)
	case "restic snapshots":
		return h.snapshots(cmd), nil
	case "docker volume ls":
		return h.volumeLs(cmd), nil
	case "docker volume inspect":
		return h.volumeInspect(cmd), nil
	case "docker compose":
		return "dump of " + cmd.Args[2] + "\n", nil
	}
	h.t.Errorf("unexpected command: %s", cmd)
	return "", fmt.Errorf("unexpected command")
}

func (h *host) set(key string, fn handler) {
	h.mu.Lock()
	h.on[key] = fn
	h.mu.Unlock()
}

// newID returns a fresh 64 digit snapshot ID.
func (h *host) newID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return fmt.Sprintf("%064x", h.nextID)
}

func summaryLine(id string, size int) string {
	return fmt.Sprintf(`{"message_type":"summary","total_bytes_processed":%d,"snapshot_id":"%s"}`+"\n", size, id)
}

// backup answers like `restic backup --json --quiet`. With --stdin it first
// reads the stream to its end, as restic does.
func (h *host) backup(ctx context.Context, cmd runner.Cmd) (string, error) {
	size := 1
	if slices.Contains(cmd.Args, "--stdin") {
		data, err := readStdin(ctx, cmd.Stdin)
		if err != nil {
			return "", err
		}
		size = len(data)
	}
	return summaryLine(h.newID(), size), nil
}

// readStdin reads r to its end unless ctx ends first, which is what killing
// the process looks like to the caller.
func readStdin(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data, err}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *host) snapshots(cmd runner.Cmd) string {
	tag := ""
	if i := slices.Index(cmd.Args, "--tag"); i >= 0 {
		tag = cmd.Args[i+1]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []record{}
	for _, r := range h.records {
		if r.hasTag(tag) {
			out = append(out, r)
		}
	}
	data, _ := json.Marshal(out)
	return string(data)
}

func (h *host) volumeLs(cmd runner.Cmd) string {
	project := strings.TrimPrefix(cmd.Args[3], "label=com.docker.compose.project=")
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for short := range h.volumes[project] {
		names = append(names, project+"_"+short)
	}
	return strings.Join(names, "\n")
}

func (h *host) volumeInspect(cmd runner.Cmd) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	type vol struct {
		Name       string
		Mountpoint string
		Labels     map[string]string
	}
	var out []vol
	for _, name := range cmd.Args[3:] {
		project, short, _ := strings.Cut(name, "_")
		out = append(out, vol{Name: name, Mountpoint: h.volumes[project][short], Labels: map[string]string{
			"com.docker.compose.project": project,
			"com.docker.compose.volume":  short,
		}})
	}
	data, _ := json.Marshal(out)
	return string(data)
}

// addRecord registers a snapshot for `restic snapshots` and returns it.
func (h *host) addRecord(at time.Time, path string, tags ...string) record {
	id := h.newID()
	// Make the IDs differ in their first characters, as real ones do.
	id = id[56:] + id[:56]
	r := record{ID: id, ShortID: id[:8], Time: at, Paths: []string{path}, Tags: tags}
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return r
}

// calls returns the recorded commands whose key is one of keys (all commands
// without keys), rendered as "name args...".
func (h *host) calls(keys ...string) []string {
	var out []string
	for _, c := range h.run.Calls() {
		if len(keys) == 0 || slices.Contains(keys, commandKey(c)) {
			out = append(out, c.String())
		}
	}
	return out
}

func exit(name string, code int) error { return &runner.ExitError{Cmd: name, Code: code} }

// recorder is an in-memory Recorder.
type recorder struct {
	mu       sync.Mutex
	started  []Trigger
	finished []string // "<id> <status> <error>"
	startErr error
}

func (r *recorder) StartRun(_ context.Context, trigger Trigger, _ time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return 0, r.startErr
	}
	r.started = append(r.started, trigger)
	return int64(len(r.started)), nil
}

func (r *recorder) FinishRun(_ context.Context, id int64, status RunStatus, errMsg string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, strings.TrimSpace(fmt.Sprintf("%d %s %s", id, status, errMsg)))
	return nil
}

func (r *recorder) startedRuns() []Trigger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

func (r *recorder) finishedRuns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.finished)
}

// controller is a scripted AppController that records the order of calls.
type controller struct {
	mu       sync.Mutex
	calls    []string
	stopErr  error
	startErr error
}

func (c *controller) Stop(_ context.Context, app App) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "stop "+app.Slug)
	return c.stopErr
}

func (c *controller) Start(_ context.Context, app App) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "start "+app.Slug)
	return c.startErr
}

func (c *controller) log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// syncBuffer is a log destination that is safe to read while a run writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a Manager wired to fakes, with an S3 repository.
type env struct {
	t        *testing.T
	m        *Manager
	host     *host
	rec      *recorder
	ctl      *controller
	clock    *clock.Fake
	logs     *syncBuffer
	dir      string
	password string
	apps     []App
}

func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	e := &env{
		t:     t,
		host:  newHost(t),
		rec:   &recorder{},
		ctl:   &controller{},
		clock: clock.NewFake(testNow),
		logs:  &syncBuffer{},
		dir:   t.TempDir(),
	}
	e.password = filepath.Join(e.dir, "restic.key")
	if err := os.WriteFile(e.password, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Repository: Repository{
			Endpoint:        "https://s3.example.com/",
			Bucket:          "paas-backups",
			Region:          "auto",
			AccessKeyID:     testAccessKey,
			SecretAccessKey: testSecretKey,
			PasswordFile:    e.password,
			CacheDir:        "/var/cache/paas/restic",
		},
		Runner:     e.host.run,
		Clock:      e.clock,
		Logger:     slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Recorder:   e.rec,
		Controller: e.ctl,
		Apps:       func(context.Context) ([]App, error) { return e.apps, nil },
		TrashDir:   filepath.Join(e.dir, "restore-trash"),
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

// wantEnv is the environment every restic command of newEnv must carry.
func (e *env) wantEnv() []string {
	return []string{
		"RESTIC_REPOSITORY=s3:https://s3.example.com/paas-backups/restic",
		"RESTIC_PASSWORD_FILE=" + e.password,
		"AWS_ACCESS_KEY_ID=" + testAccessKey,
		"AWS_SECRET_ACCESS_KEY=" + testSecretKey,
		"AWS_DEFAULT_REGION=auto",
		"RESTIC_CACHE_DIR=/var/cache/paas/restic",
	}
}

// volume creates a directory standing in for a volume's mountpoint.
func (e *env) volume(app App, short string, files map[string]string) string {
	e.t.Helper()
	dir := filepath.Join(e.dir, "volumes", app.Project+"_"+short, "_data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	writeFiles(e.t, dir, files)
	e.host.mu.Lock()
	if e.host.volumes[app.Project] == nil {
		e.host.volumes[app.Project] = map[string]string{}
	}
	e.host.volumes[app.Project][short] = dir
	e.host.mu.Unlock()
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree returns the regular files below dir as relative path -> content.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertNoSecrets fails when a credential or the password shows up in text.
func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range []string{testAccessKey, testSecretKey, testPassword} {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains a secret (%s...): %s", what, secret[:6], text)
		}
	}
}

// assertCleanCommands checks every recorded command: secrets only ever
// travel in the environment of restic, and docker gets no environment.
func (e *env) assertCleanCommands() {
	e.t.Helper()
	for _, c := range e.host.run.Calls() {
		assertNoSecrets(e.t, "argv", c.String())
		assertNoSecrets(e.t, "working directory", c.Dir)
		switch c.Name {
		case "restic":
			if !slices.Equal(c.Env, e.wantEnv()) {
				e.t.Errorf("%s: environment = %q, want %q", c, c.Env, e.wantEnv())
			}
		default:
			if len(c.Env) != 0 {
				e.t.Errorf("%s: unexpected environment %q", c, c.Env)
			}
		}
	}
	assertNoSecrets(e.t, "log", e.logs.String())
}

func assertLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got:\n  %s\nwant:\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
