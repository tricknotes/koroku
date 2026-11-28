package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// withSystem adds a system backup whose database copy and configuration
// directory live in the test's temporary directory.
func withSystem(t *testing.T, dir string) func(*Options) {
	t.Helper()
	config := filepath.Join(dir, "etc-koroku")
	writeFiles(t, config, map[string]string{"config.toml": "base_domain = \"example.com\"\n", "keys/app.pem": "pem"})
	return func(o *Options) {
		o.WorkDir = filepath.Join(dir, "work")
		o.System = &System{
			ConfigDir: config,
			SnapshotDB: func(_ context.Context, dest string) error {
				return os.WriteFile(dest, []byte("sqlite"), 0o600)
			},
		}
	}
}

// orderDump puts the two commands of a dump, which start concurrently, into
// a fixed order: the hook first.
func orderDump(calls []string) []string {
	for i := 0; i+1 < len(calls); i++ {
		if strings.Contains(calls[i], "--stdin ") && strings.HasPrefix(calls[i+1], "docker compose") {
			calls[i], calls[i+1] = calls[i+1], calls[i]
		}
	}
	return calls
}

func TestRunAllBacksUpSystemVolumesAndDump(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, withSystem(t, dir))
	app := App{Slug: "blog", Project: "koroku-blog", ExcludeVolumes: []string{"pgdata"}, DumpService: "db", DumpCommand: "pg_dump -U postgres app"}
	e.apps = []App{app, {Slug: "shop", Project: "koroku-shop"}}
	e.volume(app, "pgdata", nil)
	uploads := e.volume(app, "uploads", map[string]string{"a.txt": "a"})

	stage := filepath.Join(dir, "work", "system")
	var staged map[string]string
	var dumped string
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		switch {
		case cmd.Dir == stage:
			staged = readTree(t, stage)
		case slices.Contains(cmd.Args, "--stdin"):
			data, err := readStdin(ctx, cmd.Stdin)
			dumped = string(data)
			return summaryLine(e.host.newID(), len(data)), err
		}
		return e.host.backup(ctx, cmd)
	})
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) { return "-- dump\nCREATE TABLE posts;\n", nil })

	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	assertLines(t, "commands", orderDump(e.host.calls()), []string{
		"restic cat config",
		"restic unlock",
		"restic backup --json --quiet --host koroku --tag system --tag kind:system --retry-lock 1m0s .",
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"docker volume inspect -- koroku-blog_pgdata koroku-blog_uploads",
		"restic backup --json --quiet --host koroku --tag app:blog --tag kind:volume --tag target:uploads --retry-lock 1m0s .",
		"docker compose -p koroku-blog exec -T db sh -c pg_dump -U postgres app",
		"restic backup --json --quiet --host koroku --tag app:blog --tag kind:dump --tag target:db --retry-lock 1m0s --stdin --stdin-filename blog-db.dump",
		"docker volume ls --filter label=com.docker.compose.project=koroku-shop --format {{.Name}}",
	})
	e.assertCleanCommands()

	// The hook's command line is a single argument of `sh -c`.
	for _, c := range e.host.run.Calls() {
		if commandKey(c) == "docker compose" && !slices.Equal(c.Args, []string{"compose", "-p", "koroku-blog", "exec", "-T", "db", "sh", "-c", "pg_dump -U postgres app"}) {
			t.Errorf("dump hook argv = %q", c.Args)
		}
	}
	// restic runs inside what it backs up.
	var dirs []string
	for _, c := range e.host.run.Calls() {
		if commandKey(c) == "restic backup" {
			dirs = append(dirs, c.Dir)
		}
	}
	slices.Sort(dirs)
	want := []string{"", stage, uploads}
	slices.Sort(want)
	assertLines(t, "working directories of restic backup", dirs, want)

	if dumped != "-- dump\nCREATE TABLE posts;\n" {
		t.Errorf("restic read %q from the dump hook", dumped)
	}
	wantStaged := map[string]string{"koroku.db": "sqlite", "config/config.toml": "base_domain = \"example.com\"\n", "config/keys/app.pem": "pem"}
	if fmt.Sprint(staged) != fmt.Sprint(wantStaged) {
		t.Errorf("staged system files = %v, want %v", staged, wantStaged)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging directory was not removed: %v", err)
	}

	if res.Status != StatusSucceeded || res.Error != "" || res.ID != 1 || res.Trigger != TriggerManual {
		t.Errorf("result = %+v", res)
	}
	var labels []string
	for _, tr := range res.Targets {
		if tr.Err != nil || tr.SnapshotID == "" {
			t.Errorf("target %s: err=%v snapshot=%q", tr.label(), tr.Err, tr.SnapshotID)
		}
		labels = append(labels, tr.label())
	}
	assertLines(t, "targets", labels, []string{"system", "blog volume uploads", "blog dump db"})
	assertLines(t, "recorded runs", e.rec.finishedRuns(), []string{"1 succeeded"})
	if st := e.m.State(); st.Busy {
		t.Errorf("state after the run = %+v", st)
	}
}

