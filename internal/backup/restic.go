package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
)

// Repository locates the restic repository and holds what is needed to open
// it. Set either Path (a local directory; tests) or Endpoint and Bucket (any
// S3-compatible storage).
type Repository struct {
	// Endpoint is the S3 endpoint, for example https://s3.example.com.
	Endpoint string
	Bucket   string
	// Prefix is the directory inside the bucket. Default "restic".
	Prefix string
	// Region is passed as AWS_DEFAULT_REGION when set.
	Region          string
	AccessKeyID     string
	SecretAccessKey string

	// Path is a local repository directory. It wins over Endpoint.
	Path string

	// PasswordFile holds the repository password. Only its path is used;
	// restic reads the file.
	PasswordFile string
	// CacheDir is restic's local cache. Empty leaves restic's default,
	// which needs HOME or XDG_CACHE_HOME in the service environment.
	CacheDir string
}

// Location returns the repository in restic's notation.
func (r Repository) Location() string {
	if r.Path != "" {
		return r.Path
	}
	prefix := strings.Trim(r.Prefix, "/")
	if prefix == "" {
		prefix = "restic"
	}
	return "s3:" + strings.TrimRight(r.Endpoint, "/") + "/" + r.Bucket + "/" + prefix
}

func (r Repository) validate() error {
	if r.PasswordFile == "" {
		return errors.New("backup: Repository.PasswordFile is required")
	}
	if r.Path != "" {
		if !filepath.IsAbs(r.Path) {
			return errors.New("backup: Repository.Path must be absolute")
		}
		return nil
	}
	if r.Endpoint == "" || r.Bucket == "" {
		return errors.New("backup: Repository needs either Path or Endpoint and Bucket")
	}
	if r.AccessKeyID == "" || r.SecretAccessKey == "" {
		return errors.New("backup: Repository needs AccessKeyID and SecretAccessKey for S3 storage")
	}
	return nil
}

// env is the environment added to every restic command. Nothing in here is
// ever put into arguments, errors or logs.
func (r Repository) env() []string {
	env := []string{
		"RESTIC_REPOSITORY=" + r.Location(),
		"RESTIC_PASSWORD_FILE=" + r.PasswordFile,
	}
	if r.Path == "" {
		env = append(env,
			"AWS_ACCESS_KEY_ID="+r.AccessKeyID,
			"AWS_SECRET_ACCESS_KEY="+r.SecretAccessKey,
		)
		if r.Region != "" {
			env = append(env, "AWS_DEFAULT_REGION="+r.Region)
		}
	}
	if r.CacheDir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+r.CacheDir)
	}
	return env
}

// restic exit statuses the package reacts to.
const (
	exitIncomplete   = 3  // backup: a snapshot was written but some input could not be read
	exitNoRepository = 10 // the repository does not exist
	exitLocked       = 11 // the repository is locked
)

// commandError is a failed external command with the last lines it printed,
// already masked.
type commandError struct {
	Command string
	Err     error
	Detail  string
}

