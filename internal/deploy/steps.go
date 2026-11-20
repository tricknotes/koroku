package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	log.Step(StepFetch, "Fetching %s", d.CommitSHA)
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
	log.Step(StepPrepare, "Preparing environment and Compose override")
	composeFile, err := compose.DetectFile(releaseDir, app.ComposePath)
	if err != nil {
		return false, err
	}
	log.Printf("Compose file: %s", composeFile)
	envContent, fingerprints, err := e.renderEnv(ctx, app, commit.SHA, log.masker)
	if err != nil {
		return false, err
	}
	// What this release runs with, so the console can tell which saved
	// variables are not deployed yet (ARCHITECTURE 9.2).
	if err := e.opts.Store.SetDeploymentEnvFingerprints(ctx, d.ID, fingerprints); err != nil {
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
	// The checkout is repository content: a file of this name may already
	// exist there as a symlink to anywhere. Writing through a temporary
	// file and renaming replaces the link itself instead of following it.
	if err := writeFileAtomic(filepath.Join(releaseDir, compose.OverrideFileName), override, 0o644); err != nil {
		return false, fmt.Errorf("write override: %w", err)
	}

	// 4. Validate the merged project. The override points every service at
	// the live env file, which may not exist before the first deploy.
	log.Step(StepValidate, "Validating the Compose project")
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
		ReleaseDir:  releaseDir,
		ProjectName: app.ProjectName(),
		EnvFile:     e.EnvFile(app.Slug),
		WebService:  app.WebService,
		WebPort:     app.WebPort,
		HostPort:    app.HostPort,
		Relax:       app.RelaxPolicy,
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
		build := []string{"build"}
		if d.PromotedFromID != nil {
			// A promotion tags the promoted release's images where that is
			// safe and builds only the rest (ARCHITECTURE 9.7).
			build = append(build, e.reuseImages(ctx, d, app, repoProject, releaseDir, log)...)
		}
		if d.PromotedFromID == nil || len(build) > 1 {
			log.Step(StepBuild, "Building images")
			if err := e.compose(ctx, merged, releaseDir, log, build...); err != nil {
				return false, fmt.Errorf("build: %w", err)
			}
		}
	}
	if project.NeedsPull() {
		log.Step(StepPull, "Pulling images")
		if err := e.compose(ctx, merged, releaseDir, log, "pull", "--ignore-buildable"); err != nil {
			return false, fmt.Errorf("pull: %w", err)
		}
	}

	// Optional backup of the data the new release is about to touch. A
	// first deploy has nothing to back up yet.
	if app.BackupBeforeDeploy && app.CurrentDeploymentID != nil {
		if e.opts.Backup == nil {
			log.Printf("Backup before deploy is enabled for this app, but backups are not configured; skipping.")
		} else {
			log.Step(StepBackup, "Backing up before deploy")
			if err := e.opts.Backup.BackupApp(ctx, app); err != nil {
				return false, fmt.Errorf("backup before deploy: %w", err)
			}
			log.Printf("Backup finished.")
		}
	}

	// 6. Start. From here on a failure rolls back to the previous release.
	log.Step(StepStart, "Starting containers")
	if err := writeFileAtomic(e.EnvFile(app.Slug), []byte(envContent), 0o600); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	live := e.invocation(app, d.ID, composeFile, true)
	if err := e.up(ctx, live, releaseDir, log); err != nil {
		e.logContainerOutput(ctx, app, log)
		return true, fmt.Errorf("start containers: %w", err)
	}

	return e.routeAndCheck(ctx, app, log)
}

