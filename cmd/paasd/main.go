// Command paasd is the control plane of the personal PaaS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
	"github.com/tricknotes/koroku/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "paasd:", err)
		os.Exit(1)
	}
}

const usage = `Usage: paasd <command> [flags]

Commands:
  serve     Run the API server and deploy worker
  version   Print the version

Run "paasd <command> -h" for the flags of a command.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, nil)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// serve runs the HTTP server until ctx is canceled. When ready is non-nil it
// receives the bound address once the listener is up (used by tests).
func serve(ctx context.Context, args []string, stderr io.Writer, ready chan<- string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.toml")
	if err := flags.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	key, created, err := secrets.LoadOrCreateKey(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	if created {
		logger.Warn("generated a new master key; back it up, env vars cannot be decrypted without it",
			"path", cfg.MasterKeyFile)
	}
	box, err := secrets.NewBox(key)
	if err != nil {
		return err
	}

	run := runner.Exec{}
	clk := clock.System{}
	prober := system.Host{DataDir: cfg.DataDir, Runner: run}
	fetcher := gitrepo.Fetcher{Runner: run, AllowFileURLs: cfg.Deploy.AllowFileRepos}
	engine := deploy.New(deploy.Options{
		Store:            st,
		Runner:           run,
		Clock:            clk,
		Box:              box,
		System:           prober,
		Fetcher:          fetcher,
		Logger:           logger,
		AppsDir:          cfg.AppsDir(),
		DefaultMemory:    cfg.Deploy.DefaultMemoryLimit,
		DiskLimitPercent: cfg.Deploy.DiskUsageLimitPercent,
		PruneImages:      cfg.Deploy.PruneImages,
	})
	srv := api.New(api.Options{
		Config:   cfg,
		Store:    st,
		System:   prober,
		Web:      web.Dist(),
		Logger:   logger,
		Version:  version,
		Clock:    clk,
		Box:      box,
		Deployer: engine,
		Refs:     deploy.GitRefResolver{Fetcher: fetcher},
	})

	// The worker stops when ctx is canceled; a running deployment is then
	// recorded as interrupted.
	workerDone := make(chan error, 1)
	go func() { workerDone <- engine.Run(ctx) }()

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	logger.Info("paasd listening", "addr", listener.Addr().String(), "version", version)
	if ready != nil {
		ready <- listener.Addr().String()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case err := <-workerDone:
		if err != nil {
			return fmt.Errorf("deploy worker: %w", err)
		}
	case <-ctx.Done():
	}
	logger.Info("paasd shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Log streams stay open for as long as a client listens; do not wait
	// for them beyond the grace period.
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown: %w", err)
	}
	select {
	case <-workerDone:
	case <-time.After(45 * time.Second):
		logger.Warn("the deploy worker did not stop in time")
	}
	return nil
}
