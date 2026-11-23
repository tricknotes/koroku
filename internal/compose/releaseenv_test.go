package compose

import (
	"slices"
	"strings"
	"testing"
)

func TestReleaseEnvServicesPlain(t *testing.T) {
	params := OverrideParams{
		Project:    loadProject(t, "rails-postgres"),
		WebService: "web", WebPort: 3000, HostPort: 20001,
		EnvFile:        "/var/lib/koroku/apps/blog/env",
		ReleaseEnvFile: "/var/lib/koroku/apps/blog/env.release",
		DefaultMemory:  "512m",
	}
	// The public service, the one-shot migration and the built worker read
	// the commit variables; the database does not.
	if got, want := params.ReleaseEnvServices(), []string{"migrate", "web", "worker"}; !slices.Equal(got, want) {
		t.Errorf("ReleaseEnvServices = %v, want %v", got, want)
	}
	out, err := Override(params)
	if err != nil {
		t.Fatal(err)
	}
	db := serviceBlock(string(out), "db")
	if strings.Contains(db, "env.release") || !strings.Contains(db, "/var/lib/koroku/apps/blog/env\"") {
		t.Errorf("db block:\n%s", db)
	}
	if web := serviceBlock(string(out), "web"); !strings.Contains(web, "env.release") {
		t.Errorf("web block:\n%s", web)
	}

	// A service that follows the commit reads it too.
	image := OverrideParams{
		Project:    loadProject(t, "image-only"),
		WebService: "web", WebPort: 80, HostPort: 20002,
		EnvFile:        "/var/lib/koroku/apps/static/env",
		ReleaseEnvFile: "/var/lib/koroku/apps/static/env.release",
		DefaultMemory:  "512m",
	}
	if got := image.ReleaseEnvServices(); !slices.Equal(got, []string{"web"}) {
		t.Errorf("image-only ReleaseEnvServices = %v", got)
	}
	image.CommitServices = []string{"cache"}
	if got := image.ReleaseEnvServices(); !slices.Equal(got, []string{"cache", "web"}) {
		t.Errorf("with a commit service: %v", got)
	}

	// Without a release env file nothing reads one.
	params.ReleaseEnvFile = ""
	if got := params.ReleaseEnvServices(); got != nil {
		t.Errorf("without a release env file: %v", got)
	}
}

// The same services read the release env file whether or not the public
// service is deployed blue/green, so that turning it on or off leaves the
// other services' configuration alone.
func TestReleaseEnvServicesSameWithBlueGreen(t *testing.T) {
	plain := blueGreenParams(loadProject(t, "rails-postgres"))
	plain.BlueGreen = nil
	bg := blueGreenParams(loadProject(t, "rails-postgres"))
	var withoutColors []string
	for _, name := range bg.ReleaseEnvServices() {
		if !isColored(name, "web") {
			withoutColors = append(withoutColors, name)
		}
	}
	if !slices.Equal(plain.ReleaseEnvServices(), withoutColors) {
		t.Errorf("plain %v, blue/green %v", plain.ReleaseEnvServices(), bg.ReleaseEnvServices())
	}
	plainOut, _ := Override(plain)
	bgOut, _ := Override(bg)
	for _, name := range []string{"db", "worker", "migrate"} {
		if a, b := serviceBlock(string(plainOut), name), serviceBlock(string(bgOut), name); a != b {
			t.Errorf("%s differs:\nplain\n%s\nblue/green\n%s", name, a, b)
		}
	}
}

func TestCommitServices(t *testing.T) {
	actual, err := ParseProject([]byte(`{"services": {
		"web":    {"build": {"context": "."}},
		"worker": {"image": "ghcr.io/octo/app:sha-aaaaaaa"},
		"sidecar": {"image": "example/reporter:1", "environment": {"KOROKU_COMMIT_SHA": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		"db":     {"image": "postgres:17", "environment": {"POSTGRES_DB": "app"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := ParseProject([]byte(`{"services": {
		"web":    {"build": {"context": "."}},
		"worker": {"image": "ghcr.io/octo/app:sha-0000000"},
		"sidecar": {"image": "example/reporter:1", "environment": {"KOROKU_COMMIT_SHA": "0000000000000000000000000000000000000000"}},
		"db":     {"image": "postgres:17", "environment": {"POSTGRES_DB": "app"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := CommitServices(actual, probe); !slices.Equal(got, []string{"sidecar", "worker"}) {
		t.Errorf("CommitServices = %v", got)
	}
}

// serviceBlock returns the lines of one service in a rendered override.
func serviceBlock(out, name string) string {
	start := strings.Index(out, "  \""+name+"\":\n")
	if start < 0 {
		return ""
	}
	rest := out[start+1:]
	end := strings.Index(rest, "\n  \"")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