func TestRepositoryIsInitializedOnce(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.host.set("restic cat", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, "Fatal: repository does not exist\n")
		return "", exit("restic", exitNoRepository)
	})
	for range 2 {
		if _, err := e.m.RunAll(context.Background(), TriggerManual); err != nil {
			t.Fatalf("RunAll: %v", err)
		}
	}
	assertLines(t, "repository commands", e.host.calls("restic cat", "restic init", "restic unlock"), []string{
		"restic cat config",
		"restic init",
		"restic unlock",
		"restic unlock",
	})
	e.assertCleanCommands()
}

func TestRepositoryErrorFailsTheRunWithoutInit(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.host.set("restic cat", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, `{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}`+"\n")
		return "", exit("restic", 12)
	})
	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	if want := "open the repository: restic cat: exit status 12: Fatal: wrong password or no key found"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
	assertLines(t, "commands", e.host.calls(), []string{"restic cat config"})
	assertLines(t, "recorded runs", e.rec.finishedRuns(), []string{"1 failed " + res.Error})
	if len(e.rec.targets) != 1 || len(e.rec.targets[0]) != 0 {
		t.Errorf("a run that backed up nothing records no targets: %+v", e.rec.targets)
	}
}

func TestRunContinuesPastAFailingApp(t *testing.T) {
	e := newEnv(t)
	shop := App{Slug: "shop", Project: "koroku-shop"}
	e.apps = []App{blog, shop}
	e.volume(blog, "uploads", nil)
	e.volume(shop, "files", nil)
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if slices.Contains(cmd.Args, "app:blog") {
			// restic may echo anything; credentials must not get through.
			fmt.Fprintf(cmd.Stderr, "Fatal: unable to open repository: access denied for %s / %s\n", testAccessKey, testSecretKey)
			return "", exit("restic", 1)
		}
		return e.host.backup(ctx, cmd)
	})

	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v, want ErrRunFailed", err)
	}
	if len(res.Targets) != 2 || res.Targets[0].Err == nil || res.Targets[1].Err != nil || res.Targets[1].SnapshotID == "" {
		t.Fatalf("targets = %+v", res.Targets)
	}
	want := "blog volume uploads: restic backup: exit status 1: Fatal: unable to open repository: access denied for [MASKED] / [MASKED]"
	if res.Status != StatusFailed || res.Error != want {
		t.Errorf("status %s, error %q, want %q", res.Status, res.Error, want)
	}
	assertLines(t, "recorded runs", e.rec.finishedRuns(), []string{"1 failed " + want})
	// Each app's outcome is recorded on its own, masked like the summary.
	records := e.rec.targets[0]
	if len(records) != 2 ||
		records[0].App != "blog" || records[0].Kind != KindVolume || records[0].Target != "uploads" || records[0].Status != TargetFailed ||
		records[0].Error != "restic backup: exit status 1: Fatal: unable to open repository: access denied for [MASKED] / [MASKED]" ||
		records[1].App != "shop" || records[1].Status != TargetSucceeded || records[1].Error != "" {
		t.Errorf("records = %+v", records)
	}
	for what, text := range map[string]string{"returned error": err.Error(), "target error": res.Targets[0].Err.Error(), "recorded error": strings.Join(e.rec.finishedRuns(), "\n")} {
		assertNoSecrets(t, what, text)
	}
	e.assertCleanCommands()
	if !strings.Contains(e.logs.String(), "[MASKED]") {
		t.Errorf("the failure was not logged: %s", e.logs.String())
	}
}

