// Command koroku is the control plane of the personal PaaS.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/domains"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/logs"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/routing"
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
		fmt.Fprintln(os.Stderr, "koroku:", err)
		os.Exit(1)
	}
}

const usage = `Usage: koroku <command> [flags]

Commands:
  serve            Run the API server and deploy worker
  check            Check the host and the external services against config.toml
  restore-system   Restore the database and configuration from a backup (new host)
  version          Print the version

Run "koroku <command> -h" for the flags of a command.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, nil)
	case "check":
		return check(ctx, args[1:], stdout, stderr)
	case "restore-system":
		return restoreSystem(ctx, args[1:], stdout, stderr)
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

// newGitHubClient builds the GitHub App client from the configuration.
func newGitHubClient(cfg config.GitHub) (*github.Client, error) {
	info, err := os.Stat(cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github private key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("github private key %s: permissions %04o are too open, want 0600",
			cfg.PrivateKeyFile, info.Mode().Perm())
	}
	pemBytes, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github private key: %w", err)
	}
	key, err := github.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("github private key %s: %w", cfg.PrivateKeyFile, err)
	}
	return github.New(github.Config{
		AppID:          cfg.AppID,
		PrivateKey:     key,
		ClientID:       cfg.ClientID,
		ClientSecret:   cfg.ClientSecret,
		AccountAllowed: installationAccountFilter(cfg),
	})
}

// installationAccountFilter allows the installations on the operator's own
// account and on the accounts listed in installation_accounts.
func installationAccountFilter(cfg config.GitHub) func(int64, string) bool {
	logins := make(map[string]bool, len(cfg.InstallationAccounts))
	for _, login := range cfg.InstallationAccounts {
		logins[strings.ToLower(strings.TrimSpace(login))] = true
	}
	return func(id int64, login string) bool {
		return id == cfg.AllowedUserID || logins[strings.ToLower(login)]
	}
}

// serve runs the HTTP server until ctx is canceled. When ready is non-nil it
// receives the bound address once the listener is up (used by tests).
func serve(ctx context.Context, args []string, stderr io.Writer, ready chan<- string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath, "path to config.toml")
	insecureNoAuth := flags.Bool("insecure-no-auth", false,
		"disable login and the Origin check (local development only; anyone who can reach the port controls Docker)")
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

	// Routing: the whole Caddy configuration is derived from the database
	// and loaded at every start, so the database stays the only truth.
	var router *routing.Router
	if cfg.Caddy.Enabled {
		router = &routing.Router{
			Store:  st,
			Loader: &caddy.Client{AdminURL: cfg.Caddy.AdminURL},
			Settings: routing.Settings{
				BaseDomain:      cfg.BaseDomain,
				ConsoleHost:     cfg.ConsoleHost,
				ConsoleUpstream: cfg.Listen,
				AdminListen:     cfg.Caddy.AdminListen(),
				ACMECA:          cfg.Caddy.ACMECA,
				ACMEEmail:       cfg.Caddy.ACMEEmail,
				AccessLogDir:    cfg.Caddy.AccessLogDir,
				ListenIP:        cfg.Caddy.ListenIP,
				InternalCA:      cfg.Caddy.InternalCA,
			},
			Logger: logger,
		}
		if err := router.ForceSync(ctx); err != nil {
			// Caddy may still be starting; the periodic sync retries.
			logger.Error("loading the Caddy configuration failed; will retry", "error", err)
		}
	} else {
		logger.Warn("caddy.enabled is false: routes are not configured")
	}
	checker := &routing.DomainChecker{
		Store:    st,
		Verifier: domains.Verifier{PublicIP: net.ParseIP(cfg.PublicIP)},
		Clock:    clk,
		Logger:   logger,
	}

	// GitHub App: login and repository access. Without it nobody can log
	// in and only public repositories given by URL can be deployed.
	var gh *github.Client
	if cfg.GitHub.Configured() {
		gh, err = newGitHubClient(cfg.GitHub)
		if err != nil {
			return err
		}
	} else {
		logger.Warn("the [github] section is not configured: login is unavailable")
	}
	if *insecureNoAuth {
		// With Caddy in front the console is reachable from the internet;
		// without login that would hand out root on the host. A Caddy that
		// listens on loopback only (local development) is fine.
		if cfg.Caddy.Enabled && !cfg.Caddy.LoopbackOnly() {
			return errors.New("--insecure-no-auth is refused while [caddy] enabled = true, " +
				"unless [caddy] listen_ip is a loopback address: it is for local development only")
		}
		logger.Warn("AUTHENTICATION IS DISABLED (--insecure-no-auth): do not use this on a reachable host")
	}
	deployOpts := deploy.Options{
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
		TaskTimeout:      time.Duration(cfg.Tasks.TimeoutMinutes) * time.Minute,
	}
	apiOpts := api.Options{
		Config:  cfg,
		Store:   st,
		System:  prober,
		Web:     web.Dist(),
		Logger:  logger,
		Version: version,
		Clock:   clk,
		Box:     box,
		Domains: checker,
		Auth: api.AuthOptions{
			AllowedUserID:  cfg.GitHub.AllowedUserID,
			ConsoleURL:     cfg.ConsoleURL,
			InsecureNoAuth: *insecureNoAuth,
		},
	}

	// Runtime logs: collected to local files for every app, and archived to
	// object storage when it is configured.
	accessLogDir := ""
	if cfg.Caddy.Enabled {
		accessLogDir = cfg.Caddy.AccessLogDir
	}
	collector, err := logs.New(logs.Options{
		Dir:           cfg.Logs.Dir,
		Runner:        run,
		Clock:         clk,
		Location:      cfg.Location(),
		Apps:          collectedApps(st, accessLogDir),
		Logger:        logger,
		RetentionDays: cfg.Logs.RetentionDays,
	})
	if err != nil {
		return err
	}
	logsAPI := &appLogs{collector: collector}
	var objects *objstore.S3
	if cfg.Storage.Configured() {
		objects, err = objstore.NewS3(objstore.S3Config{
			Endpoint:        cfg.Storage.Endpoint,
			Region:          cfg.Storage.Region,
			Bucket:          cfg.Storage.Bucket,
			AccessKeyID:     cfg.Storage.AccessKeyID,
			SecretAccessKey: cfg.Storage.SecretAccessKey,
		})
		if err != nil {
			return fmt.Errorf("object storage: %w", err)
		}
		archiver, err := logs.NewArchiver(logs.ArchiverOptions{
			Collector:     collector,
			Store:         objects,
			IntervalHours: cfg.Logs.ArchiveIntervalHours,
			AppsDir:       cfg.AppsDir(),
		})
		if err != nil {
			return err
		}
		logsAPI.archiver, logsAPI.objects = archiver, objects
		go archiver.Run(ctx)
	} else {
		logger.Warn("the [storage] section is not configured: logs stay local and backups are disabled")
	}
	go func() {
		if err := collector.Run(ctx); err != nil {
			logger.Error("the log collector stopped", "error", err)
		}
	}()
	go collector.RunMaintenance(ctx)
	apiOpts.Logs = logsAPI

	gitRefs := deploy.GitRefResolver{Fetcher: fetcher}
	apiOpts.Refs = gitRefs
	// Public repositories the App cannot see (or every repository, without
	// an App) are read anonymously for the variable comparison.
	publicGitHub, err := github.NewAnonymous("", nil)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	apiOpts.PublicFiles = publicGitHub
	if gh != nil {
		tokens := deploy.GitHubTokens{Client: gh}
		deployOpts.Tokens = tokens
		gitRefs.Tokens = tokens
		apiOpts.Refs = deploy.GitHubRefResolver{Client: gh, Fallback: gitRefs}
		apiOpts.Auth.GitHub = gh
		// Mirror deployments into the repositories' GitHub deployments
		// (ARCHITECTURE 5.1). Best effort, off the deploy path.
		ghDeployments := deploy.NewGitHubDeployments(deploy.GitHubDeploymentsOptions{
			Store:      st,
			Client:     gh,
			Clock:      clk,
			Logger:     logger,
			ConsoleURL: cfg.ConsoleURL,
			BaseDomain: cfg.BaseDomain,
		})
		deployOpts.Status = ghDeployments
		go ghDeployments.Run(ctx)
		if cfg.GitHub.ObsoleteRequireTwoFactor != nil {
			logger.Warn("github.require_two_factor is no longer used and can be removed from config.toml: " +
				"GitHub does not report the two-factor status to a GitHub App, so GitHub enforces it at sign-in instead")
		}
	}
	// Assign the interfaces only when there is a router: a nil *Router
	// stored in an interface would not compare equal to nil.
	if router != nil {
		checker.Router = router
		deployOpts.Router = router
		apiOpts.Router = router
		apiOpts.RouteStatus = func() api.RouteStatus {
			status := router.Status()
			return api.RouteStatus{LastLoadedAt: status.LastLoadedAt, LastError: status.LastError}
		}
	}
	// Listing an app's volumes takes Docker only, so it does not depend on
	// the backup configuration below.
	apiOpts.Volumes = appVolumes{runner: run}
	// Backups need object storage and the restic password. The backup
	// service stops and starts apps through the engine, and the engine
	// calls the backup service before a deploy, so the controller gets its
	// engine once both exist.
	controller := &appController{store: st}
	var backups *backupService
	if ok, reason := backupsConfigured(cfg); ok {
		if err := st.FailInterruptedBackupRuns(ctx, clk.Now()); err != nil {
			return err
		}
		backups, err = newBackupService(ctx, cfg, filepath.Dir(*configPath), st,
			backupDeps{runner: run, clock: clk, logger: logger, controller: controller})
		if err != nil {
			return err
		}
		deployOpts.Backup = backups
		deployOpts.CopyBackups = backups
		apiOpts.Backups = backups
	} else {
		logger.Warn("backups are disabled: " + reason)
	}

	engine := deploy.New(deployOpts)
	controller.engine = engine
	if backups != nil {
		go func() {
			if err := backups.scheduler.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("the backup scheduler stopped", "error", err)
			}
		}()
	}
	apiOpts.Deployer = engine
	apiOpts.Promoter = engine
	apiOpts.Ops = engine
	apiOpts.Copier = engine
	apiOpts.Tasks = engine
	go engine.RunMaintenance(ctx)
	// Memory use history for the limit guidance (ARCHITECTURE 9.5).
	go engine.NewMemorySampler().Run(ctx)
	srv := api.New(apiOpts)
	go checker.Run(ctx, routing.RecheckInterval)

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
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	logger.Info("koroku listening", "addr", listener.Addr().String(), "version", version)
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
	logger.Info("koroku shutting down")
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
