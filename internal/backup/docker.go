package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
)

const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeVolume  = "com.docker.compose.volume"
)

// volume is one named volume of a Compose project.
type volume struct {
	// Name is Docker's name (koroku-blog_pgdata).
	Name string
	// Short is the name in the Compose file (pgdata).
	Short string
	// Mountpoint is where the data lives on the host.
	Mountpoint string
}

// docker runs one docker command and returns its standard output. Standard
// error is masked and its last lines become part of the error.
func (m *Manager) docker(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeouts.Command)
	defer cancel()
	var out bytes.Buffer
	errTail := &tail{}
	stderr := secrets.NewLineWriter(errTail, m.masker)
	err := m.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, Stdout: &out, Stderr: stderr})
	_ = stderr.Flush()
	if err != nil {
		return nil, &commandError{Command: "docker " + strings.Join(args[:min(2, len(args))], " "), Err: err, Detail: errTail.String()}
	}
	return out.Bytes(), nil
}

// dockerFunc runs one docker command and returns its standard output.
type dockerFunc func(ctx context.Context, args ...string) ([]byte, error)

// volumes lists the named volumes Compose created for the project, sorted by
// their short name.
func (m *Manager) volumes(ctx context.Context, project string) ([]volume, error) {
	return listVolumes(ctx, m.docker, project)
}

// volumeNamesTimeout bounds VolumeNames: two short docker queries on behalf
// of an HTTP request.
const volumeNamesTimeout = 30 * time.Second

// VolumeNames lists the named volumes of a Compose project that exist in
// Docker, by the name written in the Compose file (pgdata, not
// koroku-blog_pgdata), sorted. These are the volumes a backup run walks before
// it applies App.ExcludeVolumes, and the names snapshots are tagged with.
//
// It needs no Manager: listing volumes takes Docker only, so it also works
// on a host without a backup repository. A project that was never started
// has no volumes; that is an empty list, not an error.
func VolumeNames(ctx context.Context, r runner.Runner, project string) ([]string, error) {
	if !nameRE.MatchString(project) {
		return nil, fmt.Errorf("backup: invalid Compose project name %q", project)
	}
	ctx, cancel := context.WithTimeout(ctx, volumeNamesTimeout)
	defer cancel()
	docker := func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := runner.Output(ctx, r, runner.Cmd{Name: "docker", Args: args})
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
		}
		return out, nil
	}
	vols, err := listVolumes(ctx, docker, project)
	if err != nil {
		return nil, fmt.Errorf("list the volumes of %s: %w", project, err)
	}
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Short)
	}
	return names, nil
}

// listVolumes is the one definition of "the volumes of a project": the
// volumes that carry the project's Compose label, sorted by their short
// name. External volumes do not carry the label and are therefore not
// listed: they belong to someone else.
func listVolumes(ctx context.Context, docker dockerFunc, project string) ([]volume, error) {
	out, err := docker(ctx, "volume", "ls", "--filter", "label="+labelComposeProject+"="+project, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	// "--" keeps a volume name from ever being read as an option.
	out, err = docker(ctx, append([]string{"volume", "inspect", "--"}, names...)...)
	if err != nil {
		return nil, err
	}
	var inspected []struct {
		Name       string            `json:"Name"`
		Mountpoint string            `json:"Mountpoint"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(out, &inspected); err != nil {
		return nil, fmt.Errorf("decode docker volume inspect: %w", err)
	}
	vols := make([]volume, 0, len(inspected))
	for _, v := range inspected {
		if v.Labels[labelComposeProject] != project {
			continue
		}
		short := v.Labels[labelComposeVolume]
		if short == "" {
			short = strings.TrimPrefix(v.Name, project+"_")
		}
		if !nameRE.MatchString(short) {
			return nil, fmt.Errorf("volume %s has a name that cannot be used as a tag", v.Name)
		}
		if !filepath.IsAbs(v.Mountpoint) || filepath.Clean(v.Mountpoint) == "/" {
			return nil, fmt.Errorf("volume %s has no usable mountpoint", v.Name)
		}
		vols = append(vols, volume{Name: v.Name, Short: short, Mountpoint: filepath.Clean(v.Mountpoint)})
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Short < vols[j].Short })
	return vols, nil
}

// dumpArgs is the dump hook as a docker command. The hook's command line is
// one argument of `sh -c` inside the container; no shell runs on the host.
func dumpArgs(app App) []string {
	service := app.DumpService
	if app.DumpExecService != "" {
		service = app.DumpExecService
	}
	return []string{"compose", "-p", app.Project, "exec", "-T", service, "sh", "-c", app.DumpCommand}
}