func TestVolumeWithUnreadableFilesIsStoredWithAWarning(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.volume(blog, "uploads", nil)
	id := e.host.newID()
	e.host.set("restic backup", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, `{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read"}`+"\n")
		return `{"message_type":"error","error":{"message":"open tmp/x: no such file or directory"},"during":"archival","item":"/tmp/x"}` + "\n" + summaryLine(id, 10), exit("restic", exitIncomplete)
	})
	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	got := res.Targets[0]
	if got.Err != nil || got.SnapshotID != id || got.Warning != "some files could not be read: open tmp/x: no such file or directory" {
		t.Errorf("target = %+v", got)
	}
	if res.Status != StatusSucceeded {
		t.Errorf("status = %s", res.Status)
	}
	if r := e.rec.targets[0]; len(r) != 1 || r[0].Status != TargetWarning || r[0].Error != got.Warning {
		t.Errorf("records = %+v", r)
	}
}

func TestVolumeListingFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{{Slug: "blog", Project: "koroku-blog", DumpService: "db", DumpCommand: "pg_dumpall"}}
	e.host.set("docker volume ls", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, "Cannot connect to the Docker daemon\n")
		return "", exit("docker", 1)
	})
	res, err := e.m.RunAll(context.Background(), TriggerManual)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v", err)
	}
	if want := "blog volumes: list volumes: docker volume ls: exit status 1: Cannot connect to the Docker daemon"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
	// The dump does not depend on the volumes.
	if len(res.Targets) != 2 || res.Targets[1].Kind != KindDump || res.Targets[1].Err != nil {
		t.Errorf("targets = %+v", res.Targets)
	}
}

func TestOnlyOneOperationAtATime(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.volume(blog, "uploads", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	enter := sync.OnceFunc(func() { close(entered) })
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		enter()
		<-release
		return e.host.backup(ctx, cmd)
	})

	run, err := e.m.Start(context.Background(), TriggerManual)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.ID != 1 || run.Trigger != TriggerManual || !run.StartedAt.Equal(testNow) {
		t.Errorf("run = %+v", run)
	}
	<-entered
	if st := e.m.State(); !st.Running() || st.RunID != 1 || st.Trigger != TriggerManual || !st.StartedAt.Equal(testNow) {
		t.Errorf("state during the run = %+v", st)
	}

	attempts := map[string]func() error{
		"Start":     func() error { _, err := e.m.Start(context.Background(), TriggerManual); return err },
		"RunAll":    func() error { _, err := e.m.RunAll(context.Background(), TriggerScheduled); return err },
		"BackupApp": func() error { _, err := e.m.BackupApp(context.Background(), blog); return err },
		"RestoreVolume": func() error {
			_, err := e.m.RestoreVolume(context.Background(), blog, "0123abcd")
			return err
		},
		"Forget": func() error { return e.m.Forget(context.Background()) },
		"Check":  func() error { return e.m.Check(context.Background()) },
	}
	for name, attempt := range attempts {
		err := attempt()
		var busy *BusyError
		if !errors.Is(err, ErrBusy) || !errors.As(err, &busy) || busy.State.Operation != OpRun || busy.State.RunID != 1 {
			t.Errorf("%s while a run is in progress: err = %v", name, err)
		}
	}
	if got := e.rec.startedRuns(); len(got) != 1 {
		t.Errorf("refused runs were recorded: %v", got)
	}
	if len(e.ctl.log()) != 0 {
		t.Errorf("a refused restore touched the app: %v", e.ctl.log())
	}

	close(release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	select {
	case <-run.Done():
	default:
		t.Error("Done is not closed after Wait")
	}
	if st := e.m.State(); st.Busy || st.Running() {
		t.Errorf("state after the run = %+v", st)
	}
	if _, err := e.m.RunAll(context.Background(), TriggerManual); err != nil {
		t.Errorf("a run after the first one finished: %v", err)
	}
}

