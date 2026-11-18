package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// execute runs the deploy steps. touched reports whether the running
// containers may have been changed, which is when a failure needs a rollback
// (ARCHITECTURE chapter 7: failures up to build/pull leave the app alone).
func (e *Engine) execute(ctx context.Context, d store.Deployment, app store.App, log *deployLog) (touched bool, err error) {
	if err := e.CheckDisk(); err != nil {
		return false, err
	}
	releaseDir := e.ReleaseDir(app.Slug, d.ID)

	// 2. Fetch.
	log.Step("Fetching %s", d.CommitSHA)
	auth, err := e.repoAuth(ctx, app, log.masker)
	if err != nil {
		return false, err
	}
	repoURL := app.CloneURL()
	if repoURL == "" {
		return false, errors.New("the app has no repository configured")
	}
	commit, err := e.opts.Fetcher.Checkout(ctx, repoURL, d.CommitSHA, releaseDir, auth, log.Writer())
	if err != nil {
		return false, fmt.Errorf("fetch source: %w", err)
	}
	if err := e.opts.Store.SetDeploymentCommit(ctx, d.ID, commit.SHA, commit.Message); err != nil {
		return false, err
	}
	log.Printf("Checked out %s %s", shortSHA(commit.SHA), commit.Message)

	// 3. Generate the env file and the override.
	log.Step("Preparing environment and Compose override")
	composeFile, err := compose.DetectFile(releaseDir, app.ComposePath)
	if err != nil {
		return false, err
	}
	log.Printf("Compose file: %s", composeFile)
	envContent, err := e.renderEnv(ctx, app, log.masker)
	if err != nil {
		return false, err
	}
	// The snapshot lets a later rollback restore the environment this
	// release ran with. The live env file is only replaced right before
	// the containers are touched.
	snapshot := filepath.Join(e.stateDir(app.Slug, d.ID), "env")
	if err := writeFileAtomic(snapshot, []byte(envContent), 0o600); err != nil {
		return false, fmt.Errorf("write env snapshot: %w", err)
	}
	meta := releaseMeta{ComposeFile: composeFile}
	if err := e.writeMeta(app.Slug, d.ID, meta); err != nil {
		return false, fmt.Errorf("write release metadata: %w", err)
	}

	// Validation and build read the candidate env from the snapshot so that
	// the running release's env file stays untouched until step 6.
	base := e.invocation(app, d.ID, composeFile, false)
	base.EnvFile = snapshot
	repoProject, err := e.composeConfig(ctx, base, releaseDir, log)
	if err != nil {
		return false, err
	}
	if _, ok := repoProject.Services[app.WebService]; !ok {
		result := compose.CheckPolicy(repoProject, compose.PolicyParams{WebService: app.WebService, Relax: true})
		return false, &compose.PolicyError{Violations: filterRule(result.Violations, compose.RuleWebServiceMissing)}
	}
	overrideParams := compose.OverrideParams{
		Project:       repoProject,
		WebService:    app.WebService,
		WebPort:       app.WebPort,
		HostPort:      app.HostPort,
		EnvFile:       e.EnvFile(app.Slug),
		DefaultMemory: e.opts.DefaultMemory,
		AppMemory:     app.MemoryLimit,
		ServiceMemory: app.ServiceMemoryLimits,
		ProjectName:   app.ProjectName(),
		ReleaseTag:    "r" + strconv.FormatInt(d.ID, 10),
	}
	override, err := compose.Override(overrideParams)
	if err != nil {
		return false, err
	}
	// Recorded before building so that the images are removed with the
	// release even when a later step fails.
	meta.Images = overrideParams.ReleaseImages()
	if err := e.writeMeta(app.Slug, d.ID, meta); err != nil {
		return false, fmt.Errorf("write release metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, compose.OverrideFileName), override, 0o644); err != nil {
		return false, fmt.Errorf("write override: %w", err)
	}

	// 4. Validate the merged project. The override points every service at
	// the live env file, which may not exist before the first deploy.
	log.Step("Validating the Compose project")
	if err := ensureFile(e.EnvFile(app.Slug)); err != nil {
		return false, fmt.Errorf("prepare env file: %w", err)
	}
	merged := e.invocation(app, d.ID, composeFile, true)
	merged.EnvFile = snapshot
	project, err := e.composeConfig(ctx, merged, releaseDir, log)
	if err != nil {
		return false, err
	}
	policy := compose.CheckPolicy(project, compose.PolicyParams{
		ReleaseDir: releaseDir,
		WebService: app.WebService,
		WebPort:    app.WebPort,
		HostPort:   app.HostPort,
		Relax:      app.RelaxPolicy,
	})
	for _, v := range policy.Waived {
		log.Printf("Policy waived for this app: %s", v)
	}
	if !policy.OK() {
		for _, v := range policy.Violations {
			log.Printf("Policy violation: %s", v)
		}
		return false, &compose.PolicyError{Violations: policy.Violations}
	}
	log.Printf("Services: %v", project.ServiceNames())

	// 5. Build and pull.
	if project.NeedsBuild() {
		log.Step("Building images")
		if err := e.compose(ctx, merged, releaseDir, log, "build"); err != nil {
			return false, fmt.Errorf("build: %w", err)
		}
	}
	if project.NeedsPull() {
		log.Step("Pulling images")
		if err := e.compose(ctx, merged, releaseDir, log, "pull", "--ignore-buildable"); err != nil {
			return false, fmt.Errorf("pull: %w", err)
		}
	}

	// 6. Start. From here on a failure rolls back to the previous release.
	log.Step("Starting containers")
	if err := writeFileAtomic(e.EnvFile(app.Slug), []byte(envContent), 0o600); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	live := e.invocation(app, d.ID, composeFile, true)
	if err := e.up(ctx, live, releaseDir, log); err != nil {
		return true, fmt.Errorf("start containers: %w", err)
	}

	// 7. Route.
	if e.opts.Router != nil {
		log.Step("Updating routes")
		if err := e.opts.Router.Sync(ctx); err != nil {
			return true, fmt.Errorf("update routes: %w", err)
		}
	}

	// 8. Confirm over HTTP.
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.HostPort, app.HealthcheckPath)
	log.Step("Checking %s", url)
	if err := e.opts.Health.Wait(ctx, url, e.opts.HealthTimeout); err != nil {
		return true, err
	}
	log.Printf("The app is answering.")
	return true, nil
}

