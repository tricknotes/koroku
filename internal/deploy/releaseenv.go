package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/store"
)

// The commit variables change with every deployment. Every release keeps
// them in a release env file of their own, read only by the services that
// every deployment recreates anyway (ARCHITECTURE 6), so that a database
// keeps its configuration across code-only deployments.

// commitVars are the variables of the release env file.
var commitVars = []string{"KOROKU_COMMIT_SHA", "KOROKU_COMMIT_SHORT"}

// probeCommit stands in for the commit when telling which services follow it.
const probeCommit = "0000000000000000000000000000000000000000"

// ReleaseEnvFile is the app's live env file with the commit variables.
func (e *Engine) ReleaseEnvFile(slug string) string {
	return filepath.Join(e.appDir(slug), "env.release")
}

// releaseEnvSnapshot is the release env file as release id had it.
func (e *Engine) releaseEnvSnapshot(slug string, id int64) string {
	return filepath.Join(e.stateDir(slug, id), "env.release")
}

// hasReleaseEnv reports whether release id has a release env file. A
// release without one keeps the commit variables in its env file, and its
// override does not read the release env file.
func (e *Engine) hasReleaseEnv(slug string, id int64) bool {
	_, err := os.Stat(e.releaseEnvSnapshot(slug, id))
	return err == nil
}

// splitCommitVars separates the commit variables from the others.
func splitCommitVars(vars map[string]string) (env, release map[string]string) {
	env, release = map[string]string{}, map[string]string{}
	for k, v := range vars {
		if slices.Contains(commitVars, k) {
			release[k] = v
		} else {
			env[k] = v
		}
	}
	return env, release
}

// readEnvSnapshots reads the env snapshot of a release, and its release
// env snapshot (nil for a release without one).
func (e *Engine) readEnvSnapshots(slug string, id int64) (env, release []byte, err error) {
	env, err = os.ReadFile(filepath.Join(e.stateDir(slug, id), "env"))
	if err != nil {
		return nil, nil, err
	}
	release, err = os.ReadFile(e.releaseEnvSnapshot(slug, id))
	if errors.Is(err, os.ErrNotExist) {
		return env, nil, nil
	}
	return env, release, err
}

// writeLiveEnv replaces the app's env files with a release's. release is
// nil for a release without the release env file, which is left as it is:
// nothing of that release reads it.
func (e *Engine) writeLiveEnv(slug string, env, release []byte) error {
	if err := writeFileAtomic(e.EnvFile(slug), env, 0o600); err != nil {
		return err
	}
	if release != nil {
		return writeFileAtomic(e.ReleaseEnvFile(slug), release, 0o600)
	}
	return nil
}

// commitServices normalizes the repository's project once more with a
// probe commit and lists the services whose definition follows the commit.
func (e *Engine) commitServices(ctx context.Context, app store.App, id int64, base compose.Invocation,
	releaseDir string, releaseVars map[string]string, actual compose.Project, log *deployLog,
) ([]string, error) {
	probeVars := map[string]string{}
	for k := range releaseVars {
		probeVars[k] = probeCommit
	}
	probeVars["KOROKU_COMMIT_SHORT"] = probeCommit[:7]
	content, err := RenderEnvFile(probeVars)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(e.stateDir(app.Slug, id), "env.probe")
	if err := writeFileAtomic(path, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("write env probe: %w", err)
	}
	defer func() { _ = os.Remove(path) }()
	probe := base
	probe.ReleaseEnvFile = path
	probed, err := e.composeConfig(ctx, probe, releaseDir, log)
	if err != nil {
		return nil, err
	}
	services := compose.CommitServices(actual, probed)
	if len(services) > 0 {
		log.Printf("Services that follow the commit: %s", strings.Join(services, ", "))
	}
	return services, nil
}

// envChanges lists, sorted, the names of the variables that differ
// between the env snapshot of the app's running release and env: added,
// removed or changed. known is false when there is no snapshot to compare
// with.
func (e *Engine) envChanges(app store.App, env []byte) (keys []string, known bool) {
	if app.CurrentDeploymentID == nil {
		return nil, false
	}
	previous, _, err := e.readEnvSnapshots(app.Slug, *app.CurrentDeploymentID)
	if err != nil {
		return nil, false
	}
	before, after := envEntries(previous), envEntries(env)
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			keys = append(keys, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, true
}

func envEntries(content []byte) map[string]string {
	out := map[string]string{}
	for _, entry := range parseEnvFileEntries(string(content)) {
		out[entry[0]] = entry[1]
	}
	return out
}

// noteEnvChange explains, for each of the services in names that does not
// read the release env file, that it is recreated because the env file
// changed since the running release, naming the variables (never their
// values). Nothing is written when the env file is the same or the running
// release has no snapshot to compare with.
func (e *Engine) noteEnvChange(app store.App, env []byte, names, releaseEnvServices []string, log *deployLog) {
	keys, known := e.envChanges(app, env)
	if !known || len(keys) == 0 {
		return
	}
	for _, name := range names {
		if !slices.Contains(releaseEnvServices, name) {
			log.Printf("Recreating %s because the app's env file changed (%s); requests may fail while it restarts.",
				name, strings.Join(keys, ", "))
		}
	}
}

// keptServices are the services of the running release that the new
// release (project) also has: the ones an `up` recreates rather than
// creates.
func (e *Engine) keptServices(app store.App, project compose.Project) []string {
	if app.CurrentDeploymentID == nil {
		return nil
	}
	meta, err := e.readMeta(app.Slug, *app.CurrentDeploymentID)
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range project.ServiceNames() {
		if slices.Contains(meta.Services, name) {
			out = append(out, name)
		}
	}
	return out
}