func TestBackupAppIsRecordedAsPreDeploy(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, withSystem(t, dir))
	e.apps = []App{{Slug: "other", Project: "koroku-other"}}
	e.volume(blog, "uploads", nil)

	res, err := e.m.BackupApp(context.Background(), blog)
	if err != nil {
		t.Fatalf("BackupApp: %v", err)
	}
	// Only this app: no system backup, no other app, no retention.
	assertLines(t, "commands", e.host.calls(), []string{
		"restic cat config",
		"restic unlock",
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"docker volume inspect -- koroku-blog_uploads",
		"restic backup --json --quiet --host koroku --tag app:blog --tag kind:volume --tag target:uploads --retry-lock 1m0s .",
	})
	if res.Trigger != TriggerPreDeploy || len(res.Targets) != 1 {
		t.Errorf("result = %+v", res)
	}
	if got := e.rec.startedRuns(); !slices.Equal(got, []Trigger{TriggerPreDeploy}) {
		t.Errorf("recorded triggers = %v", got)
	}

	e.host.set("restic backup", func(context.Context, runner.Cmd) (string, error) { return "", exit("restic", 1) })
	if _, err := e.m.BackupApp(context.Background(), blog); !errors.Is(err, ErrRunFailed) {
		t.Errorf("a failed snapshot must fail BackupApp: %v", err)
	}
}

func TestScheduledRunAppliesRetention(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	if _, err := e.m.RunAll(context.Background(), TriggerScheduled); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	assertLines(t, "commands", e.host.calls(), []string{
		"restic cat config",
		"restic unlock",
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"restic unlock",
		"restic forget --tag kind:pitr-base --tag kind:wal --group-by tags --keep-within 192h --retry-lock 1m0s",
		"restic forget --group-by tags --keep-within 48h --keep-tag kind:pitr-base --keep-tag kind:wal --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune --retry-lock 1m0s",
	})
	e.assertCleanCommands()

	e.host.set("restic forget", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, "Fatal: unable to create lock\n")
		return "", exit("restic", exitLocked)
	})
	res, err := e.m.RunAll(context.Background(), TriggerScheduled)
	if !errors.Is(err, ErrRunFailed) || res.ForgetErr == nil {
		t.Fatalf("err = %v, result = %+v", err, res)
	}
	if want := "retention: restic forget: exit status 11: Fatal: unable to create lock"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
}

func TestStandaloneForgetAndCheck(t *testing.T) {
	e := newEnv(t)
	if err := e.m.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertLines(t, "commands", e.host.calls(), []string{
		"restic cat config",
		"restic unlock",
		"restic forget --tag kind:pitr-base --tag kind:wal --group-by tags --keep-within 192h --retry-lock 1m0s",
		"restic forget --group-by tags --keep-within 48h --keep-tag kind:pitr-base --keep-tag kind:wal --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune --retry-lock 1m0s",
		"restic unlock",
		"restic check --retry-lock 1m0s",
	})
	e.assertCleanCommands()
}

func TestRecorderFailureStopsTheRunBeforeItStarts(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.rec.startErr = errors.New("database is locked")
	if _, err := e.m.RunAll(context.Background(), TriggerManual); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("err = %v", err)
	}
	if len(e.host.calls()) != 0 {
		t.Errorf("commands ran: %v", e.host.calls())
	}
	if e.m.State().Busy {
		t.Error("the Manager stayed busy")
	}
}

