package runner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecRunCapturesOutput(t *testing.T) {
	var out bytes.Buffer
	err := Exec{}.Run(context.Background(), Cmd{
		Name:   "sh",
		Args:   []string{"-c", `printf '%s' "$GREETING"`},
		Env:    []string{"GREETING=hello"},
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != "hello" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestExecRunDoesNotInterpretArguments(t *testing.T) {
	out, err := Output(context.Background(), Exec{}, Cmd{Name: "echo", Args: []string{"$(id); rm -rf /tmp/x"}})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "$(id); rm -rf /tmp/x" {
		t.Errorf("argument was altered: %q", got)
	}
}

func TestExecRunExitError(t *testing.T) {
	_, err := Output(context.Background(), Exec{}, Cmd{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitError, got %v", err)
	}
	if exitErr.Code != 3 {
		t.Errorf("Code = %d", exitErr.Code)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should include stderr", err)
	}
}

func TestExecRunMissingBinary(t *testing.T) {
	err := Exec{}.Run(context.Background(), Cmd{Name: "koroku-no-such-binary"})
	if err == nil {
		t.Fatal("expected an error")
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Errorf("a missing binary must not be reported as an exit error: %v", err)
	}
}

func TestExecRunHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Exec{}.Run(ctx, Cmd{Name: "sleep", Args: []string{"5"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("command was not killed promptly")
	}
}

func TestExecRunDir(t *testing.T) {
	dir := t.TempDir()
	out, err := Output(context.Background(), Exec{}, Cmd{Name: "pwd", Dir: dir})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(out)), dir) {
		t.Errorf("pwd = %q, want suffix %q", out, dir)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	fake := &Fake{Handler: func(_ context.Context, cmd Cmd) (string, error) {
		if cmd.Name == "docker" {
			return "29.0.0\n", nil
		}
		return "", &ExitError{Cmd: cmd.Name, Code: 1}
	}}
	out, err := Output(context.Background(), fake, Cmd{Name: "docker", Args: []string{"version"}})
	if err != nil || string(out) != "29.0.0\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if err := fake.Run(context.Background(), Cmd{Name: "git"}); err == nil {
		t.Error("expected the scripted error")
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].String() != "docker version" || calls[1].Name != "git" {
		t.Errorf("calls = %+v", calls)
	}
	fake.Reset()
	if len(fake.Calls()) != 0 {
		t.Error("Reset did not clear calls")
	}
}

func TestInheritedEnvironment(t *testing.T) {
	got := inherited([]string{
		"PATH=/usr/bin", "HOME=/root", "INVOCATION_ID=abc", "GH_TOKEN=secret", "DOCKER_HOST=unix:///x",
		"COMPOSE_BAKE=false", "BUILDKIT_PROGRESS=plain", "HTTPS_PROXY=http://proxy.example.com:3128",
		"PATHOLOGICAL=1", "MASTER_KEY=x", "=odd",
	})
	want := []string{
		"PATH=/usr/bin", "HOME=/root", "DOCKER_HOST=unix:///x", "COMPOSE_BAKE=false",
		"BUILDKIT_PROGRESS=plain", "HTTPS_PROXY=http://proxy.example.com:3128",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("inherited = %v, want %v", got, want)
	}
}

func TestCleanEnvHidesTheRestOfTheEnvironment(t *testing.T) {
	t.Setenv("KOROKU_TEST_SECRET", "hidden")
	t.Setenv("DOCKER_TEST_SETTING", "kept")
	out, err := Output(context.Background(), Exec{}, Cmd{
		Name: "sh", Args: []string{"-c", `echo "${KOROKU_TEST_SECRET:-none} ${DOCKER_TEST_SETTING:-none} ${EXTRA:-none}"`},
		Env: []string{"EXTRA=added"}, CleanEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "none kept added" {
		t.Errorf("environment = %q", got)
	}
}