func (e *Engine) repoAuth(ctx context.Context, app store.App, masker *secrets.Masker) (*gitrepo.Auth, error) {
	if e.opts.Tokens == nil {
		return nil, nil
	}
	token, err := e.opts.Tokens.RepoToken(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("get repository token: %w", err)
	}
	if token == "" {
		return nil, nil
	}
	auth := &gitrepo.Auth{Token: token}
	masker.Add(gitrepo.SecretsFor(auth)...)
	return auth, nil
}

// renderEnv decrypts the app's variables, registers their values for
// masking and renders the env file.
func (e *Engine) renderEnv(ctx context.Context, app store.App, masker *secrets.Masker) (string, error) {
	rows, err := e.opts.Store.ListEnvVars(ctx, app.ID)
	if err != nil {
		return "", err
	}
	vars := make(map[string]string, len(rows))
	for _, row := range rows {
		plain, err := e.opts.Box.Open(row.ValueEncrypted, secrets.EnvVarContext(app.ID, row.Key))
		if err != nil {
			return "", fmt.Errorf("environment variable %s: %w", row.Key, err)
		}
		vars[row.Key] = string(plain)
		masker.Add(string(plain))
	}
	return RenderEnvFile(vars)
}

// composeConfig runs `docker compose config --format json`. Its output holds
// resolved environment values, so only stderr goes to the log.
func (e *Engine) composeConfig(ctx context.Context, inv compose.Invocation, dir string, log *deployLog) (compose.Project, error) {
	var stdout bytes.Buffer
	err := e.opts.Runner.Run(ctx, runner.Cmd{
		Name:   "docker",
		Args:   inv.Args("config", "--format", "json"),
		Dir:    dir,
		Stdout: &stdout,
		Stderr: log.Writer(),
	})
	if err != nil {
		return compose.Project{}, fmt.Errorf("the Compose file is not valid: %w", err)
	}
	return compose.ParseProject(stdout.Bytes())
}

func (e *Engine) compose(ctx context.Context, inv compose.Invocation, dir string, log *deployLog, sub ...string) error {
	return e.opts.Runner.Run(ctx, runner.Cmd{
		Name:   "docker",
		Args:   inv.Args(sub...),
		Dir:    dir,
		Stdout: log.Writer(),
		Stderr: log.Writer(),
	})
}

func (e *Engine) up(ctx context.Context, inv compose.Invocation, dir string, log *deployLog) error {
	wait := strconv.Itoa(int(e.opts.WaitTimeout / time.Second))
	return e.compose(ctx, inv, dir, log, "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", wait)
}

// rollback brings the previous successful release back up after a failed
// start. Without a previous release the half-started project is removed
// (volumes are kept). Problems are logged, never returned: the deployment
// has already failed.
func (e *Engine) rollback(app store.App, log *deployLog) {
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.WaitTimeout+time.Minute)
	defer cancel()

	if app.CurrentDeploymentID == nil {
		log.Step("No previous release; removing the failed containers")
		inv := compose.Invocation{ProjectName: app.ProjectName()}
		if err := e.compose(ctx, inv, "", log, "down", "--remove-orphans"); err != nil {
			log.Printf("Removing the containers failed: %v", err)
		}
		return
	}

	prev := *app.CurrentDeploymentID
	log.Step("Rolling back to deployment #%d", prev)
	meta, err := e.readMeta(app.Slug, prev)
	if err != nil {
		log.Printf("Rollback is not possible: the previous release is no longer on disk.")
		return
	}
	prevDir := e.ReleaseDir(app.Slug, prev)
	if _, err := os.Stat(filepath.Join(prevDir, compose.OverrideFileName)); err != nil {
		log.Printf("Rollback is not possible: the previous release is no longer on disk.")
		return
	}
	env, err := os.ReadFile(filepath.Join(e.stateDir(app.Slug, prev), "env"))
	if err != nil {
		log.Printf("Rollback is not possible: the previous environment snapshot is missing.")
		return
	}
	if err := writeFileAtomic(e.EnvFile(app.Slug), env, 0o600); err != nil {
		log.Printf("Rollback failed: the env file could not be restored: %v", err)
		return
	}
	if err := e.up(ctx, e.invocation(app, prev, meta.ComposeFile, true), prevDir, log); err != nil {
		log.Printf("Rollback failed: %v", err)
		return
	}
	log.Printf("Rolled back: deployment #%d is running again.", prev)
}

func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeFileAtomic(path, nil, 0o600)
}

func filterRule(violations []compose.Violation, rule string) []compose.Violation {
	var out []compose.Violation
	for _, v := range violations {
		if v.Rule == rule {
			out = append(out, v)
		}
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
