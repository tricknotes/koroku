package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// recordingRunner fails the test when koroku run reaches docker.
type recordingRunner struct{ calls []runner.Cmd }

func (r *recordingRunner) Run(_ context.Context, cmd runner.Cmd) error {
	r.calls = append(r.calls, cmd)
	return fmt.Errorf("unexpected command %s", cmd)
}

// runConsoleSetup writes a config whose database holds one app, blog,
// that was never deployed.
func runConsoleSetup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\ndata_dir = %q\nmaster_key_file = %q\n",
		dataDir, filepath.Join(dir, "master.key"))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "koroku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateApp(context.Background(), store.App{
		Slug: "blog", WebService: "web", WebPort: 3000,
	}, 20000, 20999, time.Now()); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestRunConsoleRefusals(t *testing.T) {
	configPath := runConsoleSetup(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no service", []string{"-config", configPath, "blog"}, "an app and a service are required"},
		{"unknown app", []string{"-config", configPath, "nope", "web"}, `no app "nope"`},
		{"not deployed", []string{"-config", configPath, "blog", "web", "bin/rails", "console"}, "no deployed release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{}
			var stderr bytes.Buffer
			err := runConsole(context.Background(), tc.args, consoleIO{Stdout: &stderr, Stderr: &stderr}, r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(r.calls) != 0 {
				t.Errorf("docker ran: %v", r.calls)
			}
		})
	}
}

func TestRunConsoleDoesNotWriteTheDatabase(t *testing.T) {
	configPath := runConsoleSetup(t)
	db := filepath.Join(filepath.Dir(configPath), "data", "koroku.db")
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	_ = runConsole(context.Background(), []string{"-config", configPath, "blog", "web"},
		consoleIO{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, &recordingRunner{})
	after, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("koroku run changed the database")
	}
}