func TestCanceledRunIsStillRecorded(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.volume(blog, "uploads", nil)
	ctx, cancel := context.WithCancel(context.Background())
	e.host.set("restic backup", func(ctx context.Context, _ runner.Cmd) (string, error) {
		cancel()
		<-ctx.Done()
		return "", ctx.Err()
	})
	res, err := e.m.RunAll(ctx, TriggerScheduled)
	if !errors.Is(err, ErrRunFailed) || res.Status != StatusFailed {
		t.Fatalf("err = %v, status = %s", err, res.Status)
	}
	if got := e.rec.finishedRuns(); len(got) != 1 || !strings.HasPrefix(got[0], "1 failed blog volume uploads: restic backup: context canceled") {
		t.Errorf("recorded runs = %q", got)
	}
	// No maintenance after a shutdown request.
	if calls := e.host.calls("restic forget"); len(calls) != 0 {
		t.Errorf("forget ran after cancellation: %v", calls)
	}
}

func TestLocalRepositoryEnvironment(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Repository = Repository{Path: "/srv/restic-repo", PasswordFile: "/etc/koroku/restic.key"}
		o.Timeouts.LockWait = 5 * time.Minute
		o.Keep = Retention{Daily: 1, Weekly: 2, Monthly: 3}
		o.Host = "testhost"
	})
	e.apps = []App{blog}
	e.volume(blog, "uploads", nil)
	if _, err := e.m.RunAll(context.Background(), TriggerScheduled); err != nil {
		t.Fatal(err)
	}
	want := []string{"RESTIC_REPOSITORY=/srv/restic-repo", "RESTIC_PASSWORD_FILE=/etc/koroku/restic.key"}
	for _, c := range e.host.run.Calls() {
		if c.Name == "restic" && !slices.Equal(c.Env, want) {
			t.Errorf("%s: environment = %q, want %q", c, c.Env, want)
		}
	}
	assertLines(t, "commands", e.host.calls("restic backup", "restic forget"), []string{
		"restic backup --json --quiet --host testhost --tag app:blog --tag kind:volume --tag target:uploads --retry-lock 5m0s .",
		"restic forget --tag kind:pitr-base --tag kind:wal --group-by tags --keep-within 192h --retry-lock 5m0s",
		"restic forget --group-by tags --keep-tag kind:pitr-base --keep-tag kind:wal --keep-daily 1 --keep-weekly 2 --keep-monthly 3 --prune --retry-lock 5m0s",
	})
}

func TestNewValidatesOptions(t *testing.T) {
	valid := Options{Runner: &runner.Fake{}, Repository: Repository{Path: "/srv/repo", PasswordFile: "/etc/koroku/restic.key"}}
	if _, err := New(valid); err != nil {
		t.Fatalf("valid options: %v", err)
	}
	cases := map[string]func(*Options){
		"no runner":        func(o *Options) { o.Runner = nil },
		"no password file": func(o *Options) { o.Repository.PasswordFile = "" },
		"relative path":    func(o *Options) { o.Repository.Path = "repo" },
		"no location":      func(o *Options) { o.Repository.Path = "" },
		"no credentials": func(o *Options) {
			o.Repository = Repository{Endpoint: "https://s3.example.com", Bucket: "b", PasswordFile: "/k"}
		},
		"system without work dir": func(o *Options) {
			o.System = &System{SnapshotDB: func(context.Context, string) error { return nil }}
		},
		"host with a comma": func(o *Options) { o.Host = "a,b" },
	}
	for name, mutate := range cases {
		opts := valid
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New accepted the options", name)
		}
	}
}

func TestAppNamesAreValidated(t *testing.T) {
	e := newEnv(t)
	for _, app := range []App{
		{Slug: "", Project: "koroku-x"},
		{Slug: "a,b", Project: "koroku-x"},
		{Slug: "blog", Project: ""},
		{Slug: "blog", Project: "koroku-blog", DumpService: "db"},
		{Slug: "blog", Project: "koroku-blog", DumpService: "--help", DumpCommand: "x"},
	} {
		if _, err := e.m.BackupApp(context.Background(), app); err == nil || errors.Is(err, ErrRunFailed) {
			t.Errorf("BackupApp(%+v) = %v, want a validation error", app, err)
		}
	}
	if len(e.host.calls()) != 0 {
		t.Errorf("commands ran for invalid apps: %v", e.host.calls())
	}
}
