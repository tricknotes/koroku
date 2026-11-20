package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/preflight"
	"github.com/tricknotes/koroku/internal/runner"
)

// checkTimeout bounds the whole of `koroku check`; restic on a slow link is
// the longest step.
const checkTimeout = 3 * time.Minute

// check implements `koroku check`: it verifies the configuration against the
// host and the external services, and prints what to fix (docs/SETUP.md).
func check(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.toml")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stdout, "%s config      %v\n", label(preflight.Fail), err)
		return errors.New("fix config.toml first (docs/SETUP.md 7)")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	deps := preflight.Deps{Config: cfg}
	if cfg.GitHub.Configured() {
		if gh, err := newGitHubClient(cfg.GitHub); err != nil {
			deps.GitHubErr = err
		} else {
			deps.GitHub = gh
		}
	}
	// The allowed user is public; looking it up needs no App.
	if users, err := github.NewAnonymous("", nil); err == nil {
		deps.Users = users
	}
	if cfg.Storage.Configured() {
		if objects, err := objstore.NewS3(objstore.S3Config{
			Endpoint:        cfg.Storage.Endpoint,
			Region:          cfg.Storage.Region,
			Bucket:          cfg.Storage.Bucket,
			AccessKeyID:     cfg.Storage.AccessKeyID,
			SecretAccessKey: cfg.Storage.SecretAccessKey,
		}); err == nil {
			deps.Objects = objects
		}
	}
	if ok, reason := backupsConfigured(cfg); ok {
		manager, err := backup.New(backup.Options{Repository: backupRepository(cfg), Runner: runner.Exec{}})
		if err != nil {
			deps.BackupsErr = err
		} else {
			deps.Backups = manager
		}
	} else {
		deps.BackupsErr = errors.New(reason)
	}

	results := preflight.Run(ctx, deps)
	failed := printResults(stdout, *configPath, results)
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func label(s preflight.Status) string {
	switch s {
	case preflight.OK:
		return "[ ok ]"
	case preflight.Warn:
		return "[warn]"
	case preflight.Fail:
		return "[FAIL]"
	default:
		return "[skip]"
	}
}

// printResults writes one line per check (and its hint) and returns the
// number of failures.
func printResults(w io.Writer, configPath string, results []preflight.Result) int {
	width := 0
	for _, r := range results {
		width = max(width, len(r.Name))
	}
	fmt.Fprintf(w, "koroku check (%s)\n\n", configPath)
	var failed, warned int
	for _, r := range results {
		fmt.Fprintf(w, "%s %-*s  %s\n", label(r.Status), width, r.Name, r.Message)
		if r.Hint != "" {
			fmt.Fprintf(w, "       %s  -> %s\n", strings.Repeat(" ", width), r.Hint)
		}
		switch r.Status {
		case preflight.Fail:
			failed++
		case preflight.Warn:
			warned++
		}
	}
	fmt.Fprintf(w, "\n%d failed, %d warning(s), %d checks\n", failed, warned, len(results))
	return failed
}
