// Package runner is the single place where koroku executes external commands
// (docker, git, restic). Arguments are always passed as a slice; no shell is involved.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd describes one external command invocation.
type Cmd struct {
	// Name is the program to run, resolved through PATH.
	Name string
	// Args are passed to the program verbatim.
	Args []string
	// Dir is the working directory. Empty means the current directory.
	Dir string
	// Env holds extra KEY=VALUE entries added to the inherited environment.
	Env []string
	// CleanEnv limits the inherited environment to the variables a program
	// needs to run (see inherited). For programs that put their environment
	// into files they read on behalf of someone else: Compose interpolates
	// ${VAR} in a repository's Compose file with it.
	CleanEnv bool
	// Stdin is fed to the program when set.
	Stdin io.Reader
	// Stdout and Stderr receive the program output when set.
	Stdout io.Writer
	Stderr io.Writer
}

// String renders the command for logs. Environment values are never included.
func (c Cmd) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Runner executes external commands.
type Runner interface {
	Run(ctx context.Context, cmd Cmd) error
}

// ExitError is returned when a command runs but exits with a non-zero status.
type ExitError struct {
	Cmd  string
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: exit status %d", e.Cmd, e.Code)
}

// Exec runs commands on the host with os/exec.
type Exec struct{}

// Run executes cmd and waits for it to finish.
func (Exec) Run(ctx context.Context, cmd Cmd) error {
	c := exec.CommandContext(ctx, cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	switch {
	case cmd.CleanEnv:
		c.Env = append(inherited(os.Environ()), cmd.Env...)
	case len(cmd.Env) > 0:
		c.Env = append(os.Environ(), cmd.Env...)
	}
	c.Stdin = cmd.Stdin
	c.Stdout = cmd.Stdout
	c.Stderr = cmd.Stderr
	err := c.Run()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", cmd.Name, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Cmd: cmd.Name, Code: exitErr.ExitCode()}
	}
	return fmt.Errorf("%s: %w", cmd.Name, err)
}

// inheritedNames and inheritedPrefixes name what a CleanEnv command keeps
// of koroku's environment: where programs and the user's files are, the
// locale, temporary files, TLS roots, proxies, and the settings of the
// Docker and Compose clients (the daemon to talk to, the client config, the
// build options an operator sets for the whole host).
var (
	inheritedNames = []string{
		"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	}
	inheritedPrefixes = []string{"DOCKER_", "COMPOSE_", "BUILDKIT_", "BUILDX_"}
)

// inherited returns the entries of environ that a CleanEnv command keeps.
func inherited(environ []string) []string {
	var out []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		keep := false
		for _, n := range inheritedNames {
			keep = keep || name == n
		}
		for _, p := range inheritedPrefixes {
			keep = keep || strings.HasPrefix(name, p)
		}
		if keep {
			out = append(out, entry)
		}
	}
	return out
}

// Output runs cmd and returns its standard output. Standard error is attached
// to the returned error (truncated) unless the caller set cmd.Stderr.
func Output(ctx context.Context, r Runner, cmd Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	captureStderr := cmd.Stderr == nil
	if captureStderr {
		cmd.Stderr = &stderr
	}
	if err := r.Run(ctx, cmd); err != nil {
		if captureStderr {
			if msg := lastLines(stderr.String(), 5); msg != "" {
				return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
			}
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}