// routeAndCheck runs steps 7 and 8 once the containers are up.
func (e *Engine) routeAndCheck(ctx context.Context, app store.App, log *deployLog) (touched bool, err error) {
	// 7. Route.
	if e.opts.Router != nil {
		log.Step(StepRoute, "Updating routes")
		if err := e.opts.Router.Sync(ctx); err != nil {
			return true, fmt.Errorf("update routes: %w", err)
		}
	}

	// 8. Confirm over HTTP.
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.HostPort, app.HealthcheckPath)
	log.Step(StepCheck, "Checking %s", url)
	if err := e.opts.Health.Wait(ctx, url, e.opts.HealthTimeout); err != nil {
		e.logContainerOutput(ctx, app, log)
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
// masking and renders the env file together with the variables koroku
// provides (ProvidedEnv). It also returns the fingerprint of
// each variable.
func (e *Engine) renderEnv(ctx context.Context, app store.App, commitSHA string, masker *secrets.Masker) (string, map[string]string, error) {
	rows, err := e.opts.Store.ListEnvVars(ctx, app.ID)
	if err != nil {
		return "", nil, err
	}
	vars := make(map[string]string, len(rows))
	fingerprints := make(map[string]string, len(rows))
	for _, row := range rows {
		ad := secrets.EnvVarContext(app.ID, row.Key)
		plain, err := e.opts.Box.Open(row.ValueEncrypted, ad)
		if err != nil {
			return "", nil, fmt.Errorf("environment variable %s: %w", row.Key, err)
		}
		vars[row.Key] = string(plain)
		fingerprints[row.Key] = e.opts.Box.Fingerprint(ad, plain)
		masker.Add(string(plain))
	}
	// The provided variables are not the app's: they are not fingerprinted
	// (ARCHITECTURE 9.2) and not masked (a commit is not a secret).
	for k, v := range ProvidedEnv(commitSHA) {
		vars[k] = v
	}
	content, err := RenderEnvFile(vars)
	if err != nil {
		return "", nil, err
	}
	return content, fingerprints, nil
}

// composeConfig runs `docker compose config --format json`. With
// --no-env-resolution the env files are listed by path instead of being
// folded into the environment: the policy check needs the paths, and the
// values of the app's variables stay out of the output. The output is still
// never logged; only stderr goes to the log.
func (e *Engine) composeConfig(ctx context.Context, inv compose.Invocation, dir string, log *deployLog) (compose.Project, error) {
	return e.composeConfigTo(ctx, inv, dir, log.Writer())
}

func (e *Engine) composeConfigTo(ctx context.Context, inv compose.Invocation, dir string, stderr io.Writer) (compose.Project, error) {
	var stdout bytes.Buffer
	err := e.opts.Runner.Run(ctx, runner.Cmd{
		Name:   "docker",
		Args:   inv.Args("config", "--format", "json", "--no-env-resolution"),
		Dir:    dir,
		Stdout: &stdout,
		Stderr: stderr,
	})
	if err != nil {
		return compose.Project{}, fmt.Errorf("the Compose file is not valid: %w", err)
	}
	return compose.ParseProject(stdout.Bytes())
}

// revalidate checks a stored release against the policy again right before
// its files are used for another `up` (start, rollback). The files live in
// the checkout; an app that was allowed a writable bind mount, or anything
// else that got write access to the release directory, could have changed
// them since the deploy that checked them.
func (e *Engine) revalidate(ctx context.Context, app store.App, id int64, composeFile string) error {
	_, err := e.revalidatedProject(ctx, app, id, composeFile)
	return err
}

// revalidatedProject is revalidate that also returns the merged project.
func (e *Engine) revalidatedProject(ctx context.Context, app store.App, id int64, composeFile string) (compose.Project, error) {
	dir := e.ReleaseDir(app.Slug, id)
	project, err := e.composeConfigTo(ctx, e.invocation(app, id, composeFile, true), dir, io.Discard)
	if err != nil {
		return compose.Project{}, err
	}
	policy := compose.CheckPolicy(project, compose.PolicyParams{
		ReleaseDir:  dir,
		ProjectName: app.ProjectName(),
		EnvFile:     e.EnvFile(app.Slug),
		WebService:  app.WebService,
		WebPort:     app.WebPort,
		HostPort:    app.HostPort,
		Relax:       app.RelaxPolicy,
	})
	if !policy.OK() {
		return compose.Project{}, &compose.PolicyError{Violations: policy.Violations}
	}
	return project, nil
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

// containerOutputLines is how many lines of each container are copied into
// the deploy log when the new release does not come up.
const containerOutputLines = 30

// logContainerOutput copies the last output of the app's containers into the
// deploy log. It is called when the new release failed to start or to
// answer, before the rollback replaces those containers: what they printed
// is usually the reason, and afterwards it is only in the app's own logs.
// The output passes the deploy log's masking like everything else. Problems
// are noted in the log and otherwise ignored: the deployment failed already.
func (e *Engine) logContainerOutput(ctx context.Context, app store.App, log *deployLog) {
	if ctx.Err() != nil {
		return // canceled or shutting down: nobody is waiting for the reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log.Printf("Last output of the containers:")
	inv := compose.Invocation{ProjectName: app.ProjectName()}
	tail := strconv.Itoa(containerOutputLines)
	if err := e.compose(ctx, inv, "", log, "logs", "--no-color", "--tail", tail); err != nil {
		log.Printf("Reading the container output failed: %v", err)
	}
}

// rollback brings the previous successful release back up after a failed
// start. Without a previous release the half-started project is removed
// (volumes are kept). Problems are logged, never returned: the deployment
// has already failed.
func (e *Engine) rollback(app store.App, log *deployLog) {
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.WaitTimeout+time.Minute)
	defer cancel()

	if app.CurrentDeploymentID == nil {
		log.Step(StepRollback, "No previous release; removing the failed containers")
		inv := compose.Invocation{ProjectName: app.ProjectName()}
		if err := e.compose(ctx, inv, "", log, "down", "--remove-orphans"); err != nil {
			log.Printf("Removing the containers failed: %v", err)
		}
		return
	}

	prev := *app.CurrentDeploymentID
	log.Step(StepRollback, "Rolling back to deployment #%d", prev)
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
	if err := e.revalidate(ctx, app, prev, meta.ComposeFile); err != nil {
		log.Printf("Rollback is not possible: the previous release no longer passes validation (%v).", err)
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