func (e *commandError) Error() string {
	msg := e.Command + ": "
	var exit *runner.ExitError
	if errors.As(e.Err, &exit) {
		msg += fmt.Sprintf("exit status %d", exit.Code)
	} else {
		msg += e.Err.Error()
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *commandError) Unwrap() error { return e.Err }

func exitCode(err error) int {
	var exit *runner.ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	return 0
}

// tail keeps the last lines of a command's error output. It sits behind a
// secrets.LineWriter, so every Write is one whole, masked line.
type tail struct {
	mu    sync.Mutex
	lines []string
}

const (
	tailLines = 5
	tailBytes = 600
)

func (t *tail) Write(p []byte) (int, error) {
	line := resticMessage(strings.TrimSpace(string(p)))
	if line == "" {
		return len(p), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > tailLines {
		t.lines = t.lines[len(t.lines)-tailLines:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clip(strings.Join(t.lines, "; "), tailBytes)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Keep the end: the last lines say why a command failed.
	s = s[len(s)-n:]
	for len(s) > 0 && s[0]&0xC0 == 0x80 { // do not start inside a UTF-8 sequence
		s = s[1:]
	}
	return "..." + s
}

// resticMessage turns one line of restic output into text. With --json
// restic reports errors as JSON objects, also on standard error.
func resticMessage(line string) string {
	if !strings.HasPrefix(line, "{") {
		return line
	}
	var msg struct {
		Type    string `json:"message_type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return line
	}
	text := ""
	switch msg.Type {
	case "exit_error":
		text = msg.Message
	case "error":
		text = msg.Error.Message
	case "":
		return line
	}
	return strings.Join(strings.Fields(text), " ")
}

// resticCall is one invocation of restic.
type resticCall struct {
	// args start with the subcommand.
	args []string
	dir  string
	// stdin is passed as is. An *os.File reaches restic directly.
	stdin io.Reader
	// stdout receives the output. When nil the output is discarded, except
	// that JSON error messages in it are kept for the error.
	stdout  io.Writer
	timeout time.Duration
}

// restic runs one restic command with the repository environment. Standard
// error is masked and its last lines become part of the returned error.
func (m *Manager) restic(ctx context.Context, c resticCall) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	errTail := &tail{}
	stderr := secrets.NewLineWriter(errTail, m.masker)
	cmd := runner.Cmd{
		Name:   "restic",
		Args:   c.args,
		Dir:    c.dir,
		Env:    m.opts.Repository.env(),
		Stdin:  c.stdin,
		Stdout: c.stdout,
		Stderr: stderr,
	}
	var stdout *secrets.LineWriter
	if c.stdout == nil {
		stdout = secrets.NewLineWriter(jsonErrors{errTail}, m.masker)
		cmd.Stdout = stdout
	}
	err := m.opts.Runner.Run(ctx, cmd)
	_ = stderr.Flush()
	if stdout != nil {
		_ = stdout.Flush()
	}
	if err != nil {
		return &commandError{Command: "restic " + c.args[0], Err: err, Detail: errTail.String()}
	}
	return nil
}

// jsonErrors forwards only restic's JSON error messages to a tail and drops
// every other line of standard output.
type jsonErrors struct{ t *tail }

func (j jsonErrors) Write(p []byte) (int, error) {
	if bytes.HasPrefix(bytes.TrimSpace(p), []byte(`{"message_type":"e`)) {
		return j.t.Write(p)
	}
	return len(p), nil
}

func (m *Manager) lockWait() string {
	return m.opts.Timeouts.LockWait.Round(time.Second).String()
}

// ensureRepository checks that the repository can be opened. With create it
// initializes a repository that does not exist yet; restores never do. The
// answer is remembered for the life of the Manager.
func (m *Manager) ensureRepository(ctx context.Context, create bool) error {
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	if m.repoReady {
		return nil
	}
	err := m.restic(ctx, resticCall{args: []string{"cat", "config"}, stdout: io.Discard, timeout: m.opts.Timeouts.Command})
	if exitCode(err) == exitNoRepository {
		if !create {
			return ErrNoRepository
		}
		m.opts.Logger.Info("initializing the restic repository")
		err = m.restic(ctx, resticCall{args: []string{"init"}, timeout: m.opts.Timeouts.Command})
	}
	if err != nil {
		return err
	}
	m.repoReady = true
	return nil
}

// unlockStale removes locks left behind by restic processes that were killed
// (a timeout, a failed dump, a shutdown in the middle of a backup). restic
// does not do this by itself, and such a lock would block forget, prune and
// check forever. `restic unlock` only removes locks whose process is gone or
// that have not been refreshed for 30 minutes; a running restic keeps its lock.
func (m *Manager) unlockStale(ctx context.Context) error {
	return m.restic(ctx, resticCall{args: []string{"unlock"}, timeout: m.opts.Timeouts.Command})
}

// --- tags ---

const (
	tagSystem     = "system"
	tagAppPrefix  = "app:"
	tagKindPrefix = "kind:"
	// tagTargetPrefix carries the volume name or the dump hook service.
	tagTargetPrefix = "target:"
)

func systemTags() []string {
	return []string{tagSystem, tagKindPrefix + string(KindSystem)}
}

func appTags(slug string, kind Kind, target string) []string {
	return []string{tagAppPrefix + slug, tagKindPrefix + string(kind), tagTargetPrefix + target}
}

func tagArgs(tags []string) []string {
	args := make([]string, 0, 2*len(tags))
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	return args
}

// --- snapshots ---

// Snapshot is one restorable point of an app, as shown in the UI.
type Snapshot struct {
	// ID is restic's short snapshot ID.
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	// Target is the volume name or the dump hook service.
	Target string `json:"target"`
}

// record is a snapshot as restic reports it.
type record struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Paths   []string  `json:"paths"`
	Tags    []string  `json:"tags"`
}

func (r record) tag(prefix string) string {
	for _, t := range r.Tags {
		if v, ok := strings.CutPrefix(t, prefix); ok {
			return v
		}
	}
	return ""
}

func (r record) hasTag(tag string) bool {
	for _, t := range r.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func (r record) kind() Kind     { return Kind(r.tag(tagKindPrefix)) }
func (r record) target() string { return r.tag(tagTargetPrefix) }

func (r record) snapshot() Snapshot {
	id := r.ShortID
	if id == "" && len(r.ID) >= 8 {
		id = r.ID[:8]
	}
	return Snapshot{ID: id, Time: r.Time.UTC(), Kind: r.kind(), Target: r.target()}
}

// records lists the snapshots carrying tag, newest first. A repository that
// does not exist yet has no snapshots.
func (m *Manager) records(ctx context.Context, tag string) ([]record, error) {
	list := func() ([]byte, error) {
		var out bytes.Buffer
		err := m.restic(ctx, resticCall{
			args:    []string{"snapshots", "--json", "--tag", tag, "--retry-lock", m.lockWait()},
			stdout:  &out,
			timeout: m.opts.Timeouts.Command,
		})
		return out.Bytes(), err
	}
	out, err := list()
	if exitCode(err) == exitLocked {
		// Nothing but a stale lock keeps a listing waiting this long.
		if uerr := m.unlockStale(ctx); uerr == nil {
			out, err = list()
		}
	}
	if exitCode(err) == exitNoRepository {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []record
	if err := json.Unmarshal(out, &recs); err != nil {
		return nil, fmt.Errorf("decode restic snapshots: %w", err)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time.After(recs[j].Time) })
	return recs, nil
}

// Snapshots lists the volume and dump snapshots of an app, newest first.
func (m *Manager) Snapshots(ctx context.Context, app App) ([]Snapshot, error) {
	if err := app.validate(); err != nil {
		return nil, err
	}
	recs, err := m.records(ctx, tagAppPrefix+app.Slug)
	if err != nil {
		return nil, fmt.Errorf("list snapshots of %s: %w", app.Slug, err)
	}
	snaps := make([]Snapshot, 0, len(recs))
	for _, r := range recs {
		if k := r.kind(); (k == KindVolume || k == KindDump) && r.target() != "" {
			snaps = append(snaps, r.snapshot())
		}
	}
	return snaps, nil
}

// SystemSnapshots lists the system snapshots, newest first.
func (m *Manager) SystemSnapshots(ctx context.Context) ([]Snapshot, error) {
	recs, err := m.systemRecords(ctx)
	if err != nil {
		return nil, err
	}
	snaps := make([]Snapshot, 0, len(recs))
	for _, r := range recs {
		snaps = append(snaps, r.snapshot())
	}
	return snaps, nil
}

func (m *Manager) systemRecords(ctx context.Context) ([]record, error) {
	recs, err := m.records(ctx, tagKindPrefix+string(KindSystem))
	if err != nil {
		return nil, fmt.Errorf("list system snapshots: %w", err)
	}
	out := recs[:0]
	for _, r := range recs {
		if r.hasTag(tagSystem) {
			out = append(out, r)
		}
	}
	return out, nil
}

// snapshotIDRE accepts short and full snapshot IDs and nothing that restic
// could read as an option or as a keyword such as "latest".
var snapshotIDRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// matchRecord picks the snapshot with the given ID (or ID prefix) from recs.
func matchRecord(recs []record, id string) (record, error) {
	if !snapshotIDRE.MatchString(id) {
		return record{}, fmt.Errorf("%w: malformed ID", ErrSnapshotNotFound)
	}
	var found []record
	for _, r := range recs {
		if strings.HasPrefix(r.ID, id) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return record{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, id)
	case 1:
		return found[0], nil
	default:
		return record{}, fmt.Errorf("%w: %s matches more than one snapshot", ErrSnapshotNotFound, id)
	}
}

// appRecord finds a snapshot of the app and checks its kind. A snapshot of
// another app is not found: only the app's own snapshots are searched.
func (m *Manager) appRecord(ctx context.Context, app App, id string, kind Kind) (record, error) {
	if !snapshotIDRE.MatchString(id) {
		return record{}, fmt.Errorf("%w: malformed ID", ErrSnapshotNotFound)
	}
	recs, err := m.records(ctx, tagAppPrefix+app.Slug)
	if err != nil {
		return record{}, fmt.Errorf("list snapshots of %s: %w", app.Slug, err)
	}
	rec, err := matchRecord(recs, id)
	if err != nil {
		return record{}, err
	}
	if rec.kind() != kind || !nameRE.MatchString(rec.target()) {
		return record{}, fmt.Errorf("%w: %s is not a %s snapshot", ErrSnapshotNotFound, id, kind)
	}
	return rec, nil
}

// --- backup output ---

// backupSummary is the last message of `restic backup --json`.
type backupSummary struct {
	SnapshotID string `json:"snapshot_id"`
	Bytes      uint64 `json:"total_bytes_processed"`
}

// backupReport reads the output of `restic backup --json --quiet`: one
// summary and, for inputs that could not be read, error messages.
type backupReport struct {
	mu       sync.Mutex
	summary  backupSummary
	problems []string
	count    int
}

func (r *backupReport) Write(p []byte) (int, error) {
	line := bytes.TrimSpace(p)
	if !bytes.HasPrefix(line, []byte("{")) {
		return len(p), nil
	}
	var msg struct {
		Type string `json:"message_type"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch msg.Type {
	case "summary":
		_ = json.Unmarshal(line, &r.summary)
	case "error":
		r.count++
		if len(r.problems) < 3 {
			r.problems = append(r.problems, resticMessage(string(line)))
		}
	}
	return len(p), nil
}

func (r *backupReport) snapshotID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.summary.SnapshotID
}

func (r *backupReport) bytes() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.summary.Bytes
}

// problemText describes the inputs restic could not read.
func (r *backupReport) problemText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return ""
	}
	text := strings.Join(r.problems, "; ")
	if r.count > len(r.problems) {
		text += fmt.Sprintf(" (and %d more)", r.count-len(r.problems))
	}
	return clip(text, tailBytes)
}

// backupArgs builds `restic backup` for one snapshot. more is appended last:
// the path or the --stdin options.
func (m *Manager) backupArgs(tags []string, more ...string) []string {
	args := []string{"backup", "--json", "--quiet", "--host", m.opts.Host}
	args = append(args, tagArgs(tags)...)
	args = append(args, "--retry-lock", m.lockWait())
	return append(args, more...)
}

// backupDir stores the contents of dir as one snapshot. restic runs inside
// dir and backs up ".", which puts the contents at the root of the snapshot:
// a restore then needs no knowledge of where the directory was.
func (m *Manager) backupDir(ctx context.Context, dir string, tags []string) (*backupReport, error) {
	report := &backupReport{}
	stdout := secrets.NewLineWriter(report, m.masker)
	err := m.restic(ctx, resticCall{
		args:    m.backupArgs(tags, "."),
		dir:     dir,
		stdout:  stdout,
		timeout: m.opts.Timeouts.Backup,
	})
	_ = stdout.Flush()
	return report, err
}
