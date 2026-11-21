package logs

import (
	"os"
	"path/filepath"
)

// systemPluginDirs are where the docker CLI looks for plugins after the
// user's directory, in its order (docker/cli, cli-plugins/manager).
var systemPluginDirs = []string{
	"/usr/local/lib/docker/cli-plugins",
	"/usr/local/libexec/docker/cli-plugins",
	"/usr/lib/docker/cli-plugins",
	"/usr/libexec/docker/cli-plugins",
}

// FindComposePlugin returns the path of the Compose plugin binary the docker
// CLI would run for `docker compose`, or "" when there is none in the usual
// places. Running it directly saves the docker CLI process that would
// otherwise stay alive next to it for as long as a follow command runs.
func FindComposePlugin() string {
	config := os.Getenv("DOCKER_CONFIG")
	if config == "" {
		if home, err := os.UserHomeDir(); err == nil {
			config = filepath.Join(home, ".docker")
		}
	}
	dirs := systemPluginDirs
	if config != "" {
		dirs = append([]string{filepath.Join(config, "cli-plugins")}, systemPluginDirs...)
	}
	return findComposePlugin(dirs)
}

func findComposePlugin(dirs []string) string {
	for _, dir := range dirs {
		path := filepath.Join(dir, "docker-compose")
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}
