package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// exitStatus is returned by a command that ends with the exit code of a
// program it ran: main exits with it and prints nothing more.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// consoleIO is the terminal `koroku run` hands to docker.
type consoleIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Terminal reports whether Stdin is a terminal: docker then allocates
	// a TTY, otherwise the input is passed through as is.
	Terminal bool
}

func stdConsoleIO(stdout, stderr io.Writer) consoleIO {
	info, err := os.Stdin.Stat()
	terminal := err == nil && info.Mode()&os.ModeCharDevice != 0
	return consoleIO{Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, Terminal: terminal}
}

const runUsage = `Usage: koroku run [-config path] <app> <service> [command [args...]]

Runs a command (sh when omitted) in a new container of the service, with the
image, environment and memory limit of the app's running release. The
terminal is attached, so interactive programs (rails console, psql) work.
Needs root, like the release files it reads (ARCHITECTURE 9.6).

Flags:
`

// runConsole implements `koroku run` (ARCHITECTURE 9.6).
func runConsole(ctx context.Context, args []string, stdio consoleIO, r runner.Runner) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stdio.Stderr)
	flags.Usage = func() {
		fmt.Fprint(stdio.Stderr, runUsage)
		flags.PrintDefaults()
	}
	configPath := flags.String("config", config.DefaultPath, "path to config.toml")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) < 2 {
		flags.Usage()
		return errors.New("an app and a service are required")
	}
	slug, service, command := rest[0], rest[1], rest[2:]
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	app, err := st.GetApp(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no app %q", slug)
	}
	if err != nil {
		return err
	}

	engine := deploy.New(deploy.Options{Store: st, Runner: r, AppsDir: cfg.AppsDir()})
	cmd, err := engine.ConsoleCmd(ctx, app, service, command, stdio.Terminal)
	var unknown *deploy.UnknownServiceError
	switch {
	case errors.Is(err, deploy.ErrNotDeployed):
		return fmt.Errorf("%s has no deployed release to run in", slug)
	case errors.As(err, &unknown):
		return fmt.Errorf("%s has no service %q (services: %v)", slug, unknown.Service, unknown.Services)
	case err != nil:
		return err
	}
	if app.Environment == store.EnvironmentProduction {
		fmt.Fprintf(stdio.Stderr, "koroku: %s is a production app: the command works on production data.\n", slug)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr

	// Interrupts reach docker from the terminal too, and docker passes them
	// on to the container. koroku must not stop docker before that: the
	// container would be left running. main catches the signals, so they
	// only cancel ctx, which the command does not follow.
	err = r.Run(context.WithoutCancel(ctx), cmd)
	var exitErr *runner.ExitError
	if errors.As(err, &exitErr) {
		return exitStatus(exitErr.Code)
	}
	return err
}
