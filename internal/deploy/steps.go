package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	// Only a commit that a branch or tag of the repository reaches is
	// deployed (ARCHITECTURE 7). Deployments are queued with their ref; one
	// without is checked against the branch the app follows.
	ref := d.Ref
	if ref == "" {
		ref = app.DeployBranch()
	}
	log.Printf("Checking that %s is on %s", shortSHA(d.CommitSHA), ref)
	commit, err := e.opts.Fetcher.Checkout(ctx, repoURL, d.CommitSHA, ref, releaseDir, auth, log.Writer())
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
	// A blue/green app replaces its public service side by side
	// (ARCHITECTURE 7.2).
	blueGreen := app.BlueGreen
	if blueGreen && app.AltHostPort == 0 {
		return false, errors.New("blue/green deploys are on, but the app has no second host port; turn the setting off and on again")
	}
	vars, fingerprints, err := e.renderEnvVars(ctx, app, commit.SHA, log.masker)
	if err != nil {
		return false, err
	}
	// The commit variables go to the services every deployment recreates
	// anyway (ARCHITECTURE 6).
	envVars, releaseVars := splitCommitVars(vars)
	envContent, err := RenderEnvFile(envVars)
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
	releaseContent, err := RenderEnvFile(releaseVars)
	if err != nil {
		return false, err
	}
	snapshotRelease := e.releaseEnvSnapshot(app.Slug, d.ID)
	if err := writeFileAtomic(snapshotRelease, []byte(releaseContent), 0o600); err != nil {
		return false, fmt.Errorf("write env snapshot: %w", err)
	}
	meta := releaseMeta{ComposeFile: composeFile, BlueGreen: blueGreen}
	if err := e.writeMeta(app.Slug, d.ID, meta); err != nil {
		return false, fmt.Errorf("write release metadata: %w", err)
	}

	// The app's own registry login, for the build, pull and start below.
	dockerConfig, err := e.registryConfig(ctx, app, log.masker)
	if err != nil {
		return false, err
	}
	if dockerConfig != "" {
		log.Printf("Container registry credential: set for this app")
	}

	// Validation and build read the candidate env from the snapshot so that
	// the running release's env file stays untouched until step 6.
	base := e.invocation(app, d.ID, composeFile, false)
	base.EnvFile = snapshot
	base.ReleaseEnvFile = snapshotRelease
	base.DockerConfig = dockerConfig
	// Compose loads what the file includes while it reads it, so the files
	// are checked before Compose sees them (ARCHITECTURE 6).
	if err := e.checkFiles(app, releaseDir, composeFile, log); err != nil {
		return false, err
	}
	repoProject, err := e.composeConfig(ctx, base, releaseDir, log)
	if err != nil {
		return false, err
	}
	if _, ok := repoProject.Services[app.WebService]; !ok {
		result := compose.CheckPolicy(repoProject, compose.PolicyParams{WebService: app.WebService, Relax: true})
		return false, &compose.PolicyError{Violations: filterRule(result.Violations, compose.RuleWebServiceMissing)}
	}
	problems := compose.CheckBlueGreen(repoProject, app.WebService)
	if problems == nil {
		problems = []compose.Violation{}
	}
	meta.BlueGreenProblems = &problems
	overrideParams := memoryParams(app, e.opts.DefaultMemory)
	overrideParams.Project = repoProject
	overrideParams.WebService = app.WebService
	overrideParams.WebPort = app.WebPort
	overrideParams.HostPort = app.HostPort
	overrideParams.EnvFile = e.EnvFile(app.Slug)
	overrideParams.ReleaseEnvFile = e.ReleaseEnvFile(app.Slug)
	overrideParams.ProjectName = app.ProjectName()
	overrideParams.ReleaseTag = "r" + strconv.FormatInt(d.ID, 10)
	if blueGreen {
		if len(problems) > 0 {
			for _, v := range problems {
				log.Printf("Policy violation: %s", v)
			}
			return false, &compose.PolicyError{Violations: problems}
		}
		overrideParams.BlueGreen = &compose.BlueGreenParams{
			ComposeFile: composeFile,
			AltHostPort: app.AltHostPort,
		}
	}
	commitServices, err := e.commitServices(ctx, app, d.ID, base, releaseDir, releaseVars, repoProject, log)
	if err != nil {
		return false, err
	}
	overrideParams.CommitServices = commitServices
	meta.ReleaseEnvServices = overrideParams.ReleaseEnvServices()
	override, err := compose.Override(overrideParams)
	if err != nil {
		return false, err
	}
	reservations := overrideParams.Reservations()
	if err := e.opts.Store.SetDeploymentMemoryReservations(ctx, d.ID, reservations); err != nil {
		return false, err
	}
	if err := e.opts.Store.SetDeploymentMemoryShapes(ctx, d.ID, repoProject.MemoryShapes()); err != nil {
		return false, err
	}
	if overrideParams.Production {
		log.Printf("Production app: killed last when the host runs out of memory, reserving %s", describeReservations(reservations))
	} else {
		log.Printf("Staging app: killed first when the host runs out of memory, no memory reserved")
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
	for _, path := range []string{e.EnvFile(app.Slug), e.ReleaseEnvFile(app.Slug)} {
		if err := ensureFile(path); err != nil {
			return false, fmt.Errorf("prepare env file: %w", err)
		}
	}
	merged := e.invocation(app, d.ID, composeFile, true)
	merged.EnvFile = snapshot
	merged.ReleaseEnvFile = snapshotRelease
	merged.DockerConfig = dockerConfig
	if blueGreen {
		// Build and pull see the public service the override disables.
		merged.Profiles = []string{compose.DisabledProfile}
		merged.BlueGreen = true
	}
	var configOutput strings.Builder
	project, err := e.composeConfigTo(ctx, merged, releaseDir, io.MultiWriter(log.Writer(), &configOutput))
	if err != nil {
		if blueGreen && compose.ExtendsFailure(configOutput.String(), app.WebService) {
			v := compose.IncludedViolation(app.WebService, composeFile)
			log.Printf("Policy violation: %s", v)
			return false, &compose.PolicyError{Violations: []compose.Violation{v}}
		}
		return false, err
	}
	policy := compose.CheckPolicy(project, e.policyParams(app, releaseDir, blueGreen))
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
	meta.Services = project.ServiceNames()
	// The coloured copies run the release's own image (or the public
	// service's, which is listed under it).
	pulling := project
	if blueGreen {
		pulling = project.WithoutServices(compose.ColoredNames(app.WebService)...)
	}
	// Recorded before pulling, like the built images, so that a failed
	// deploy does not leave its pulled images behind.
	meta.PulledImages = pulling.PulledImages()
	if err := e.writeMeta(app.Slug, d.ID, meta); err != nil {
		return false, fmt.Errorf("write release metadata: %w", err)
	}

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
			if err := e.composePulling(ctx, merged, releaseDir, log, build...); err != nil {
				return false, fmt.Errorf("build: %w", err)
			}
		}
	}
	if pulling.NeedsPull() {
		log.Step(StepPull, "Pulling images")
		if err := e.composePulling(ctx, merged, releaseDir, log, "pull", "--ignore-buildable"); err != nil {
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

	if blueGreen {
		return e.deployBlueGreen(ctx, d, app, blueGreenRelease{
			id: d.ID, composeFile: composeFile, project: project, dockerConfig: dockerConfig,
			env: []byte(envContent), releaseEnv: []byte(releaseContent),
			releaseEnvServices: meta.ReleaseEnvServices,
		}, log)
	}

	// 6. Start. From here on a failure rolls back to the previous release.
	log.Step(StepStart, "Starting containers")
	e.logStopGraces(ctx, app, project, log)
	if err := e.endSleep(ctx, app, log); err != nil {
		return false, err
	}
	e.noteEnvChange(app, []byte(envContent), e.keptServices(app, project), meta.ReleaseEnvServices, log)
	if err := e.writeLiveEnv(app.Slug, []byte(envContent), []byte(releaseContent)); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	if err := e.upRelease(ctx, app, d.ID, dockerConfig, e.deployRun(log)); err != nil {
		line := e.logContainerOutput(ctx, app, log)
		return true, withOutput(fmt.Errorf("start containers: %w", err), line)
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

	// 8. Confirm over HTTP, on the port the release now listens on.
	if current, err := e.opts.Store.GetAppByID(ctx, app.ID); err == nil {
		app = current
	}
	url := healthURL(app)
	log.Step(StepCheck, "Checking %s", url)
	if err := e.opts.Health.Wait(ctx, url, e.opts.HealthTimeout); err != nil {
		line := e.logContainerOutput(ctx, app, log)
		return true, withOutput(err, line)
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
	vars, fingerprints, err := e.renderEnvVars(ctx, app, commitSHA, masker)
	if err != nil {
		return "", nil, err
	}
	content, err := RenderEnvFile(vars)
	if err != nil {
		return "", nil, err
	}
	return content, fingerprints, nil
}

// renderEnvVars is renderEnv before rendering: the variables by name.
func (e *Engine) renderEnvVars(ctx context.Context, app store.App, commitSHA string, masker *secrets.Masker) (map[string]string, map[string]string, error) {
	rows, err := e.opts.Store.ListEnvVars(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
	vars := make(map[string]string, len(rows))
	fingerprints := make(map[string]string, len(rows))
	for _, row := range rows {
		ad := secrets.EnvVarContext(app.ID, row.Key)
		plain, err := e.opts.Box.Open(row.ValueEncrypted, ad)
		if err != nil {
			return nil, nil, fmt.Errorf("environment variable %s: %w", row.Key, err)
		}
		vars[row.Key] = string(plain)
		fingerprints[row.Key] = e.opts.Box.Fingerprint(ad, plain)
		masker.Add(string(plain))
	}
	// The provided variables are not the app's: they are not fingerprinted
	// (ARCHITECTURE 9.2) and not masked (a commit or a URL is not a secret).
	provided := Provided{CommitSHA: commitSHA, AppURL: PrimaryURL(ctx, e.opts.Store, app, e.opts.BaseDomain)}
	if review, err := e.opts.Store.GetReviewApp(ctx, app.ID); err == nil {
		provided.PRNumber = review.PRNumber
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	for k, v := range ProvidedEnv(provided) {
		vars[k] = v
	}
	return vars, fingerprints, nil
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
	if inv.BlueGreen && !slices.Contains(inv.Profiles, compose.DisabledProfile) {
		// A blue/green release: the public service the override disables
		// is part of the project to check (ARCHITECTURE 7.2).
		inv.Profiles = append(slices.Clone(inv.Profiles), compose.DisabledProfile)
	}
	var stdout bytes.Buffer
	err := e.opts.Runner.Run(ctx, runner.Cmd{
		Name: "docker",
		Args: inv.Args("config", "--format", "json", "--no-env-resolution"),
		Env:  inv.Env(),
		// The repository's file is interpolated with the app's variables,
		// not with koroku's own environment.
		CleanEnv: true,
		Dir:      dir,
		Stdout:   &stdout,
		Stderr:   stderr,
	})
	if err != nil {
		return compose.Project{}, fmt.Errorf("the Compose file is not valid: %w", err)
	}
	return compose.ParseProject(stdout.Bytes())
}

// checkFiles applies compose.ScanFiles to a release with the app's relax
// flag. log may be nil.
func (e *Engine) checkFiles(app store.App, dir, composeFile string, log *deployLog) error {
	var violations []compose.Violation
	for _, v := range compose.ScanFiles(dir, composeFile) {
		switch {
		case !app.RelaxPolicy:
			violations = append(violations, v)
			if log != nil {
				log.Printf("Policy violation: %s", v)
			}
		case log != nil:
			log.Printf("Policy waived for this app: %s", v)
		}
	}
	if len(violations) > 0 {
		return &compose.PolicyError{Violations: violations}
	}
	return nil
}

// revalidate checks a stored release against the policy again right before
// its files are used for another `up` (start, rollback). The files live in
// the checkout; an app that was allowed a writable bind mount, or anything
// else that got write access to the release directory, could have changed
// them since the deploy that checked them.
//
// dockerConfig is the Docker config directory of the Compose command that
// follows ("" when it runs without one). Compose interpolates the files
// with its environment and with the env files, so the project is only the
// one that will run when it is normalized with the same ones: the check
// uses the variables the release starts with (its env snapshot) and the
// same DOCKER_CONFIG.
func (e *Engine) revalidate(ctx context.Context, app store.App, id int64, composeFile, dockerConfig string) error {
	_, err := e.revalidatedProject(ctx, app, id, composeFile, dockerConfig)
	return err
}

// revalidatedProject is revalidate that also returns the merged project.
func (e *Engine) revalidatedProject(ctx context.Context, app store.App, id int64, composeFile, dockerConfig string) (compose.Project, error) {
	dir := e.ReleaseDir(app.Slug, id)
	if err := e.checkFiles(app, dir, composeFile, nil); err != nil {
		return compose.Project{}, err
	}
	inv := e.invocation(app, id, composeFile, true)
	inv.DockerConfig = dockerConfig
	if snapshot := filepath.Join(e.stateDir(app.Slug, id), "env"); fileExists(snapshot) {
		inv.EnvFile = snapshot
		if inv.ReleaseEnvFile != "" {
			inv.ReleaseEnvFile = e.releaseEnvSnapshot(app.Slug, id)
		}
	}
	project, err := e.composeConfigTo(ctx, inv, dir, io.Discard)
	if err != nil {
		return compose.Project{}, err
	}
	policy := compose.CheckPolicy(project, e.policyParams(app, dir, e.releaseIsBlueGreen(app.Slug, id)))
	if !policy.OK() {
		return compose.Project{}, &compose.PolicyError{Violations: policy.Violations}
	}
	return project, nil
}

func (e *Engine) compose(ctx context.Context, inv compose.Invocation, dir string, log *deployLog, sub ...string) error {
	return e.composeTo(ctx, inv, dir, log.Writer(), sub...)
}

func (e *Engine) composeTo(ctx context.Context, inv compose.Invocation, dir string, out io.Writer, sub ...string) error {
	return e.opts.Runner.Run(ctx, runner.Cmd{
		Name:     "docker",
		Args:     inv.Args(sub...),
		Env:      inv.Env(),
		CleanEnv: true,
		Dir:      dir,
		Stdout:   out,
		Stderr:   out,
	})
}

// composePulling runs a compose command that may pull images (build, pull,
// up). When it fails and the output says the registry refused access, the
// log gets a hint about the app's registry credential and the error is a
// *RegistryAuthError. When the output says the registry has no image for a
// tag, the log says so and the error wraps ErrImageNotFound.
func (e *Engine) composePulling(ctx context.Context, inv compose.Invocation, dir string, log *deployLog, sub ...string) error {
	var last string
	tail := &lineWatcher{dst: log.Writer(), match: func(line string) bool {
		if line = strings.TrimSpace(line); line != "" {
			last = line
		}
		return false
	}}
	auth := &lineWatcher{dst: tail, match: looksLikeRegistryAuthFailure}
	missing := &lineWatcher{dst: auth, match: looksLikeMissingImage}
	err := e.composeTo(ctx, inv, dir, missing, sub...)
	if err == nil || ctx.Err() != nil {
		return err
	}
	switch {
	case auth.Seen():
		configured := inv.DockerConfig != ""
		logRegistryHint(log, configured)
		return &RegistryAuthError{Configured: configured, Err: err}
	case missing.Seen():
		logMissingImageHint(log)
		return fmt.Errorf("%w: %w", ErrImageNotFound, err)
	}
	// Compose ends a failed run with the reason, such as
	// `service "migrate" didn't complete successfully: exit 1`.
	if last != "" {
		return fmt.Errorf("%w: %s", err, capRunes(last, failureLineMax))
	}
	return err
}

func (e *Engine) up(ctx context.Context, inv compose.Invocation, dir string, log *deployLog) error {
	return e.composePulling(ctx, inv, dir, log, e.upArgs()...)
}

// upArgs is the `up` that every deploy, rollback and start runs. With
// services, only those are brought up, without their dependencies and
// without removing the others.
//
// --pull missing overrides a pull_policy in the Compose file (such as
// always): images are pulled in the pull step of a deploy, and a rollback or
// a start must bring up the images the release already has, even when the
// registry is unreachable or no longer has the tag (ARCHITECTURE 7). An image
// that is gone from the host is still pulled.
func (e *Engine) upArgs(services ...string) []string {
	args := []string{"up", "-d", "--pull", "missing"}
	if len(services) > 0 {
		args = append(args, "--no-deps")
	} else {
		args = append(args, "--remove-orphans")
	}
	args = append(args, "--wait", "--wait-timeout", e.waitSeconds())
	return append(args, services...)
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
//
// With services, only their output is copied. It returns the line of that
// output that most likely names the reason (see failureLine), "" when none
// does; withOutput adds it to the deployment's error.
func (e *Engine) logContainerOutput(ctx context.Context, app store.App, log *deployLog, services ...string) string {
	if ctx.Err() != nil {
		return "" // canceled or shutting down: nobody is waiting for the reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log.Printf("Last output of the containers:")
	inv := compose.Invocation{ProjectName: app.ProjectName()}
	tail := strconv.Itoa(containerOutputLines)
	var output bytes.Buffer
	args := append([]string{"logs", "--no-color", "--tail", tail}, services...)
	if err := e.composeTo(ctx, inv, "", io.MultiWriter(log.Writer(), &output), args...); err != nil {
		log.Printf("Reading the container output failed: %v", err)
		return ""
	}
	return failureLine(output.String())
}

// failureLineMax caps the container line added to a deployment's error.
const failureLineMax = 200

// failureLine picks, from the output of docker compose logs, the last line
// that reads like an error message, as "<container>: <message>". Stack
// frames (indented lines and Ruby's "from ..." lines) are skipped: the
// message that names the reason comes before them. Rake prints
// "<task> aborted!" and the message on the next line; both are taken. It
// is "" when no line mentions an error, so that an unrelated last line is
// never presented as the reason.
func failureLine(output string) string {
	type line struct{ container, message string }
	var lines []line
	for _, raw := range strings.Split(output, "\n") {
		container, message, found := strings.Cut(raw, " | ")
		if !found {
			container, message = "", raw
		}
		lines = append(lines, line{strings.TrimSpace(container), message})
	}
	for i := len(lines) - 1; i >= 0; i-- {
		container, message := lines[i].container, lines[i].message
		if message == "" || message[0] == ' ' || message[0] == '\t' || strings.HasPrefix(message, "from ") {
			continue
		}
		message = strings.TrimSpace(message)
		if strings.HasSuffix(message, " aborted!") {
			if i+1 < len(lines) && lines[i+1].container == container && strings.TrimSpace(lines[i+1].message) != "" {
				message += " " + strings.TrimSpace(lines[i+1].message)
			}
		} else if lower := strings.ToLower(message); !strings.Contains(lower, "error") && !strings.Contains(lower, "exception") &&
			!strings.Contains(lower, "fatal") && !strings.Contains(lower, "panic") {
			continue
		}
		message = capRunes(message, failureLineMax)
		if container != "" {
			return container + ": " + message
		}
		return message
	}
	return ""
}

// capRunes cuts s to at most n characters, marking the cut with "…".
func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// withOutput adds the container line from logContainerOutput to err, which
// stays unwrappable.
func withOutput(err error, line string) error {
	if line == "" {
		return err
	}
	return fmt.Errorf("%w (container output: %s)", err, line)
}

// rollback brings the previous successful release back up after a failed
// start. Without a previous release the half-started project is removed
// (volumes are kept). Problems are logged, never returned: the deployment
// has already failed. restored reports whether the previous release runs
// again.
func (e *Engine) rollback(app store.App, log *deployLog) (restored bool) {
	ctx, cancel := context.WithTimeout(context.Background(), e.upTimeout())
	defer cancel()

	if app.CurrentDeploymentID == nil {
		log.Step(StepRollback, "No previous release; removing the failed containers")
		inv := compose.Invocation{ProjectName: app.ProjectName()}
		if err := e.compose(ctx, inv, "", log, "down", "--remove-orphans"); err != nil {
			log.Printf("Removing the containers failed: %v", err)
		}
		// Nothing runs in a colour any more.
		if err := e.opts.Store.SetAppWebColor(ctx, app.ID, ""); err != nil {
			log.Printf("Recording the colour of the public service failed: %v", err)
		}
		return false
	}

	prev := *app.CurrentDeploymentID
	log.Step(StepRollback, "Rolling back to deployment #%d", prev)
	meta, err := e.readMeta(app.Slug, prev)
	if err != nil {
		log.Printf("Rollback is not possible: the previous release is no longer on disk.")
		return false
	}
	prevDir := e.ReleaseDir(app.Slug, prev)
	if _, err := os.Stat(filepath.Join(prevDir, compose.OverrideFileName)); err != nil {
		log.Printf("Rollback is not possible: the previous release is no longer on disk.")
		return false
	}
	env, releaseEnv, err := e.readEnvSnapshots(app.Slug, prev)
	if err != nil {
		log.Printf("Rollback is not possible: the previous environment snapshot is missing.")
		return false
	}
	dockerConfig, err := e.registryConfig(ctx, app, log.masker)
	if err != nil {
		log.Printf("Rollback failed: %v", err)
		return false
	}
	if err := e.revalidate(ctx, app, prev, meta.ComposeFile, dockerConfig); err != nil {
		log.Printf("Rollback is not possible: the previous release no longer passes validation (%v).", err)
		return false
	}
	if err := e.writeLiveEnv(app.Slug, env, releaseEnv); err != nil {
		log.Printf("Rollback failed: the env file could not be restored: %v", err)
		return false
	}
	// The failed release may have changed the colour; the previous one
	// comes back in the colour the database now names.
	if current, err := e.opts.Store.GetAppByID(ctx, app.ID); err == nil {
		app = current
	}
	if err := e.upRelease(ctx, app, prev, dockerConfig, e.deployRun(log)); err != nil {
		log.Printf("Rollback failed: %v", err)
		return false
	}
	log.Printf("Rolled back: deployment #%d is running again.", prev)
	return true
}

func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeFileAtomic(path, nil, 0o600)
}

// memoryParams holds the app settings that decide memory limits and
// reservations (ARCHITECTURE 6.2).
func memoryParams(app store.App, defaultMemory string) compose.OverrideParams {
	return compose.OverrideParams{
		DefaultMemory: defaultMemory,
		AppMemory:     app.MemoryLimit,
		ServiceMemory: app.ServiceMemoryLimits,
		// The environment decides which apps a host-wide memory shortage
		// hits first.
		Production:         app.Environment == store.EnvironmentProduction,
		AppReservation:     app.MemoryReservation,
		ServiceReservation: app.ServiceMemoryReservations,
	}
}

// NextReservations is what the next deployment of the app reserves for
// each service (ARCHITECTURE 6.2): the app's current settings applied to
// the services of its running release, whose shapes the deployment
// recorded. Services without a reservation are left out.
func NextReservations(app store.App, defaultMemory string, shapes map[string]compose.MemoryShape) map[string]int64 {
	return memoryParams(app, defaultMemory).ReservationsFor(shapes)
}

// describeReservations renders per-service reservations for the deploy
// log: "db 256 MiB, web 128 MiB", or "nothing".
func describeReservations(reservations map[string]int64) string {
	if len(reservations) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(reservations))
	for _, name := range slices.Sorted(maps.Keys(reservations)) {
		parts = append(parts, fmt.Sprintf("%s %d MiB", name, reservations[name]>>20))
	}
	return strings.Join(parts, ", ")
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

// fileExists reports whether path names something that can be read.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
